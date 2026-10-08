package permissions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Errors returned by the stores and by record validation.
var (
	// ErrInvalidRecord is returned for a log line that does not satisfy the
	// record shape (unknown version, unknown type, missing payload).
	ErrInvalidRecord = errors.New("invalid permission record")
	// ErrPrincipalNotFound / ErrAgentNotFound are returned by the lookup
	// helpers whose absence is meaningful to a caller.
	ErrPrincipalNotFound = errors.New("principal not found")
	ErrAgentNotFound     = errors.New("agent class record not found")
)

// RecordType is the closed set of permission log line types.
type RecordType string

const (
	// RecordPrincipal is a principal record-version (§2.1, §2.2): lifecycle
	// changes are NEW versions at the same id, never in-place edits.
	RecordPrincipal RecordType = "permission.principal"
	// RecordBinding is a binding record-version (§2.3).
	RecordBinding RecordType = "permission.binding"
	// RecordGrant is a grant record-version (§6.1): creating a grant appends
	// one, revoking it appends another at the same id with the tombstone set
	// (§6.6). Keep-LAST decides which wins; the older line is never removed.
	RecordGrant RecordType = "permission.grant"
	// RecordAgent is an agent-class record-version (§3.1): the class, owner
	// and scope reach the ACL reads. It is the class-lookup source until the
	// registry row grows `class` (see AgentInfo).
	RecordAgent RecordType = "permission.agent"
)

// ValidRecordType reports whether t is a member of the closed set.
func ValidRecordType(t RecordType) bool {
	switch t {
	case RecordPrincipal, RecordBinding, RecordGrant, RecordAgent:
		return true
	}
	return false
}

// Record is ONE line of the JSONL ordered append log — the transport form and
// the ordering authority, exactly as internal/session's record is. Fields are
// shared where the shape shares them (`v`, `type`, `id`, `ts`) and the payload
// is carried in exactly one type-specific member, so a line is self-describing
// and a reader dispatches on a version it knows.
type Record struct {
	V    int        `json:"v"`
	Type RecordType `json:"type"`
	ID   string     `json:"id"`
	TS   time.Time  `json:"ts"`

	Principal *Principal `json:"principal,omitempty"`
	Binding   *Binding   `json:"binding,omitempty"`
	Grant     *Grant     `json:"grant,omitempty"`
	Agent     *AgentInfo `json:"agent,omitempty"`
}

// Validate checks a record against the shape. It is the same check every store
// applies before a line is written or folded, so a malformed record is refused
// at the boundary rather than becoming a corrupt line.
func (r *Record) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidRecord)
	}
	if r.V != RecordFormatVersion {
		return fmt.Errorf("%w: unsupported record version %d (want %d)", ErrInvalidRecord, r.V, RecordFormatVersion)
	}
	if !ValidRecordType(r.Type) {
		return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, r.Type)
	}
	if r.ID == "" {
		return fmt.Errorf("%w: record without id", ErrInvalidRecord)
	}
	if r.TS.IsZero() {
		return fmt.Errorf("%w: record %q has a zero ts", ErrInvalidRecord, r.ID)
	}
	switch r.Type {
	case RecordPrincipal:
		if r.Principal == nil || r.Principal.ID != r.ID {
			return fmt.Errorf("%w: principal record %q does not carry a principal payload with the same id", ErrInvalidRecord, r.ID)
		}
		if !ValidPrincipalStatus(r.Principal.Status) {
			return fmt.Errorf("%w: principal %q has unknown status %q", ErrInvalidRecord, r.ID, r.Principal.Status)
		}
		if r.Principal.Role != "" && !ValidRole(r.Principal.Role) {
			return fmt.Errorf("%w: principal %q has unknown role %q", ErrInvalidRecord, r.ID, r.Principal.Role)
		}
	case RecordBinding:
		if r.Binding == nil || r.Binding.ID != r.ID {
			return fmt.Errorf("%w: binding record %q does not carry a binding payload with the same id", ErrInvalidRecord, r.ID)
		}
		if r.Binding.Principal == "" || r.Binding.Agent == "" {
			return fmt.Errorf("%w: binding %q without principal/agent", ErrInvalidRecord, r.ID)
		}
	case RecordGrant:
		if r.Grant == nil || r.Grant.ID != r.ID {
			return fmt.Errorf("%w: grant record %q does not carry a grant payload with the same id", ErrInvalidRecord, r.ID)
		}
		if err := r.Grant.Validate(); err != nil {
			return err
		}
	case RecordAgent:
		if r.Agent == nil || r.Agent.ID != r.ID {
			return fmt.Errorf("%w: agent record %q does not carry an agent payload with the same id", ErrInvalidRecord, r.ID)
		}
		if err := r.Agent.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// MarshalLine renders the record as ONE JSONL line (trailing newline
// included), the only byte form this package appends or imports.
func (r *Record) MarshalLine() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal permission record: %w", err)
	}
	return append(b, '\n'), nil
}

