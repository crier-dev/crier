// Package detect implements crier's DETECTION layer (CR-FEAT-030): an
// append-only signed delivery log, behaviour baselines with alerts, a
// single-call containment (kill-switch) and canary tokens.
//
// The distinction this package exists for is ATTRIBUTION vs DETECTION. Crier
// could already say WHO delivered what, after the fact, from its ed25519
// attribution and its failure receipts. It could not say "this is happening
// NOW" while an agent was fanning out. Nothing here invents a new trust
// primitive: it wires the three the bus already holds — per-agent ed25519
// identity, the single delivery choke point, and the failure receipts — into
// signals an operator can act on.
//
// Everything is additive and OPT-IN (CR_DETECT_ENABLED): with the flag unset,
// the server registers no detection route and writes no file.
package detect

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Kind* are the record kinds the delivery log carries.
const (
	// KindDelivery is one delivery outcome: who sent what to whom, when, and
	// the verdict the bus reached.
	KindDelivery = "delivery"
	// KindAlert is one baseline signal tripping.
	KindAlert = "alert"
	// KindContainment is one kill-switch call and the actions it performed.
	KindContainment = "containment"
)

// genesisHash is the prev-hash of the first record in a log file. It is a
// constant so a truncated or restarted log is verifiable: the first record of
// a file must name it, never an arbitrary value.
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// maxInMemoryEntries bounds the in-memory window served by the read API. The
// FILE holds every record; the window is what GET /delivery-log answers from,
// and it is reported explicitly (TotalEntries vs Entries) so a reader never
// mistakes the window for the whole log.
const maxInMemoryEntries = 10000

// Entry is one append-only, signed delivery-log record.
//
// Hash covers every field except Hash and Signature; Signature is the ed25519
// signature over Hash alone. Because Hash covers PrevHash, the records form a
// hash chain: editing, reordering or deleting any record breaks verification
// from that point on, and no field can be re-signed without the private key.
type Entry struct {
	Seq       uint64 `json:"seq"`
	At        string `json:"at"`
	Kind      string `json:"kind"`
	Sender    string `json:"sender,omitempty"`
	Target    string `json:"target,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	Transport string `json:"transport,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Detail    string `json:"detail,omitempty"`
	PrevHash  string `json:"prev_hash"`
	Hash      string `json:"hash"`
	Signature string `json:"signature"`
	KeyID     string `json:"key_id"`
}

// canonical returns the bytes Hash is computed over and Signature signs: the
// record with its own Hash and Signature blanked, encoded deterministically
// (Go marshals struct fields in declaration order).
func (e Entry) canonical() []byte {
	e.Hash = ""
	e.Signature = ""
	raw, err := json.Marshal(e)
	if err != nil {
		// Unreachable: every field is a string, a uint64 or absent.
		panic(fmt.Sprintf("detect: canonical entry: %v", err))
	}
	return raw
}

// Signer is the ed25519 key the log is signed with. The key is a FILE
// (0600) so the log survives restarts and stays verifiable: a per-boot key
// would make every restart an unverifiable log.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   string
}

// KeyID is the hex prefix of the sha256 of the public key — the identifier
// every record carries.
func (s *Signer) KeyID() string { return s.id }

// PublicKey returns the verification key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// LoadOrCreateSigner loads the hex seed at path, creating it (0600) when
// absent. A file that is unreadable, not hex, or the wrong length is an error:
// silently regenerating a key would rewrite the meaning of every existing
// record.
func LoadOrCreateSigner(path string) (*Signer, error) {
	if path == "" {
		return nil, errors.New("detect: empty signing key path")
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		seed, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf("detect: signing key %s is not hex: %w", path, decErr)
		}
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("detect: signing key %s is %d bytes, want %d", path, len(seed), ed25519.SeedSize)
		}
		return signerFromSeed(seed), nil
	case os.IsNotExist(err):
		seed := make([]byte, ed25519.SeedSize)
		if _, err := io.ReadFull(rand.Reader, seed); err != nil {
			return nil, fmt.Errorf("detect: generate signing key: %w", err)
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("detect: create key dir %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("detect: write signing key %s: %w", path, err)
		}
		return signerFromSeed(seed), nil
	default:
		return nil, fmt.Errorf("detect: read signing key %s: %w", path, err)
	}
}

