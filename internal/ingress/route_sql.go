package ingress

// route_sql.go — the SQL spelling of the ingress.route receipt (CRIER-INGRESS
// §3), behind the SAME RouteStore seam the handler already depends on.
//
// The handler never learns which store it got: route.go's JSONLRouteStore and
// this SQLRouteStore both answer RecordRoute, so mode=inbox records the route
// the same way on every backend and a failure from either surfaces as the
// ErrStoreError that names the inbox entry id. cmd/server selects between them
// at boot: a SQL session store projects a row into its own view, anything else
// keeps the JSONL log, whose bytes are unchanged by this row.
//
// The row lives in the session store's SQL view (session.IngressRouteWriter).
// See internal/session/ingress_route.go for why the spec's abstract
// `chat_messages.kind=ingress.route` is projected as the store's own
// chat_ingress_routes row rather than a rewritten transcript.

import (
	"context"
	"fmt"

	"github.com/crier-dev/crier/internal/session"
)

// SQLRouteStore records route records in the session store's SQL view.
type SQLRouteStore struct {
	writer session.IngressRouteWriter
}

// NewSQLRouteStore wraps the SQL session store that will hold the rows. A nil
// writer is a boot error: a route store that cannot write is exactly the state
// the ingress surface refuses to run in.
func NewSQLRouteStore(writer session.IngressRouteWriter) (*SQLRouteStore, error) {
	if writer == nil {
		return nil, fmt.Errorf("ingress route store: no SQL session store is wired")
	}
	return &SQLRouteStore{writer: writer}, nil
}

// RecordRoute implements RouteStore. The mapping is explicit rather than a
// field copy: the two types live in different packages for a reason (the
// session store must not import the ingress handler), and a silent field
// rename on either side has to be a compile error, not a zero value.
func (s *SQLRouteStore) RecordRoute(ctx context.Context, rec RouteRecord) error {
	if err := s.writer.RecordIngressRoute(ctx, session.IngressRoute{
		ID:         rec.ID,
		AgentID:    rec.AgentID,
		Capability: rec.Capability,
		MessageID:  rec.MessageID,
		Author:     rec.Author,
		Tags:       rec.Tags,
		CreatedAt:  rec.CreatedAt,
	}); err != nil {
		return fmt.Errorf("ingress route record: %w", err)
	}
	return nil
}
