package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"
)

// This file pins the DF-CRIER-200 contract: PostgresStore must implement the
// ListErrorReporter capability (store.go, DF-CRIER-199) so a failing database
// is distinguishable from an empty registry at the MCP list_agents surface.
//
// Assertions go through the local listErrReporter structural type (defined in
// remote_list_error_test.go) rather than ListErrorReporter itself: a missing
// method is then a BEHAVIORAL failure that names the defect, not a
// package-wide collection error, and the RED run stays readable.

// listQueryRegex is the SELECT expectation for List, mirroring the statement
// in postgres_store.go List (agentConfigColumns + FROM agents).
const listQueryRegex = `SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard FROM agents`

// expectListRows stubs one SUCCESSFUL empty List query.
func expectListRows(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(listQueryRegex).
		WillReturnRows(pgxmock.NewRows(agentRowColumns()))
}

// expectListQueryError stubs one FAILING List query.
func expectListQueryError(mock pgxmock.PgxPoolIface, err error) {
	mock.ExpectQuery(listQueryRegex).WillReturnError(err)
}

// listFailureCase names one of List's failure exits and drives it through the
// mocked pool. The store's non-nil-empty-slice answer on failure is asserted
// in the shared runner below for every branch.
type listFailureCase struct {
	name string
	// drive installs the mock expectation(s) for one failing List call.
	drive func(mock pgxmock.PgxPoolIface)
	// wantIn names a substring the recorded ListError must carry, naming the
	// failing step (query, scan, iteration, or per-row decode).
	wantIn string
}

// postgresListFailureCases enumerates List's failure exits (postgres_store.go
// List): the query itself, the row scan, a corrupt stored public key, and the
// three per-row JSON decodes (capabilities, webhook, guard). The seventh
// exit — rows.Err — is covered separately: pgxmock rows expose no error
// injection (NewRows has no WithErr), so it is driven through a stubbed
// connPool in TestPostgresListError_RowsIterationError below.
func postgresListFailureCases(now time.Time) []listFailureCase {
	pub := make([]byte, 32)
	return []listFailureCase{
		{
			name: "query error",
			drive: func(mock pgxmock.PgxPoolIface) {
				expectListQueryError(mock, errors.New("connection refused"))
			},
			wantIn: "list",
		},
		{
			name: "row scan error",
			drive: func(mock pgxmock.PgxPoolIface) {
				// Eight values (pgxmock panics on a count mismatch at
				// AddRow), but registered_at is unscannable into a
				// time.Time: the scan of this row must fail.
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("scan-broken", make([]byte, 32), []byte(`[]`), "online", struct{}{}, now, nil, nil)
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
			},
			wantIn: "scan",
		},
		{
			name: "corrupt stored public key",
			drive: func(mock pgxmock.PgxPoolIface) {
				// A non-32-byte, non-empty key is corrupt data: the whole
				// listing is dropped (pre-existing behavior, unchanged).
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("corrupt-key", []byte("short"), []byte(`[]`), "online", now, now, nil, nil)
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
			},
			wantIn: "public key",
		},
		{
			name: "malformed capabilities JSON",
			drive: func(mock pgxmock.PgxPoolIface) {
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("bad-caps", pub, []byte(`{"not":"an array"}`), "online", now, now, nil, nil)
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
			},
			wantIn: "capabilities",
		},
		{
			name: "malformed webhook JSON",
			drive: func(mock pgxmock.PgxPoolIface) {
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("bad-webhook", pub, []byte(`[]`), "online", now, now, []byte(`{"url":`), nil)
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
			},
			wantIn: "webhook",
		},
		{
			name: "malformed guard JSON",
			drive: func(mock pgxmock.PgxPoolIface) {
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("bad-guard", pub, []byte(`[]`), "online", now, now, nil, []byte(`[1,2]`))
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
			},
			wantIn: "guard",
		},
	}
}

