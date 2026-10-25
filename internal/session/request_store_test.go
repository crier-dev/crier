package session

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMessageRequestLockCancellation(t *testing.T) {
	t.Parallel()
	backends := append(localBackends(), localBackend{
		name: "sqlite_memory", pathFn: func(*testing.T) string { return ":memory:" },
		open: func(t *testing.T, path string) Repository {
			s, err := NewSQLiteStore(path)
			require.NoError(t, err)
			return s
		},
	})
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = s.Close() })
			release, err := s.LockMessage(context.Background(), "room", "key")
			require.NoError(t, err)
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			_, err = s.LockMessage(ctx, "room", "key")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			// Another key or session must remain independent of this lock.
			for _, pair := range [][2]string{{"room", "other"}, {"other", "key"}} {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				unlock, err := s.LockMessage(ctx, pair[0], pair[1])
				cancel()
				require.NoError(t, err)
				unlock()
			}
		})
	}
}

func TestSQLiteMessageRequestProjectionIsAtomic(t *testing.T) {
	t.Parallel()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "atomic.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	h := newAPIHarness(t, s)
	room := h.createRoom(t)
	// Fail AFTER transcript insertion, exactly at the additive key projection.
	_, err = s.DB().Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON chat_message_requests
		BEGIN SELECT RAISE(ABORT, 'request projection failure'); END`)
	require.NoError(t, err)
	req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "atomic"}
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, nil, nil)
	require.Equal(t, http.StatusInternalServerError, code)
	st, err := s.Load(context.Background(), room)
	require.NoError(t, err)
	require.Empty(t, st.Messages, "a failed key projection must roll back its message")
	_, err = s.DB().Exec(`DROP TRIGGER reject_request`)
	require.NoError(t, err)
	first := h.postTo(t, room, req)
	second := h.postTo(t, room, req)
	require.Equal(t, first.ID, second.ID)
	st, err = s.Load(context.Background(), room)
	require.NoError(t, err)
	require.Len(t, st.Messages, 1)
}

func TestMessageRequestIdempotencyRefusalDetails(t *testing.T) {
	t.Parallel()
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = s.Close() })
			h := newAPIHarness(t, s)
			room := h.createRoom(t)
			req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "refused",
				Targets: []AudienceTarget{{Kind: TargetAgent, ID: "missing"}}}
			first := h.postTo(t, room, req)
			require.NotEmpty(t, first.Outcomes)
			second := h.postTo(t, room, req)
			require.Equal(t, first.ID, second.ID)
			require.Equal(t, "redelivered", second.ReplayAction)
			require.Equal(t, first.Outcomes[0].Target, second.Outcomes[0].Target)
			require.Equal(t, first.Outcomes[0].Outcome, second.Outcomes[0].Outcome)
			require.Equal(t, first.Outcomes[0].Detail, second.Outcomes[0].Detail)
			require.False(t, second.Outcomes[0].UpdatedAt.Before(first.Outcomes[0].UpdatedAt))
			require.Equal(t, first.SkippedAgents, second.SkippedAgents)
		})
	}
}