func signerFromSeed(seed []byte) *Signer {
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return &Signer{priv: priv, pub: pub, id: hex.EncodeToString(sum[:8])}
}

// AuditLog is the append-only signed delivery log. The file is opened
// O_APPEND and every record is fsynced before Append returns, so a record that
// was ACKed to its caller is on disk.
type AuditLog struct {
	path string
	key  *Signer

	mu      sync.Mutex
	f       *os.File
	seq     uint64
	head    string
	window  []Entry
	total   int
	lastErr error
}

// OpenAuditLog opens (creating if needed) the log at path and VERIFIES it.
//
// A file whose chain does not verify — an edited record, a deleted record, a
// line signed by another key, a truncated record — makes Open fail. That is
// deliberate: a detection log that silently accepts a rewritten history would
// be worse than none, and the operator finds out at startup rather than at
// review time.
func OpenAuditLog(path string, key *Signer) (*AuditLog, error) {
	if path == "" {
		return nil, errors.New("detect: empty audit log path")
	}
	if key == nil {
		return nil, errors.New("detect: nil signing key")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("detect: create log dir %s: %w", dir, err)
		}
	}
	// Read the existing file first (fail before opening for append, so a
	// tampered log is never appended to).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("detect: open log %s: %w", path, err)
	}
	l := &AuditLog{path: path, key: key, f: f, head: genesisHash}
	if err := l.replay(); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, fmt.Errorf("detect: seek log %s: %w", path, err)
	}
	return l, nil
}

// replay reads the whole file, verifying every record and rebuilding the
// tail window, the next sequence number and the chain head.
func (l *AuditLog) replay() error {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("detect: seek log %s: %w", l.path, err)
	}
	sc := bufio.NewScanner(l.f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	prev := genesisHash
	var seq uint64
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			return fmt.Errorf("detect: delivery log %s line %d is blank — a torn or hand-edited log is refused, never silently skipped", l.path, line)
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return fmt.Errorf("detect: delivery log %s line %d is not a record: %w", l.path, line, err)
		}
		if err := verifyRecord(e, l.key.pub, seq+1, prev); err != nil {
			return fmt.Errorf("detect: delivery log %s line %d: %w", l.path, line, err)
		}
		seq = e.Seq
		prev = e.Hash
		l.total++
		l.push(e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("detect: read delivery log %s: %w", l.path, err)
	}
	l.seq = seq
	l.head = prev
	return nil
}

