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

func TestPublishSubscribe(t *testing.T) {
	r := New()
	ch, unsub := r.Subscribe("agent.status")
	defer unsub()

	event := json.RawMessage(`{"state":"online"}`)
	if err := r.Publish("agent.status", event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if string(got) != string(event) {
			t.Fatalf("got %s, want %s", got, event)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	r := New()
	ch1, unsub1 := r.Subscribe("agent.heartbeat")
	defer unsub1()
	ch2, unsub2 := r.Subscribe("agent.heartbeat")
	defer unsub2()

	event := json.RawMessage(`{"ts":1}`)
	if err := r.Publish("agent.heartbeat", event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case got := <-ch:
			if string(got) != string(event) {
				t.Fatalf("sub %d: got %s, want %s", i, got, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timeout", i)
		}
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	r := New()
	if err := r.Publish("agent.status", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Publish with no subscribers should be no-op, got: %v", err)
	}
}

func TestTopicsCounts(t *testing.T) {
	r := New()
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
	r := New()
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
	r := New()
	if err := r.Publish("", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for empty topic")
	}
	if err := r.Publish("bad topic", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for topic with space")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	r := New()
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
	rly := New()
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
	rly := New()
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
	rly := New()
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
	rly := New()
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
	rly := New()
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
	rly := New()
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
	if !strings.Contains(string(msg), `"hello":"world"`) {
		t.Fatalf("unexpected ws message: %s", msg)
	}
}
