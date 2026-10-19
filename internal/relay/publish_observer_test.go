package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishObserverBeforeFanoutAndAccept(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("a2a.task.test")
	defer unsub()
	calls := 0
	r.SetPublishObserver(func(realm, agent, topic string, event json.RawMessage) error {
		calls++
		require.Empty(t, realm)
		require.Equal(t, "owner", agent)
		require.Equal(t, "a2a.task.test", topic)
		require.JSONEq(t, `{"answer":"yes"}`, string(event))
		select {
		case <-ch:
			t.Fatal("fanout ran before durable observation")
		default:
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(`{"topic":"a2a.task.test","event":{"answer":"yes"}}`))
	req.Header.Set("X-Agent-ID", "owner")
	rec := httptest.NewRecorder()
	r.HandlePublish(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, 1, calls)
	select {
	case <-ch:
	default:
		t.Fatal("accepted publish was not fanned out")
	}
}

func TestPublishObserverErrorAndInvalidPublish(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("valid")
	defer unsub()
	calls := 0
	r.SetPublishObserver(func(_, _, _ string, _ json.RawMessage) error {
		calls++
		return errors.New("store unavailable")
	})
	for _, tc := range []struct {
		body          string
		status, calls int
	}{
		{`{"topic":"bad.*","event":{}}`, http.StatusBadRequest, 0},
		{`{"topic":"valid"}`, http.StatusBadRequest, 0},
		{`{"namespace":"bad","topic":"valid","event":{}}`, http.StatusBadRequest, 0},
		{`{"topic":"valid","event":{}}`, http.StatusInternalServerError, 1},
	} {
		rec := httptest.NewRecorder()
		r.HandlePublish(rec, httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(tc.body)))
		require.Equal(t, tc.status, rec.Code)
		require.Equal(t, tc.calls, calls)
		select {
		case <-ch:
			t.Fatal("rejected publish reached subscribers")
		default:
		}
	}
}