// runListFailureCase drives one failing List through the store and asserts
// the three-part contract: non-nil empty slice, capability present, and a
// contextual error naming the failing step plus postgres context.
func runListFailureCase(t *testing.T, tc listFailureCase) {
	t.Helper()
	s, mock := newMockStore(t)
	tc.drive(mock)

	got := s.List()
	if got == nil {
		t.Fatal("List() = nil slice, want non-nil empty slice per the Store contract")
	}
	if len(got) != 0 {
		t.Fatalf("List() = %d agents, want 0 on failure", len(got))
	}

	rep, ok := any(s).(listErrReporter)
	if !ok {
		t.Fatal("PostgresStore does not implement the ListErrorReporter capability — a failing database is indistinguishable from an empty registry at the MCP list_agents surface")
	}
	err := rep.ListError()
	if err == nil {
		t.Fatal("ListError() = nil after a failed List, want the recorded failure")
	}
	if !strings.Contains(err.Error(), tc.wantIn) {
		t.Errorf("ListError() = %q, want it to name the failing step (%q)", err.Error(), tc.wantIn)
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("ListError() = %q, want it to carry postgres context", err.Error())
	}
	if lerr := mock.ExpectationsWereMet(); lerr != nil {
		t.Errorf("expectations not met: %v", lerr)
	}
}

// TestPostgresListError_FailureBranchesRecordContextualError drives every
// failure exit of List through the pgxmock doubles: each must keep the
// spec-pinned contract (a NON-NIL empty slice, the existing log line) AND
// record a contextual error naming the failing step. Pre-fix (DF-CRIER-200),
// PostgresStore laundered every failure into a silent empty slice with
// nothing recorded.
func TestPostgresListError_FailureBranchesRecordContextualError(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range postgresListFailureCases(now) {
		t.Run(tc.name, func(t *testing.T) {
			runListFailureCase(t, tc)
		})
	}
}

// errRows is a pgx.Rows stub whose Err reports a mid-stream failure — it
// drives List's rows.Err() exit, which the pgxmock rows API cannot express
// (NewRows has no error injection).
type errRows struct {
	err error
}

func (r *errRows) Next() bool                    { return false }
func (r *errRows) Scan(dest ...any) error        { return errors.New("unreachable: Next is false") }
func (r *errRows) Err() error                    { return r.err }
func (r *errRows) Close()                        {}
func (r *errRows) CommandTag() pgconn.CommandTag { return pgconn.NewCommandTag("") }
func (r *errRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *errRows) Values() ([]any, error) { return nil, errors.New("unreachable: Next is false") }
func (r *errRows) RawValues() [][]byte    { return nil }
func (r *errRows) Raw() []byte            { return nil }
func (r *errRows) Conn() *pgx.Conn        { return nil }

// errRowsPool is a connPool whose every Query returns stubbed rows that fail
// on iteration — List's query succeeds, iteration yields nothing, and
// rows.Err() carries the failure.
type errRowsPool struct{ err error }

func (p *errRowsPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), p.err
}
func (p *errRowsPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return &errRows{err: p.err}, nil
}
func (p *errRowsPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return errRow{p.err}
}
func (p *errRowsPool) Begin(ctx context.Context) (pgx.Tx, error) { return nil, p.err }
func (p *errRowsPool) Ping(ctx context.Context) error            { return p.err }
func (p *errRowsPool) Close()                                    {}

// errRow satisfies pgx.Row for the interface above without pulling in more
// of pgx than List needs.
type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

// TestPostgresListError_RowsIterationError covers the seventh failure exit:
// the query succeeds, rows.Next() terminates without a row, and rows.Err()
// reports a mid-iteration failure (connection reset mid-stream). Pre-fix
// this laundered into a silent empty slice exactly like a query failure.
func TestPostgresListError_RowsIterationError(t *testing.T) {
	s := &PostgresStore{pool: &errRowsPool{err: errors.New("connection reset mid-stream")}}

	got := s.List()
	if got == nil || len(got) != 0 {
		t.Fatalf("List() = %+v, want non-nil empty slice on rows failure", got)
	}
	rep, ok := any(s).(listErrReporter)
	if !ok {
		t.Fatal("PostgresStore does not implement the ListErrorReporter capability")
	}
	err := rep.ListError()
	if err == nil {
		t.Fatal("ListError() = nil after a rows iteration failure, want the recorded failure")
	}
	if !strings.Contains(err.Error(), "connection reset mid-stream") {
		t.Errorf("ListError() = %q, want it to wrap the underlying failure", err.Error())
	}
}

