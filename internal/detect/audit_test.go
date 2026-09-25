package detect

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustOpen(t *testing.T, dir string) (*AuditLog, *Signer, string) {
	t.Helper()
	path := filepath.Join(dir, "delivery.jsonl")
	key, err := LoadOrCreateSigner(filepath.Join(dir, "delivery.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateSigner: %v", err)
	}
	log, err := OpenAuditLog(path, key)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, key, path
}

func appendRecords(t *testing.T, l *AuditLog, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(Entry{Kind: KindDelivery, Sender: "agent-a", Target: "agent-b", Verdict: "delivered"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

// TestAuditLogVerifiesAndSurvivesRestart is the deliverable's core property: an
// append-only log that a restart does not invalidate. The second Open is a NEW
// process image as far as the log is concerned — fresh file handle, fresh read —
// and it must (a) verify every record written before it, and (b) continue the
// sequence rather than restarting it.
func TestAuditLogVerifiesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	log, signer, path := mustOpen(t, dir)
	appendRecords(t, log, 3)

	rep := log.Verify()
	if !rep.OK || rep.Entries != 3 {
		t.Fatalf("Verify after 3 appends = %+v, want ok with 3 entries", rep)
	}
	if rep.KeyID != signer.KeyID() {
		t.Fatalf("Verify key_id = %q, want %q", rep.KeyID, signer.KeyID())
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open exactly as a restarted server would.
	key2, err := LoadOrCreateSigner(filepath.Join(dir, "delivery.key"))
	if err != nil {
		t.Fatalf("reload signer: %v", err)
	}
	if key2.KeyID() != signer.KeyID() {
		t.Fatalf("reloaded key id = %q, want %q (the key must be a FILE, not per-boot)", key2.KeyID(), signer.KeyID())
	}
	log2, err := OpenAuditLog(path, key2)
	if err != nil {
		t.Fatalf("re-open log: %v", err)
	}
	defer log2.Close()
	if got := log2.Total(); got != 3 {
		t.Fatalf("re-opened log total = %d, want 3 (records must survive a restart)", got)
	}
	if !log2.Verify().OK {
		t.Fatalf("re-opened log does not verify: %+v", log2.Verify())
	}

	// The chain CONTINUES: the next record is seq 4 and still verifies.
	e, err := log2.Append(Entry{Kind: KindAlert, Signal: SignalFanoutSpike, Sender: "agent-a"})
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if e.Seq != 4 {
		t.Fatalf("seq after restart = %d, want 4 (a restart must not restart the sequence)", e.Seq)
	}
	if e.PrevHash == genesisHash || e.PrevHash == "" {
		t.Fatalf("first record after restart names prev_hash %q, want the previous head", e.PrevHash)
	}
	rep = log2.Verify()
	if !rep.OK || rep.Entries != 4 {
		t.Fatalf("Verify after restart+append = %+v, want ok with 4 entries", rep)
	}
}

// TestAuditLogRefusesEditedRecord is the "signed, not just append-only" half:
// rewriting a record in place is DETECTED, both by a live Verify and by the
// next startup.
func TestAuditLogRefusesEditedRecord(t *testing.T) {
	dir := t.TempDir()
	log, _, path := mustOpen(t, dir)
	appendRecords(t, log, 3)
	_ = log.Close()

	lines := readLines(t, path)
	var rec Entry
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("decode line 2: %v", err)
	}
	rec.Verdict = "delivered-to-attacker" // the edit an intruder would make
	edited, _ := json.Marshal(rec)
	lines[1] = string(edited)
	writeLines(t, path, lines)

	key, _ := LoadOrCreateSigner(filepath.Join(dir, "delivery.key"))
	fresh, err := OpenAuditLog(path, key)
	if err == nil {
		fresh.Close()
		t.Fatal("OpenAuditLog accepted an EDITED record — a rewritten history must be refused, not appended to")
	}
	if !strings.Contains(err.Error(), "seq 2") || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("refusal = %v, want it to name seq 2 as edited", err)
	}
}

// TestAuditLogVerifySeesTamperingAfterOpen covers the case a startup check
// cannot: the log was verified at Open, and somebody edited the file WHILE the
// server was running. Verify re-reads the file, so the break is visible without
// a restart.
func TestAuditLogVerifySeesTamperingAfterOpen(t *testing.T) {
	dir := t.TempDir()
	log, _, path := mustOpen(t, dir)
	appendRecords(t, log, 3)

	if rep := log.Verify(); !rep.OK {
		t.Fatalf("pre-tamper Verify = %+v, want ok", rep)
	}

	lines := readLines(t, path)
	lines[2] = strings.Replace(lines[2], `"verdict":"delivered"`, `"verdict":"exfiltrated"`, 1)
	writeLines(t, path, lines)

	rep := log.Verify()
	if rep.OK {
		t.Fatal("Verify reported a tampered file as intact (it must re-read the file, not trust the in-memory window)")
	}
	if rep.FirstBadSeq != 3 {
		t.Fatalf("FirstBadSeq = %d, want 3 (the first record that no longer matches its signature)", rep.FirstBadSeq)
	}
	if rep.Error == "" {
		t.Fatal("Verify reported a failure with no reason")
	}
	t.Logf("tamper detected: %s", rep.Error)
}

// TestAuditLogVerifyReportsFirstBadSeqOnDeletion proves a DELETED record is
// caught too — the middle of the chain, not only the tail.
func TestAuditLogVerifyReportsFirstBadSeqOnDeletion(t *testing.T) {
	dir := t.TempDir()
	log, key, path := mustOpen(t, dir)
	appendRecords(t, log, 4)
	_ = log.Close()

	lines := readLines(t, path)
	writeLines(t, path, append(lines[:1], lines[2:]...)) // drop record 2

	fresh, err := OpenAuditLog(path, key)
	if err == nil {
		fresh.Close()
		t.Fatal("OpenAuditLog accepted a log with a DELETED record")
	}
	if !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("refusal = %v, want it to name the missing sequence number", err)
	}
}

