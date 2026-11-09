package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

type rssCapture struct{ messages []ingress.InboxMessage }

func (c *rssCapture) Deliver(_ context.Context, _ string, _ string, m ingress.InboxMessage) ([]session.DeliveryOutcome, error) {
	c.messages = append(c.messages, m)
	return nil, nil
}
func TestRSSPollConditionalDedupRestartAndTrustMark(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "rss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	body := `<rss version="2.0"><channel><item><title>one</title><link>https://example.com/one</link><guid>guid-1</guid></item><item><title>two</title><guid>guid-2</guid></item></channel></rss>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "v1")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	cap := &rssCapture{}
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	p := &RSSPoller{Store: store, Inbox: cap, Now: func() time.Time { return now }, Client: server.Client(), MaxItemBytes: 4096}
	f := Feed{ID: "releases", URL: server.URL, IntervalSecs: 60, DeliverTo: "board-manager", Tags: []string{"rss"}}
	if err := p.PollOnce(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 2 {
		t.Fatalf("first poll got %d messages", len(cap.messages))
	}
	var firstPayload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(cap.messages[0].Payload, &firstPayload); err != nil || firstPayload.URL != "https://example.com/one" {
		t.Fatalf("first payload URL = %q, err=%v", firstPayload.URL, err)
	}
	for _, m := range cap.messages {
		if m.Author.System != "feed-releases" || !strings.Contains(strings.Join(m.Tags, ","), "untrusted:feed") {
			t.Fatalf("identity/trust marker missing: %#v", m)
		}
		if !strings.HasPrefix(m.IdempotencyKey, "rss:releases:") {
			t.Fatalf("idempotency key %q", m.IdempotencyKey)
		}
	}
	if err := p.PollOnce(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 2 {
		t.Fatalf("304 redelivered, total=%d", len(cap.messages))
	}
	body = `<rss version="2.0"><channel><item><title>one</title><link>https://example.com/one</link><guid>guid-1</guid></item><item><title>two</title><guid>guid-2</guid></item><item><title>three</title><link>https://example.com/three</link><guid>guid-3</guid></item></channel></rss>`
	// The fixture omits a 304 after its initial conditional response to simulate a changed feed.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "v2" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("If-Modified-Since") != "Mon, 02 Jan 2006 15:04:05 GMT" {
			t.Errorf("missing If-Modified-Since validator: %q", r.Header.Get("If-Modified-Since"))
		}
		w.Header().Set("ETag", "v2")
		_, _ = w.Write([]byte(body))
	})
	if err := p.PollOnce(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 3 {
		t.Fatalf("new guid delivered %d total messages", len(cap.messages))
	}
	state, err := store.LoadFeedState(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ETag != "v2" || len(state.Seen) != 3 {
		t.Fatalf("state not persisted: %#v", state)
	}
	health := p.Health()
	if health.ItemsSeen != 5 || health.DedupeHits != 2 || health.FeedStatus[f.ID] != "200" || health.LastPollAt.IsZero() {
		t.Fatalf("poll health = %#v", health)
	}
	_ = store.Close()
	reopened, err := OpenStore(filepath.Join(filepath.Dir(store.path), "rss.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	p.Store = reopened
	if err := p.PollOnce(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 3 {
		t.Fatalf("restart duplicated messages: %d", len(cap.messages))
	}
}
func TestRSSFailureBackoffAndReset(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	status := http.StatusInternalServerError
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusOK {
			_, _ = fmt.Fprint(w, `<rss><channel></channel></rss>`)
		} else {
			w.WriteHeader(status)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	p := &RSSPoller{Store: store, Client: server.Client(), Now: func() time.Time { return now }, Inbox: &rssCapture{}, Jitter: func(d time.Duration) time.Duration { return d }}
	f := Feed{ID: "failure", URL: server.URL, IntervalSecs: 60, DeliverTo: "agent"}
	for i, multiple := range []int{1, 2, 4} {
		if err := p.PollOnce(t.Context(), f); err == nil {
			t.Fatal("expected HTTP failure")
		}
		s, e := store.LoadFeedState(t.Context(), f.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := time.Duration(60*multiple) * time.Second
		if got := s.NextPoll.Sub(now); got != want {
			t.Fatalf("failure %d backoff=%s want %s", i+1, got, want)
		}
	}
	status = http.StatusOK
	if err := p.PollOnce(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	s, err := store.LoadFeedState(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConsecutiveFailures != 0 || s.NextPoll.Sub(now) != 60*time.Second {
		t.Fatalf("success did not reset backoff: %#v", s)
	}
}
func TestRSSSeenStateTTLAndSizeBounds(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	seen := map[string]time.Time{"expired": now.Add(-feedStateTTL - time.Second)}
	for i := 0; i < 1001; i++ {
		seen[fmt.Sprintf("id-%04d", i)] = now.Add(time.Duration(i) * time.Second)
	}
	pruneSeen(seen, now)
	if len(seen) != 1000 {
		t.Fatalf("seen set has %d ids, want 1000", len(seen))
	}
	if _, ok := seen["expired"]; ok {
		t.Fatal("expired guid remained in seen ledger")
	}
}

func TestRSSConfigValidationNamesFeedAndSSRF(t *testing.T) {
	cases := []struct {
		name, url string
		interval  int
		known     bool
		want      string
	}{{"bad-url", "http://example.com/feed", 60, true, "bad-url"}, {"slow", "https://example.com/feed", 59, true, "slow"}, {"unknown", "https://example.com/feed", 60, false, "unknown"}, {"loop", "https://127.0.0.1/feed", 60, true, "loop"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feeds.json")
			raw := fmt.Sprintf(`[{"id":%q,"url":%q,"interval_secs":%d,"deliver_to":"target"}]`, tc.name, tc.url, tc.interval)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFeeds(path, func(string) bool { return tc.known })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
	if _, err := LoadFeeds("", nil); err == nil || !strings.Contains(err.Error(), "CR_INGEST_RSS_CONFIG_FILE") {
		t.Fatalf("missing file error=%v", err)
	}
	client := NewSafeFeedClient()
	redirect, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1/feed", nil)
	if err := client.CheckRedirect(redirect, []*http.Request{{}}); err == nil {
		t.Fatal("redirect to loopback was accepted")
	}
	for _, address := range []string{"10.0.0.1", "169.254.169.254", "192.168.1.1", "::1", "fe80::1"} {
		if err := rejectUnsafeHost(address); err == nil {
			t.Errorf("unsafe address %s accepted", address)
		}
	}
}
