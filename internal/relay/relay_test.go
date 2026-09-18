package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

func BenchmarkPublish(b *testing.B) {
	const N = 1000

	r := New(0)
	unsubscribes := make([]func(), 0, N)
	for i := 0; i < N; i++ {
		_, unsubscribe := r.Subscribe("bench.topic")
		unsubscribes = append(unsubscribes, unsubscribe)
	}
	b.Cleanup(func() {
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	})
	event := json.RawMessage(`{"event":"benchmark"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Publish("bench.topic", event); err != nil {
			b.Fatalf("Publish: %v", err)
		}
	}
}

func BenchmarkSubscribe(b *testing.B) {
	r := New(0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, unsubscribe := r.Subscribe("bench.topic")
		unsubscribe()
	}
}

func TestPublishSubscribe(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("agent.status")
	defer unsub()

	event := json.RawMessage(`{"state":"online"}`)
	if err := r.Publish("agent.status", event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := wantFrame("agent.status", `{"state":"online"}`)
	select {
	case got := <-ch:
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	r := New(0)
	ch1, unsub1 := r.Subscribe("agent.heartbeat")
	defer unsub1()
	ch2, unsub2 := r.Subscribe("agent.heartbeat")
	defer unsub2()

	event := json.RawMessage(`{"ts":1}`)
	if err := r.Publish("agent.heartbeat", event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := wantFrame("agent.heartbeat", `{"ts":1}`)
	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case got := <-ch:
			if string(got) != want {
				t.Fatalf("sub %d: got %s, want %s", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timeout", i)
		}
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	r := New(0)
	if err := r.Publish("agent.status", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Publish with no subscribers should be no-op, got: %v", err)
	}
}

func TestTopicsCounts(t *testing.T) {
	r := New(0)
	_, unsub1 := r.Subscribe("a.one")
	defer unsub1()
	_, unsub2 := r.Subscribe("a.one")
	defer unsub2()
	_, unsub3 := r.Subscribe("b.two")
	defer unsub3()

	infos := r.Topics()
	if len(infos) != 2 {
		t.Fatalf("Topics len = %d, want 2: %+v", len(infos), infos)
	}

	byName := map[string]int{}
	for _, ti := range infos {
		byName[ti.Name] = ti.Subscribers
	}
	if byName["a.one"] != 2 {
		t.Errorf("a.one subscribers = %d, want 2", byName["a.one"])
	}
	if byName["b.two"] != 1 {
		t.Errorf("b.two subscribers = %d, want 1", byName["b.two"])
	}
}

func TestUnsubscribe(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("agent.status")

	infos := r.Topics()
	if len(infos) != 1 || infos[0].Subscribers != 1 {
		t.Fatalf("before unsub: %+v", infos)
	}

	unsub()

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel after unsub")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for channel close")
	}

	if infos := r.Topics(); len(infos) != 0 {
		t.Fatalf("after unsub Topics = %+v, want empty", infos)
	}

	// Publish after unsub should not panic / hang.
	if err := r.Publish("agent.status", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatalf("Publish after unsub: %v", err)
	}
}

func TestInvalidTopic(t *testing.T) {
	r := New(0)
	if err := r.Publish("", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for empty topic")
	}
	if err := r.Publish("bad topic", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for topic with space")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	r := New(0)
	const (
		nSubs = 20
		nPubs = 50
	)

	var wg sync.WaitGroup
	received := make([]int, nSubs)
	unsubs := make([]func(), nSubs)

	for i := 0; i < nSubs; i++ {
		ch, unsub := r.Subscribe("race.topic")
		unsubs[i] = unsub
		wg.Add(1)
		go func(idx int, c <-chan []byte) {
			defer wg.Done()
			for range c {
				received[idx]++
			}
		}(i, ch)
	}

	var pubWG sync.WaitGroup
	for i := 0; i < nPubs; i++ {
		pubWG.Add(1)
		go func(i int) {
			defer pubWG.Done()
			payload, _ := json.Marshal(map[string]int{"n": i})
			if err := r.Publish("race.topic", payload); err != nil {
				t.Errorf("Publish: %v", err)
			}
		}(i)
	}
	pubWG.Wait()

	// Allow delivery, then unsubscribe (closes channels, ends receivers).
	time.Sleep(100 * time.Millisecond)
	for _, u := range unsubs {
		u()
	}
	wg.Wait()

	// With buffered non-blocking send we expect most deliveries; at least one each.
	for i, n := range received {
		if n == 0 {
			t.Errorf("subscriber %d received 0 events", i)
		}
	}
}

func TestHandlePublishAndTopics(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")
	router.HandleFunc("/relay/topics", rly.HandleTopics).Methods("GET")

	// Subscribe so topics list is non-empty after publish path is exercised.
	_, unsub := rly.Subscribe("agent.status")
	defer unsub()

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
		`{"topic":"agent.status","event":{"ok":true}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202", rr.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/relay/topics", nil)
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("topics status = %d", rr2.Code)
	}
	var body topicsResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode topics: %v body=%s", err, rr2.Body.String())
	}
	if len(body.Topics) != 1 || body.Topics[0].Name != "agent.status" || body.Topics[0].Subscribers != 1 {
		t.Fatalf("topics body = %+v", body)
	}
}