// TestAuditLogRefusesForeignSignature proves the signature is what makes the
// log meaningful: a record appended by a DIFFERENT key does not verify under
// this server's key, even though its hash is internally consistent.
func TestAuditLogRefusesForeignSignature(t *testing.T) {
	dir := t.TempDir()
	log, _, path := mustOpen(t, dir)
	appendRecords(t, log, 2)
	_ = log.Close()

	// A second, unrelated signer forges a well-formed record.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	forger := signerFromSeed(seed)
	forged := Entry{
		Seq:       3,
		At:        "2026-09-25T00:00:00Z",
		Kind:      KindDelivery,
		Sender:    "attacker",
		PrevHash:  headOf(t, path),
		KeyID:     forger.id,
		Signature: "",
	}
	sum := sha256.Sum256(forged.canonical())
	forged.Hash = hex.EncodeToString(sum[:])
	forged.Signature = hex.EncodeToString(ed25519.Sign(forger.priv, []byte(forged.Hash)))
	raw, _ := json.Marshal(forged)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	key, _ := LoadOrCreateSigner(filepath.Join(dir, "delivery.key"))
	fresh, err := OpenAuditLog(path, key)
	if err == nil {
		fresh.Close()
		t.Fatal("OpenAuditLog accepted a record signed by a foreign key")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("refusal = %v, want a signature failure", err)
	}
}

// TestAuditLogEntriesWindowAndFilter covers the read API's accounting: the
// window is a window, and kind filtering narrows it without lying about the
// file's total.
func TestAuditLogEntriesWindowAndFilter(t *testing.T) {
	dir := t.TempDir()
	log, _, _ := mustOpen(t, dir)
	appendRecords(t, log, 5)
	if _, err := log.Append(Entry{Kind: KindAlert, Signal: SignalCanaryTrip, Sender: "agent-a"}); err != nil {
		t.Fatal(err)
	}

	if got := len(log.Entries(0, "")); got != 6 {
		t.Fatalf("Entries(0,\"\") = %d, want 6", got)
	}
	if got := len(log.Entries(2, "")); got != 2 {
		t.Fatalf("Entries(2,\"\") = %d, want the last 2", got)
	}
	alerts := log.Entries(0, KindAlert)
	if len(alerts) != 1 || alerts[0].Signal != SignalCanaryTrip {
		t.Fatalf("Entries(kind=alert) = %+v, want the one alert", alerts)
	}
	if got := log.Total(); got != 6 {
		t.Fatalf("Total = %d, want 6 (the file count, not the window)", got)
	}
}

func TestLoadOrCreateSignerRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("not-hex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(bad); err == nil {
		t.Fatal("LoadOrCreateSigner accepted a non-hex key file — regenerating would silently invalidate the existing log")
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("aabb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(short); err == nil {
		t.Fatal("LoadOrCreateSigner accepted a wrong-length seed")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func headOf(t *testing.T, path string) string {
	t.Helper()
	lines := readLines(t, path)
	var last Entry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	return last.Hash
}