// TestPostgresListError_RecoveredBySuccess pins the last-call semantics: a
// failed List records the error, and the NEXT successful List — empty or
// populated — clears it. ListError must describe the LAST call.
func TestPostgresListError_RecoveredBySuccess(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		// stubSuccess queues one successful List query and returns the
		// number of agents it answers with.
		stubSuccess func(mock pgxmock.PgxPoolIface) int
	}{
		{
			name: "successful empty",
			stubSuccess: func(mock pgxmock.PgxPoolIface) int {
				expectListRows(mock)
				return 0
			},
		},
		{
			name: "successful populated",
			stubSuccess: func(mock pgxmock.PgxPoolIface) int {
				rows := pgxmock.NewRows(agentRowColumns()).
					AddRow("a1", make([]byte, 32), []byte(`[]`), "online", now, now, nil, nil)
				mock.ExpectQuery(listQueryRegex).WillReturnRows(rows)
				return 1
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newMockStore(t)
			rep, ok := any(s).(listErrReporter)
			if !ok {
				t.Fatal("PostgresStore does not implement the ListErrorReporter capability")
			}

			// 1. Failing query: error recorded.
			expectListQueryError(mock, errors.New("connection refused"))
			if got := s.List(); got == nil || len(got) != 0 {
				t.Fatalf("failing List = %+v, want non-nil empty slice", got)
			}
			if err := rep.ListError(); err == nil {
				t.Fatal("ListError() = nil after the failed call, want the recorded failure")
			}

			// 2. Successful List (the subtest's shape): error cleared.
			want := tc.stubSuccess(mock)
			got := s.List()
			if got == nil {
				t.Fatal("List() = nil on success, want non-nil")
			}
			if len(got) != want {
				t.Fatalf("successful List = %d agents, want %d", len(got), want)
			}
			if err := rep.ListError(); err != nil {
				t.Fatalf("ListError() = %v after a successful List, want nil (last call succeeded)", err)
			}
			if lerr := mock.ExpectationsWereMet(); lerr != nil {
				t.Errorf("expectations not met: %v", lerr)
			}
		})
	}
}

// TestPostgresListError_EmptyRegistryIsClean pins the other side of the
// distinction on this backend: a HEALTHY database with zero agents is a
// successful call — same non-nil empty slice, but ListError stays nil. This
// is the assertion that keeps an over-eager fix from reporting errors on
// empty results.
func TestPostgresListError_EmptyRegistryIsClean(t *testing.T) {
	s, mock := newMockStore(t)
	expectListRows(mock)

	got := s.List()
	if got == nil || len(got) != 0 {
		t.Fatalf("List() = %+v, want non-nil empty slice for a healthy empty registry", got)
	}
	rep, ok := any(s).(listErrReporter)
	if !ok {
		t.Fatal("PostgresStore does not implement the ListErrorReporter capability")
	}
	if err := rep.ListError(); err != nil {
		t.Fatalf("ListError() = %v after a successful empty list, want nil — an empty registry is not a failure", err)
	}
	if lerr := mock.ExpectationsWereMet(); lerr != nil {
		t.Errorf("expectations not met: %v", lerr)
	}
}

// TestPostgresListError_ConcurrentListsSynchronized drives concurrent List
// and ListError callers through the race detector: the shared error field is
// read and written from many goroutines with no sleeps — every goroutine's
// work is real and complete before the barrier, so the race detector (not
// timing) enforces the mutex discipline.
func TestPostgresListError_ConcurrentListsSynchronized(t *testing.T) {
	s, mock := newMockStore(t)
	rep, ok := any(s).(listErrReporter)
	if !ok {
		t.Fatal("PostgresStore does not implement the ListErrorReporter capability")
	}

	// Alternate failing and succeeding expectations; the mock delivers them
	// in FIFO order, and each List goroutine consumes exactly one.
	const total = 8
	for i := 0; i < total; i++ {
		if i%2 == 0 {
			expectListQueryError(mock, errors.New("connection refused"))
		} else {
			expectListRows(mock)
		}
	}

	var listWG sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < total; i++ {
		listWG.Add(1)
		go func() {
			defer listWG.Done()
			<-start
			s.List()
		}()
	}
	// A concurrent ListError reader exercises the read side under the same
	// discipline while Lists run; it stops once every List has finished.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
				_ = rep.ListError()
			}
		}
	}()
	close(start)
	listWG.Wait() // every List consumed its expectation
	close(stop)
	readerWG.Wait()

	// Mock queues are FIFO, but goroutine START order is not: with 4
	// failing and 4 succeeding expectations the final call's outcome is not
	// deterministic, so assert the invariant that holds either way — the
	// recorded state is one of the two legal outcomes, never garbage.
	if err := rep.ListError(); err != nil && !strings.Contains(err.Error(), "postgres") {
		t.Errorf("ListError() = %v, want nil or a postgres-naming error", err)
	}
	if lerr := mock.ExpectationsWereMet(); lerr != nil {
		t.Errorf("expectations not met: %v", lerr)
	}
}