// ParseRecord parses one JSONL line into a Record and validates it. A caller
// that sees ErrInvalidRecord has an unknown or malformed line; the log is the
// log of record, so it is reported, never silently dropped.
func ParseRecord(line []byte) (*Record, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// NextTS is the monotonic convenience a store uses when it must mint a
// timestamp and the caller gave none.
func NextTS(prev, want time.Time) time.Time {
	if want.After(prev) {
		return want.UTC()
	}
	return prev.UTC()
}

// Store is the permission store contract, shared by the JSONL and PostgreSQL
// implementations exactly as internal/session's Store is shared by its two.
//
// Two properties are load-bearing:
//
//   - Append writes ONE record-version and is durable before it returns (an
//     fsync on the JSONL path, a committed upsert on the PostgreSQL path): a
//     grant is in force for the deployment when its record is in the store.
//   - Snapshot reflects every record appended BEFORE the call, including a
//     tombstone: the effective-permission computation is per request (§6.6),
//     so revocation takes effect on the NEXT delivery, not on a cached set.
type Store interface {
	// Append writes one record-version.
	Append(ctx context.Context, rec *Record) error
	// Snapshot folds the current store into the state the ACL reads.
	Snapshot(ctx context.Context) (*Snapshot, error)
	// Close releases the store's resources.
	Close() error
}

// Snapshot is the reduced state assembled from the record log by keep-LAST per
// (type, id): a later record-version at the same id wins, and the older line
// stays in the log as evidence (§6.6).
type Snapshot struct {
	principals map[string]*Principal
	bindings   map[string]*Binding
	grants     map[string]*Grant
	agents     map[string]*AgentInfo
}

// bindingKey is the lookup key of a binding: a principal's speech right onto a
// specific agent.
func bindingKey(principal, agent string) string { return principal + "\x00" + agent }

// Replay reduces an ordered record slice to a Snapshot, applying keep-LAST per
// (type, id). Identical duplicate lines are no-ops.
func Replay(recs []*Record) (*Snapshot, error) {
	s := &Snapshot{
		principals: map[string]*Principal{},
		bindings:   map[string]*Binding{},
		grants:     map[string]*Grant{},
		agents:     map[string]*AgentInfo{},
	}
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if err := rec.Validate(); err != nil {
			return nil, err
		}
		switch rec.Type {
		case RecordPrincipal:
			cp := *rec.Principal
			s.principals[rec.ID] = &cp
		case RecordBinding:
			cb := *rec.Binding
			s.bindings[bindingKey(cb.Principal, cb.Agent)] = &cb
		case RecordGrant:
			cg := *rec.Grant
			cg.Actions = append([]Action(nil), rec.Grant.Actions...)
			s.grants[rec.ID] = &cg
		case RecordAgent:
			ca := *rec.Agent
			ca.Capabilities = append([]string(nil), rec.Agent.Capabilities...)
			ca.Scopes = append([]Scope(nil), rec.Agent.Scopes...)
			s.agents[rec.ID] = &ca
		}
	}
	return s, nil
}

// Principal returns a principal by id, or nil.
func (s *Snapshot) Principal(id string) *Principal {
	if s == nil {
		return nil
	}
	return s.principals[id]
}

// Binding returns the binding of a principal onto an agent, or nil.
func (s *Snapshot) Binding(principal, agent string) *Binding {
	if s == nil {
		return nil
	}
	return s.bindings[bindingKey(principal, agent)]
}

// Agent returns an agent's class/owner/scope record, or nil (unclassed).
func (s *Snapshot) Agent(id string) *AgentInfo {
	if s == nil {
		return nil
	}
	return s.agents[id]
}

// Grants returns every grant record in the snapshot (including tombstoned
// ones), ordered by id — the audit view.
func (s *Snapshot) Grants() []*Grant {
	if s == nil {
		return nil
	}
	out := make([]*Grant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, g)
	}
	sortGrants(out)
	return out
}

// LiveGrantFor returns the first LIVE grant of a principal on a subject that
// carries an action at instant t, or nil. Expired and tombstoned grants are
// treated as absent (§6.1, §6.6).
func (s *Snapshot) LiveGrantFor(principal string, subject Subject, action Action, t time.Time) *Grant {
	if s == nil {
		return nil
	}
	var found *Grant
	for _, g := range s.grants {
		if g.Principal != principal || g.Subject != subject || !g.HasAction(action) || !g.Live(t) {
			continue
		}
		if found == nil || g.ID < found.ID {
			found = g
		}
	}
	return found
}