// verifyRecord checks one record against the expected position, the expected
// prev-hash and the signing key.
func verifyRecord(e Entry, pub ed25519.PublicKey, wantSeq uint64, wantPrev string) error {
	if e.Seq != wantSeq {
		return fmt.Errorf("record seq %d, want %d (a record was deleted, reordered or duplicated)", e.Seq, wantSeq)
	}
	if e.PrevHash != wantPrev {
		return fmt.Errorf("record seq %d prev_hash %s, want %s (the chain is broken here)", e.Seq, short(e.PrevHash), short(wantPrev))
	}
	if e.KeyID == "" {
		return fmt.Errorf("record seq %d carries no key_id", e.Seq)
	}
	sum := sha256.Sum256(e.canonical())
	want := hex.EncodeToString(sum[:])
	if e.Hash != want {
		return fmt.Errorf("record seq %d hash %s, want %s (the record was edited)", e.Seq, short(e.Hash), short(want))
	}
	sig, err := hex.DecodeString(e.Signature)
	if err != nil {
		return fmt.Errorf("record seq %d signature is not hex: %w", e.Seq, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("record seq %d signature is %d bytes, want %d", e.Seq, len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, []byte(e.Hash), sig) {
		return fmt.Errorf("record seq %d signature does not verify against key %s (the record was not signed by this server)", e.Seq, e.KeyID)
	}
	return nil
}

// push appends to the in-memory window, trimming it to maxInMemoryEntries.
func (l *AuditLog) push(e Entry) {
	l.window = append(l.window, e)
	if len(l.window) > maxInMemoryEntries {
		l.window = l.window[len(l.window)-maxInMemoryEntries:]
	}
}

// Append signs and writes one record, then fsyncs it. The returned Entry is
// the record as it was written (seq, hash, signature filled in).
func (l *AuditLog) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return Entry{}, errors.New("detect: delivery log is closed")
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if e.Kind == "" {
		e.Kind = KindDelivery
	}
	e.Seq = l.seq + 1
	e.PrevHash = l.head
	e.KeyID = l.key.id
	sum := sha256.Sum256(e.canonical())
	e.Hash = hex.EncodeToString(sum[:])
	e.Signature = hex.EncodeToString(ed25519.Sign(l.key.priv, []byte(e.Hash)))

	raw, err := json.Marshal(e)
	if err != nil {
		return Entry{}, fmt.Errorf("detect: marshal record: %w", err)
	}
	if _, err := l.f.Write(append(raw, '\n')); err != nil {
		return Entry{}, fmt.Errorf("detect: write record: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return Entry{}, fmt.Errorf("detect: fsync record: %w", err)
	}
	l.seq = e.Seq
	l.head = e.Hash
	l.total++
	l.push(e)
	return e, nil
}

// Verify re-reads the FILE from disk and verifies the whole chain with the
// loaded key. Unlike the replay at Open, this sees a record appended or edited
// by another process after this log was opened.
func (l *AuditLog) Verify() LogReport {
	rep := LogReport{Path: l.path, KeyID: l.key.id}
	f, err := os.Open(l.path)
	if err != nil {
		rep.Error = fmt.Sprintf("open: %v", err)
		return rep
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	prev := genesisHash
	var seq uint64
	for sc.Scan() {
		rep.Entries++
		var e Entry
		if err := json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &e); err != nil {
			rep.Error = fmt.Sprintf("line %d: not a record: %v", rep.Entries, err)
			rep.FirstBadSeq = rep.Entries
			return rep
		}
		if err := verifyRecord(e, l.key.pub, seq+1, prev); err != nil {
			rep.Error = fmt.Sprintf("line %d: %v", rep.Entries, err)
			rep.FirstBadSeq = int(seq) + 1
			return rep
		}
		seq = e.Seq
		prev = e.Hash
	}
	if err := sc.Err(); err != nil {
		rep.Error = fmt.Sprintf("read: %v", err)
		return rep
	}
	rep.OK = true
	return rep
}

// LogReport is the answer to "is this log still what it was when it was
// written?".
type LogReport struct {
	Path        string `json:"path"`
	Entries     int    `json:"entries"`
	OK          bool   `json:"ok"`
	FirstBadSeq int    `json:"first_bad_seq,omitempty"`
	Error       string `json:"error,omitempty"`
	KeyID       string `json:"key_id"`
}

// Entries returns up to limit records from the in-memory window, newest last,
// optionally filtered by kind. A limit <= 0 means the whole window.
func (l *AuditLog) Entries(limit int, kind string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, 0, len(l.window))
	for _, e := range l.window {
		if kind != "" && e.Kind != kind {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Total returns the number of records in the FILE (as of the last replay or
// append), which is what makes the read window visibly a window.
func (l *AuditLog) Total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Path returns the file the log is written to.
func (l *AuditLog) Path() string { return l.path }

// KeyID names the key the records are signed with.
func (l *AuditLog) KeyID() string { return l.key.id }

// Close releases the file handle.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