func TestHandlePublishInvalidJSON(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", rr.Code)
	}
}

func TestHandlePublishEmptyTopic(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
		`{"topic":"","event":{}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty topic status = %d, want 400", rr.Code)
	}
}

func TestHandlePublishEmptyEvent(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
		`{"topic":"agent.status"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty event status = %d, want 400", rr.Code)
	}
}

func TestHandlePublishInvalidTopic(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
		`{"topic":"bad topic","event":{"x":1}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid topic status = %d, want 400", rr.Code)
	}
}

func TestHandleSubscribeWebSocket(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay/subscribe/agent.status"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Give subscribe time to register.
	time.Sleep(50 * time.Millisecond)

	pubReq, err := http.NewRequest(http.MethodPost, srv.URL+"/relay/publish", strings.NewReader(
		`{"topic":"agent.status","event":{"hello":"world"}}`,
	))
	if err != nil {
		t.Fatalf("new publish request: %v", err)
	}
	pubReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("publish status = %d", resp.StatusCode)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws: %v", err)
	}
	if want := wantFrame("agent.status", `{"hello":"world"}`); string(msg) != want {
		t.Fatalf("ws message = %s, want %s", msg, want)
	}
}

// ---------- Rate Limiter Tests ----------

func TestRateLimiterAllows(t *testing.T) {
	rl := NewRateLimiter(time.Minute)
	key := "agent-1"

	// 5 requests within the limit of 10 should all pass.
	for i := 0; i < 5; i++ {
		if !rl.Allow(key, 10, time.Minute) {
			t.Fatalf("request %d should be allowed", i)
		}
	}
}

func TestRateLimiterBlocks(t *testing.T) {
	rl := NewRateLimiter(time.Minute)
	key := "agent-1"

	// First 3 requests within limit of 3 should pass.
	for i := 0; i < 3; i++ {
		if !rl.Allow(key, 3, time.Minute) {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// 4th request should be blocked.
	if rl.Allow(key, 3, time.Minute) {
		t.Fatal("4th request should be rate limited")
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	rl := NewRateLimiter(50 * time.Millisecond)
	key := "agent-1"

	// Use up the limit.
	for i := 0; i < 3; i++ {
		if !rl.Allow(key, 3, time.Minute) {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// Should be blocked now.
	if rl.Allow(key, 3, time.Minute) {
		t.Fatal("should be rate limited after hitting limit")
	}

	// Wait for cleanup to run (cleanup interval is 50ms, window is 2× that = 100ms).
	// But the Allow window is 1 minute, so cleanup won't remove entries within the window.
	// This test verifies cleanup doesn't panic or corrupt state.
	time.Sleep(200 * time.Millisecond)

	// Still blocked because the window is 1 minute.
	if rl.Allow(key, 3, time.Minute) {
		t.Fatal("should still be rate limited within the window")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := NewRateLimiter(time.Minute)
	key := "agent-1"

	// limit=0 disables rate limiting — all requests pass.
	for i := 0; i < 1000; i++ {
		if !rl.Allow(key, 0, time.Minute) {
			t.Fatalf("request %d should be allowed when limit=0", i)
		}
	}
}

func TestRateLimiterPerKey(t *testing.T) {
	rl := NewRateLimiter(time.Minute)

	// Agent 1 hits the limit.
	for i := 0; i < 3; i++ {
		if !rl.Allow("agent-1", 3, time.Minute) {
			t.Fatalf("agent-1 request %d should be allowed", i)
		}
	}
	if rl.Allow("agent-1", 3, time.Minute) {
		t.Fatal("agent-1 should be rate limited")
	}

	// Agent 2 should still be allowed.
	if !rl.Allow("agent-2", 3, time.Minute) {
		t.Fatal("agent-2 should be allowed (different key)")
	}
}

func TestHandlePublishRateLimited(t *testing.T) {
	// Create a relay with rate limiting enabled (limit=3 per minute).
	rly := New(3)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	makeReq := func(agentID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
			`{"topic":"agent.status","event":{"ok":true}}`,
		))
		req.Header.Set("Content-Type", "application/json")
		if agentID != "" {
			req.Header.Set("X-Agent-ID", agentID)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// First 3 requests from agent-1 should succeed (202).
	for i := 0; i < 3; i++ {
		rr := makeReq("agent-1")
		if rr.Code != http.StatusAccepted {
			t.Fatalf("request %d: got %d, want 202", i, rr.Code)
		}
	}

	// 4th request from agent-1 should be rate limited (429).
	rr := makeReq("agent-1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited request: got %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rate limit exceeded") {
		t.Fatalf("rate limit body: got %q, want 'rate limit exceeded'", rr.Body.String())
	}

	// Agent-2 should still be allowed.
	rr2 := makeReq("agent-2")
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("agent-2: got %d, want 202", rr2.Code)
	}
}

func TestHandlePublishRateLimitDisabled(t *testing.T) {
	// Rate limiting disabled (limit=0): no X-Agent-ID header required,
	// all requests pass.
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	// Many requests should all pass.
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
			`{"topic":"agent.status","event":{"n":1}}`,
		))
		req.Header.Set("Content-Type", "application/json")
		// No X-Agent-ID header — identity is only required when limiting is on.
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("request %d: got %d, want 202 (rate limiting disabled)", i, rr.Code)
		}
	}
}

func TestHandlePublishRequiresAgentID(t *testing.T) {
	// Rate limiting enabled: publishing without X-Agent-ID is rejected with 401
	// (no RemoteAddr fallback — identity is mandatory when limiting is on).
	rly := New(1)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
		`{"topic":"agent.status","event":{"ok":true}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	// No X-Agent-ID header.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing X-Agent-ID: got %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "X-Agent-ID header required") {
		t.Fatalf("body: got %q, want 'X-Agent-ID header required'", rr.Body.String())
	}
}

func TestHandlePublishRateLimitDefaultHundred(t *testing.T) {
	// Default limit: 100 events/minute per agent (same identity).
	rly := New(100)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
			`{"topic":"agent.status","event":{"ok":true}}`,
		))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", "agent-1")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for i := 0; i < 100; i++ {
		if rr := makeReq(); rr.Code != http.StatusAccepted {
			t.Fatalf("request %d: got %d, want 202", i, rr.Code)
		}
	}

	// 101st publish from the same agent within the window is rate limited.
	rr := makeReq()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("101st request: got %d, want 429", rr.Code)
	}
}
