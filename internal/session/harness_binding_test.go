package session

// CRIER-HARNESS §4.3: the harness binding lives on the Crier session
// record, so it must survive BOTH persistence shapes — the JSONL append log and
// the SQL view (SQLite here; PostgreSQL shares the projection code).

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func harnessTestSession(id string) *Session {
	return &Session{
		ID:               id,
		Namespace:        "crier",
		Kind:             KindChannel,
		Title:            "harness bound session",
		CreatedAt:        time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC),
		CreatedBy:        AuthorRef{Agent: "alpha"},
		State:            SessionOpen,
		Harness:          "opencode",
		HarnessSessionID: "ses_7f2c",
	}
}

func TestJSONL_HarnessBindingRoundTrips(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	ctx := context.Background()

	sess := harnessTestSession("s_harness_jsonl")
	require.NoError(t, store.CreateSession(ctx, sess, 1))

	// The create line carries the binding fields by name, so an operator can
	// read it in the log without the fold.
	records, err := store.Records(ctx, "s_harness_jsonl")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "opencode", records[0].Harness)
	require.Equal(t, "ses_7f2c", records[0].HarnessSessionID)

	loaded, err := store.Session(ctx, "s_harness_jsonl")
	require.NoError(t, err)
	require.Equal(t, "opencode", loaded.Harness)
	require.Equal(t, "ses_7f2c", loaded.HarnessSessionID)
}

func TestSQLite_HarnessBindingRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.sqlite")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, store.CreateSession(ctx, harnessTestSession("s_harness_sql"), 1))

	loaded, err := store.Session(ctx, "s_harness_sql")
	require.NoError(t, err)
	require.Equal(t, "opencode", loaded.Harness)
	require.Equal(t, "ses_7f2c", loaded.HarnessSessionID)
	require.NoError(t, store.Close())

	// A restart reads the same binding back from the view.
	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	again, err := reopened.Session(ctx, "s_harness_sql")
	require.NoError(t, err)
	require.Equal(t, "opencode", again.Harness)
	require.Equal(t, "ses_7f2c", again.HarnessSessionID)
}

func TestSQLite_StoreOnlySessionHasEmptyBinding(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	sess := harnessTestSession("s_store_only")
	sess.Harness = ""
	sess.HarnessSessionID = ""
	require.NoError(t, store.CreateSession(ctx, sess, 1))

	loaded, err := store.Session(ctx, "s_store_only")
	require.NoError(t, err)
	require.Empty(t, loaded.Harness, "a store-only session must not acquire a harness")
	require.Empty(t, loaded.HarnessSessionID)
}

// TestCreateSessionRecordsHarnessBinding is the create half of CRIER-HARNESS
// §4.3 on the HTTP surface: POST /sessions accepts `harness`, and the binding
// is on the SESSION (readable through the same API a client uses), not just in
// the request body. Without it nothing in the product could produce a bound
// session through a public route.
func TestCreateSessionRecordsHarnessBinding(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			var created sessionView
			code := h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:     "harness bound",
				CreatedBy: &AuthorRef{Principal: fixCreator},
				Harness:   "pi",
			}, &created, nil)
			require.Equal(t, http.StatusCreated, code)
			require.Equal(t, "pi", created.Harness, "the create response must echo the RECORDED binding")
			require.Empty(t, created.HarnessSessionID,
				"a create can never assert a harness-side id: none exists until an adapter answers")

			st, err := store.Load(context.Background(), created.ID)
			require.NoError(t, err)
			require.Equal(t, "pi", st.Session.Harness)
			require.Empty(t, st.Session.HarnessSessionID)

			// The list read is the surface a client actually consults, so the
			// binding has to survive the projection, not only the response.
			var list sessionsResponse
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions", nil, &list, nil))
			found := false
			for _, s := range list.Sessions {
				if s.ID == created.ID {
					found = true
					require.Equal(t, "pi", s.Harness, "the list drops the binding")
				}
			}
			require.True(t, found, "the created session is not in the list")
		})
	}
}

// TestCreateSessionWithoutHarnessStaysStoreOnly is the backward-compatibility
// half: the field is optional and an absent one changes nothing.
func TestCreateSessionWithoutHarnessStaysStoreOnly(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			var created sessionView
			code := h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:     "plain",
				CreatedBy: &AuthorRef{Principal: fixCreator},
			}, &created, nil)
			require.Equal(t, http.StatusCreated, code)
			require.Empty(t, created.Harness)

			st, err := store.Load(context.Background(), created.ID)
			require.NoError(t, err)
			require.Empty(t, st.Session.Harness)
			require.Empty(t, st.Session.HarnessSessionID)
		})
	}
}