// GrantsFor returns every grant a principal holds on a subject, live or not,
// ordered by id — used by tests and by a future audit surface.
func (s *Snapshot) GrantsFor(principal string, subject Subject) []*Grant {
	if s == nil {
		return nil
	}
	var out []*Grant
	for _, g := range s.grants {
		if g.Principal == principal && g.Subject == subject {
			out = append(out, g)
		}
	}
	sortGrants(out)
	return out
}

func sortGrants(gs []*Grant) {
	for i := 1; i < len(gs); i++ {
		for j := i; j > 0 && gs[j-1].ID > gs[j].ID; j-- {
			gs[j-1], gs[j] = gs[j], gs[j-1]
		}
	}
}

// ---------------------------------------------------------------------------
// Record builders. One per type, so a caller composes a line through the domain
// object instead of hand-assembling the union struct.
// ---------------------------------------------------------------------------

// Record renders a principal record-version.
func (p *Principal) Record(ts time.Time) *Record {
	cp := *p
	if cp.Kind == "" {
		cp.Kind = "principal"
	}
	return &Record{V: RecordFormatVersion, Type: RecordPrincipal, ID: p.ID, TS: ts.UTC(), Principal: &cp}
}

// Record renders a binding record-version.
func (b *Binding) Record(ts time.Time) *Record {
	cb := *b
	if cb.Kind == "" {
		cb.Kind = "binding"
	}
	return &Record{V: RecordFormatVersion, Type: RecordBinding, ID: b.ID, TS: ts.UTC(), Binding: &cb}
}

// Record renders a grant record-version. A revocation is the SAME builder over
// a copy with the tombstone set (§6.6) — the record is versioned, never edited.
func (g *Grant) Record(ts time.Time) *Record {
	cg := *g
	if cg.Kind == "" {
		cg.Kind = "grant"
	}
	cg.Actions = append([]Action(nil), g.Actions...)
	return &Record{V: RecordFormatVersion, Type: RecordGrant, ID: g.ID, TS: ts.UTC(), Grant: &cg}
}

// Record renders an agent-class record-version.
func (a *AgentInfo) Record(ts time.Time) *Record {
	ca := *a
	ca.Capabilities = append([]string(nil), a.Capabilities...)
	ca.Scopes = append([]Scope(nil), a.Scopes...)
	return &Record{V: RecordFormatVersion, Type: RecordAgent, ID: a.ID, TS: ts.UTC(), Agent: &ca}
}

// ---------------------------------------------------------------------------
// Store helpers shared by both implementations.
// ---------------------------------------------------------------------------

// PutPrincipal appends a principal record-version.
func PutPrincipal(ctx context.Context, s Store, p *Principal, ts time.Time) error {
	return s.Append(ctx, p.Record(ts))
}

// PutBinding appends a binding record-version.
func PutBinding(ctx context.Context, s Store, b *Binding, ts time.Time) error {
	return s.Append(ctx, b.Record(ts))
}

// PutGrant appends a grant record-version.
func PutGrant(ctx context.Context, s Store, g *Grant, ts time.Time) error {
	return s.Append(ctx, g.Record(ts))
}

// PutAgentClass appends an agent-class record-version.
func PutAgentClass(ctx context.Context, s Store, a *AgentInfo, ts time.Time) error {
	return s.Append(ctx, a.Record(ts))
}

// RevokeGrant tombstones a live grant (§6.6): it reads the grant, copies it
// with revoked_at/revoked_by set and appends the new version. The original line
// stays in the log. A missing grant is ErrGrantNotFound.
func RevokeGrant(ctx context.Context, s Store, grantID, revokedBy string, ts time.Time) error {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	g, ok := snap.grants[grantID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrGrantNotFound, grantID)
	}
	cp := *g
	revokedAt := ts.UTC()
	cp.RevokedAt = &revokedAt
	cp.RevokedBy = revokedBy
	return s.Append(ctx, cp.Record(ts))
}

// ErrGrantNotFound is returned by RevokeGrant for an unknown id.
var ErrGrantNotFound = errors.New("grant not found")

// NewRecordID mints a record id with the spec's prefix shape
// (`prin_01J9Z6V0Q7`, §2.1): the named prefix plus 12 random bytes, hex —
// the same id discipline the registry's lease ids use. A record id is never
// guessable as "agent or human" by its shape (§2.1: kind is part of the
// record), and the prefix is what keeps the two vocabularies visibly apart.
func NewRecordID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// A crypto/rand failure cannot be retried into a weaker id: the
		// callers append records keyed by this id, so a fallback value
		// would risk collision. Panic, as the id-minting the registry
		// itself does on the same failure.
		panic("permissions: id generation failed: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}
