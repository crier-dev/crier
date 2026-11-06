package session

import (
	"context"
	"fmt"
	"time"
)

// ingress_route.go — the SQL spelling of the `ingress.route` receipt
// (CRIER-INGRESS §3): "where did this push actually land".
//
// The spec names the row abstractly: "a row in chat_messages with
// kind=ingress.route". This repo has no `chat_messages` table — the concrete
// transcript is `chat_transcript` — and the transcript CANNOT carry the row
// without a storage redesign, for two reasons that are both load-bearing today:
//
//   - `chat_transcript.session_id` is `NOT NULL REFERENCES chat_sessions(id)`.
//     An inbox push never touches a session (mode=inbox is precisely the
//     session-less mode, §4.2), so the row has no session id to project and a
//     synthetic one would invent a room that does not exist.
//   - `chat_transcript.message_kind` is CHECKed to `('plain','addressed',
//     'task')` (§4.4 — a reader must never guess a record's kind from text).
//     Adding `ingress.route` there is a CHECK constraint rebuild on every
//     existing database, on both engines, for a record that is not a message.
//
// So the route is projected into its own row in the SAME view and the SAME
// database the session store owns, carrying an explicit `kind` column whose
// one legal value is `ingress.route` — the property the spec's wording is
// asking for ("a row ... with kind=ingress.route"), without restructuring the
// transcript. The table is created by the schema statements both engines
// already apply, so no migration framework and no ALTER are introduced: a new
// table with CREATE TABLE IF NOT EXISTS reaches an existing database exactly
// the way chat_seq, chat_tasks and the group tables already do.
//
// The JSONL backend is untouched: its route log is ingress.JSONLRouteStore,
// written beside the session log root. Only a SQL session store answers
// IngressRouteWriter below, and that is what the server selects on.

// IngressRouteKind is the one legal value of chat_ingress_routes.kind. It is
// spelled exactly as §3 spells it, so an operator querying the view reads the
// same word the spec and the JSONL records use.
const IngressRouteKind = "ingress.route"

// IngressRoute is one `ingress.route` row: the ingress.RouteRecord fields,
// spelled in this package's vocabulary so the SQL stores can project it
// without importing internal/ingress (which imports this package).
type IngressRoute struct {
	// ID is the route record's own id (the handler mints it).
	ID string
	// AgentID is the RESOLVED recipient: the named agent, or the holder the
	// capability selector picked. Never empty.
	AgentID string
	// Capability is the selector the caller named, "" when the push addressed
	// the agent directly.
	Capability string
	// MessageID is the inbox entry id the delivery path returned — the id an
	// ack later names.
	MessageID string
	// Author is the caller as it was established on the ingress surface.
	Author AuthorRef
	// Tags are the normalized tags the entry was stored with (CRIER-TAGS
	// §2.4). Empty means untagged, stored as SQL NULL.
	Tags []string
	// CreatedAt is when the push landed.
	CreatedAt time.Time
}

// validate enforces the row's WRITE contract: a route record that cannot say
// where the push landed is refused rather than stored, because a receipt with
// no destination would claim a delivery that cannot be answered for.
func (r IngressRoute) validate() error {
	if r.ID == "" {
		return fmt.Errorf("%w: ingress.route without an id", ErrInvalidRecord)
	}
	if r.AgentID == "" {
		return fmt.Errorf("%w: ingress.route %q names no resolved agent", ErrInvalidRecord, r.ID)
	}
	if r.MessageID == "" {
		return fmt.Errorf("%w: ingress.route %q names no inbox entry id", ErrInvalidRecord, r.ID)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: ingress.route %q has no created_at", ErrInvalidRecord, r.ID)
	}
	return nil
}

// IngressRouteWriter records one ingress.route row. Both SQL stores implement
// it (the shared SQLStore that backs the sqlite AND postgres adapters, and the
// pgxpool-backed PostgresStore), which is what lets the server select the SQL
// route store by capability instead of by backend name. The JSONL store does
// NOT implement it: its route log is ingress's own file, not a table.
type IngressRouteWriter interface {
	RecordIngressRoute(ctx context.Context, rec IngressRoute) error
}

// ingressRouteStatements is the route table's DDL, parameterised by the
// engine's timestamp spelling so all three schema sets (sqlite TEXT, the
// shared store's canonical TEXT on BOTH engines, and the pgxpool store's
// native TIMESTAMPTZ) stay one body that cannot drift. `kind` is column one and
// CHECKed to the single legal value, so the view cannot hold a route row that
// misnames itself.
func ingressRouteStatements(createdAtType string) []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS chat_ingress_routes (
			kind         TEXT NOT NULL CHECK (kind = '` + IngressRouteKind + `'),
			route_id     TEXT PRIMARY KEY,
			agent_id     TEXT NOT NULL,
			capability   TEXT NOT NULL DEFAULT '',
			message_id   TEXT NOT NULL,
			author_type  TEXT NOT NULL DEFAULT '',
			author_id    TEXT NOT NULL DEFAULT '',
			principal_id TEXT NOT NULL DEFAULT '',
			tags         TEXT NULL,
			created_at   ` + createdAtType + ` NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS chat_ingress_routes_message_idx
			ON chat_ingress_routes (message_id)`,
		`CREATE INDEX IF NOT EXISTS chat_ingress_routes_agent_idx
			ON chat_ingress_routes (agent_id)`,
	}
}

// RecordIngressRoute implements IngressRouteWriter over the shared SQL store.
//
// A duplicate route_id is an ERROR, not a silent no-op: the id is minted per
// push (the handler does not re-record a replayed idempotency key), so a
// collision means two different receipts claim one id, and dropping the second
// would be the swallowed failure §3's error path exists to prevent.
func (s *SQLStore) RecordIngressRoute(ctx context.Context, rec IngressRoute) error {
	if err := rec.validate(); err != nil {
		return err
	}
	authorType, authorID, principalID := splitAuthor(&rec.Author)
	_, err := s.exec(ctx, `
		INSERT INTO chat_ingress_routes
			(kind, route_id, agent_id, capability, message_id,
			 author_type, author_id, principal_id, tags, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		IngressRouteKind, rec.ID, rec.AgentID, rec.Capability, rec.MessageID,
		authorType, authorID, principalID, encTags(rec.Tags), encTime(rec.CreatedAt))
	if err != nil {
		return fmt.Errorf("project ingress.route %q: %w", rec.ID, err)
	}
	return nil
}

// RecordIngressRoute implements IngressRouteWriter over the pgxpool-backed
// PostgreSQL store — the projector OpenStore selects for CR_SESSION_BACKEND=
// postgres. Same row, same column names, engine-native timestamp.
func (s *PostgresStore) RecordIngressRoute(ctx context.Context, rec IngressRoute) error {
	if err := rec.validate(); err != nil {
		return err
	}
	authorType, authorID, principalID := splitAuthor(&rec.Author)
	_, err := s.requestExecutor().Exec(ctx, `
		INSERT INTO chat_ingress_routes
			(kind, route_id, agent_id, capability, message_id,
			 author_type, author_id, principal_id, tags, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		IngressRouteKind, rec.ID, rec.AgentID, rec.Capability, rec.MessageID,
		authorType, authorID, principalID, encTags(rec.Tags), rec.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("project ingress.route %q: %w", rec.ID, err)
	}
	return nil
}
