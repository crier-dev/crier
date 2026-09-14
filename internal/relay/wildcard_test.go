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

// Wildcard subscription semantics under test:
//
//   - "*" is one complete dot-separated segment and matches exactly one segment.
//   - ">" is one complete final segment and matches one or more trailing segments.
//   - Published topics stay literal: wildcards are subscriber-side only.
//   - A publish fans out exactly once to every matching exact/wildcard subscription.

const (
	recvTimeout = 500 * time.Millisecond
	dropTimeout = 150 * time.Millisecond
)

func mustReceive(t *testing.T, ch <-chan []byte, want string) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed while waiting for %s", want)
		}
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("timeout waiting for %s", want)
	}
}

func mustNotReceive(t *testing.T, ch <-chan []byte, context string) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed unexpectedly", context)
		}
		t.Fatalf("%s: unexpected delivery %s", context, got)
	case <-time.After(dropTimeout):
	}
}

// TestWildcardStarMatchesExactlyOneSegment covers demo.* : demo.one is
// delivered; demo.one.two (deeper), demo (shorter) and other.one (different
// prefix) are not.
func TestWildcardStarMatchesExactlyOneSegment(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("demo.*")
	defer unsub()

	if err := r.Publish("demo.one", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatalf("Publish demo.one: %v", err)
	}
	mustReceive(t, ch, `{"n":1}`)

	for _, topic := range []string{"demo", "demo.one.two", "other.one"} {
		if err := r.Publish(topic, json.RawMessage(`{"n":2}`)); err != nil {
			t.Fatalf("Publish %s: %v", topic, err)
		}
		mustNotReceive(t, ch, "demo.* must not receive "+topic)
	}
}

// TestWildcardTerminalGTMatchesTrailingSegments covers demo.> : one or more
// trailing segments match; the bare prefix does not.
func TestWildcardTerminalGTMatchesTrailingSegments(t *testing.T) {
	r := New(0)
	ch, unsub := r.Subscribe("demo.>")
	defer unsub()

	for _, topic := range []string{"demo.one", "demo.one.two"} {
		if err := r.Publish(topic, json.RawMessage(`{"n":3}`)); err != nil {
			t.Fatalf("Publish %s: %v", topic, err)
		}
		mustReceive(t, ch, `{"n":3}`)
	}

	for _, topic := range []string{"demo", "other.one"} {
		if err := r.Publish(topic, json.RawMessage(`{"n":4}`)); err != nil {
			t.Fatalf("Publish %s: %v", topic, err)
		}
		mustNotReceive(t, ch, "demo.> must not receive "+topic)
	}
}

// TestWildcardBareTokens pins the degenerate patterns: "*" matches any single
// segment, ">" matches every topic.
func TestWildcardBareTokens(t *testing.T) {
	r := New(0)
	star, unsubStar := r.Subscribe("*")
	defer unsubStar()
	gt, unsubGT := r.Subscribe(">")
	defer unsubGT()

	if err := r.Publish("one", json.RawMessage(`{"n":5}`)); err != nil {
		t.Fatalf("Publish one: %v", err)
	}
	mustReceive(t, star, `{"n":5}`)
	mustReceive(t, gt, `{"n":5}`)

	if err := r.Publish("one.two", json.RawMessage(`{"n":6}`)); err != nil {
		t.Fatalf("Publish one.two: %v", err)
	}
	mustNotReceive(t, star, `"*" must not match a two-segment topic`)
	mustReceive(t, gt, `{"n":6}`)
}

// TestPublishRejectsWildcardTopics: wildcards are subscriber-side only.
func TestPublishRejectsWildcardTopics(t *testing.T) {
	r := New(0)
	// A wildcard subscriber must never be reachable through a publish that
	// names the pattern itself.
	_, unsub := r.Subscribe("demo.*")
	defer unsub()

	for _, topic := range []string{"demo.*", "demo.>", "*", ">", "demo.*.one"} {
		if err := r.Publish(topic, json.RawMessage(`{"x":1}`)); err == nil {
			t.Errorf("Publish(%q) = nil error, want rejection", topic)
		}
	}
}

// TestPatternMatchingTable pins matching semantics without touching channels.
func TestPatternMatchingTable(t *testing.T) {
	cases := []struct {
		pattern string
		topic   string
		want    bool
	}{
		{"demo.*", "demo.one", true},
		{"demo.*", "demo.one.two", false},
		{"demo.*", "demo", false},
		{"demo.*", "other.one", false},
		{"demo.>", "demo.one", true},
		{"demo.>", "demo.one.two", true},
		{"demo.>", "demo.one.two.three", true},
		{"demo.>", "demo", false},
		{"demo.>", "other.one", false},
		{">", "anything", true},
		{">", "a.b.c", true},
		{"*", "one", true},
		{"*", "one.two", false},
		{"*.one", "demo.one", true},
		{"*.one", "demo.two", false},
		{"*.>", "demo.one", true},
		{"*.>", "demo", false},
		{"demo.*.one", "demo.x.one", true},
		{"demo.*.one", "demo.x.two", false},
		{"demo.*.one", "demo.x.y.one", false},
		{"demo.one", "demo.one", true},
		{"demo.one", "demo.one.two", false},
		{"a-b.c_d", "a-b.c_d", true},
		{"a-b.c_d", "a-b.c-e", false},
	}
	for _, tc := range cases {
		parsed, err := parseSubscriptionPattern(tc.pattern)
		if err != nil {
			t.Errorf("parseSubscriptionPattern(%q): %v", tc.pattern, err)
			continue
		}
		if got := parsed.matches(strings.Split(tc.topic, ".")); got != tc.want {
			t.Errorf("pattern %q vs topic %q = %v, want %v", tc.pattern, tc.topic, got, tc.want)
		}
	}
}

// TestSubscriptionPatternValidation: valid patterns register, invalid ones are
// refused at the Subscribe boundary (closed channel, no topic inventory entry).
func TestSubscriptionPatternValidation(t *testing.T) {
	valid := []string{
		"demo.one",
		"demo.*",
		"demo.>",
		"*",
		">",
		"*.one",
		"demo.*.one",
		"demo.*.>",
		"a-b.c_d",
		"demo.*.one.>",
	}
	invalid := []string{
		"",
		".demo",
		"demo.",
		"demo..one",
		"demo.>.one",
		"demo.>bar",
		"demo.>.*",
		"foo*bar",
		"demo.*x",
		"*demo",
		"demo.one*",
		"bad topic",
		"demo.one two",
		"demo.*.one >",
		"demo.😀",
		"demo.<>",
		strings.Repeat("a", 257),
	}

	r := New(0)
	for _, pattern := range valid {
		ch, unsub := r.Subscribe(pattern)
		select {
		case _, ok := <-ch:
			if !ok {
				t.Errorf("Subscribe(%q): channel closed for a valid pattern", pattern)
			}
		default:
		}
		unsub()
	}

	for _, pattern := range invalid {
		ch, unsub := r.Subscribe(pattern)
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("Subscribe(%q): expected closed channel for invalid pattern", pattern)
			}
		case <-time.After(recvTimeout):
			t.Errorf("Subscribe(%q): channel neither closed nor readable", pattern)
		}
		unsub()
	}
}

// TestPublishFansOutExactlyOnce: overlapping subscriptions each get one copy.
func TestPublishFansOutExactlyOnce(t *testing.T) {
	r := New(0)
	exact, unsubExact := r.Subscribe("demo.one")
	defer unsubExact()
	star, unsubStar := r.Subscribe("demo.*")
	defer unsubStar()
	gt, unsubGT := r.Subscribe("demo.>")
	defer unsubGT()

	if err := r.Publish("demo.one", json.RawMessage(`{"n":7}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	mustReceive(t, exact, `{"n":7}`)
	mustReceive(t, star, `{"n":7}`)
	mustReceive(t, gt, `{"n":7}`)

	// No duplicate fan-out.
	mustNotReceive(t, exact, "exact duplicate delivery")
	mustNotReceive(t, star, "wildcard duplicate delivery")
	mustNotReceive(t, gt, "terminal > duplicate delivery")
}

// TestWildcardUnsubscribeAndTopicInventory: wildcard subscriptions show up in
// the inventory under their pattern name and disappear on unsubscribe.
func TestWildcardUnsubscribeAndTopicInventory(t *testing.T) {
	r := New(0)
	_, unsubA := r.Subscribe("demo.*")
	_, unsubB := r.Subscribe("demo.*")
	_, unsubExact := r.Subscribe("demo.one")
	defer unsubExact()
	defer unsubA()
	defer unsubB()

	byName := map[string]int{}
	for _, ti := range r.Topics() {
		byName[ti.Name] = ti.Subscribers
	}
	if byName["demo.*"] != 2 {
		t.Fatalf("demo.* subscribers = %d, want 2: %+v", byName["demo.*"], r.Topics())
	}
	if byName["demo.one"] != 1 {
		t.Fatalf("demo.one subscribers = %d, want 1: %+v", byName["demo.one"], r.Topics())
	}

	unsubA()
	unsubB()

	for _, ti := range r.Topics() {
		if ti.Name == "demo.*" {
			t.Fatalf("demo.* still present after unsubscribe: %+v", r.Topics())
		}
	}

	// Unsubscribed wildcard is no longer routed to: re-subscribing is the only
	// way to receive again.
	ch, unsubC := r.Subscribe("demo.*")
	defer unsubC()
	if err := r.Publish("demo.two", json.RawMessage(`{"n":8}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	mustReceive(t, ch, `{"n":8}`)
}

// TestHandlePublishWildcardTopicBadRequest: POST /relay/publish with a
// wildcard topic is a client error, not an accepted broadcast.
func TestHandlePublishWildcardTopicBadRequest(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	for _, topic := range []string{"demo.*", "demo.>"} {
		req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(
			`{"topic":"`+topic+`","event":{"x":1}}`,
		))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("publish topic %q status = %d, want 400", topic, rr.Code)
		}
	}
}

// TestPublishTopicsAreExactlyLiteralPatterns pins the shared vocabulary of the
// two validators: a string is a legal PUBLISH topic if and only if it is a
// legal subscription pattern carrying no wildcard token. A pattern with a
// wildcard is subscriber-side only and must never be publishable.
func TestPublishTopicsAreExactlyLiteralPatterns(t *testing.T) {
	corpus := []string{
		"demo", "demo.one", "demo.one.two", "a-b.c_d", "A1.b2", "demo.1", "1.2.3",
		"", ".", "..", "demo.", ".demo", "demo..one", "demo. one", "bad topic",
		"demo.*", "demo.>", "*", ">", "*.one", "demo.*.one", "demo.*.>", "*.>",
		"foo*bar", "demo.*x", "*demo", "demo.one*", "demo.>bar", "demo.>.one", "demo.>.>",
		"demo.😀", "demo.<>", strings.Repeat("a", 256), strings.Repeat("a", 257),
	}
	for _, s := range corpus {
		publishable := validateTopic(s) == nil
		parsed, err := parseSubscriptionPattern(s)
		switch {
		case err != nil:
			if publishable {
				t.Errorf("validateTopic(%q) accepted a topic the subscription parser rejects", s)
			}
			continue
		case parsed.literal != publishable:
			t.Errorf("%q: pattern literal=%v but publishable=%v", s, parsed.literal, publishable)
			continue
		}
		if publishable && !parsed.matches(strings.Split(s, ".")) {
			t.Errorf("literal pattern %q does not match itself", s)
		}
	}
}

// TestHandleSubscribeInvalidPatternBadRequest: invalid patterns fail before
// the WebSocket upgrade with HTTP 400.
func TestHandleSubscribeInvalidPatternBadRequest(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)

	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, pattern := range []string{"demo..one", "demo.>.one", "foo*bar", "demo.>bar"} {
		resp, err := http.Get(srv.URL + "/relay/subscribe/" + pattern)
		if err != nil {
			t.Fatalf("GET %s: %v", pattern, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("subscribe %q status = %d, want 400", pattern, resp.StatusCode)
		}
	}

	if infos := rly.Topics(); len(infos) != 0 {
		t.Fatalf("invalid patterns registered topics: %+v", infos)
	}
}

// TestHandleSubscribeWildcardWebSocket: a real WebSocket wildcard subscriber
// upgrades and receives matching publishes exactly once.
func TestHandleSubscribeWildcardWebSocket(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	srv := httptest.NewServer(router)
	defer srv.Close()

	dial := func(pattern string) *websocket.Conn {
		t.Helper()
		url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay/subscribe/" + pattern
		conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %s: %v (status %v)", pattern, err, resp)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}

	publish := func(topic, event string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/relay/publish", strings.NewReader(
			`{"topic":"`+topic+`","event":`+event+`}`,
		))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("publish %s: %v", topic, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("publish %s status = %d, want 202", topic, resp.StatusCode)
		}
	}

	star := dial("demo.*")
	gt := dial("demo.>")
	// Give both subscriptions time to register.
	time.Sleep(50 * time.Millisecond)

	publish("demo.one", `{"n":9}`)

	for name, conn := range map[string]*websocket.Conn{"demo.*": star, "demo.>": gt} {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		if !strings.Contains(string(msg), `"n":9`) {
			t.Fatalf("%s message = %s", name, msg)
		}
	}

	// demo.* must not see a deeper topic; demo.> must.
	publish("demo.one.two", `{"n":10}`)

	_ = gt.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := gt.ReadMessage(); err != nil || !strings.Contains(string(msg), `"n":10`) {
		t.Fatalf("demo.> deeper read: msg=%s err=%v", msg, err)
	}
	_ = star.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, msg, err := star.ReadMessage(); err == nil {
		t.Fatalf("demo.* received a deeper topic it must not match: %s", msg)
	}
}

// TestConcurrentWildcardPublishSubscribe exercises wildcard routing under the
// race detector: every matching subscription receives every event exactly once
// and non-matching topics are never delivered.
func TestConcurrentWildcardPublishSubscribe(t *testing.T) {
	r := New(0)
	const (
		nStarSubs = 10
		nGTSubs   = 5
		nExact    = 3
		nPubs     = 10
	)

	var wg sync.WaitGroup
	total := nStarSubs + nGTSubs + nExact
	// counts is PRE-ALLOCATED and never appended to: the reader goroutines
	// below write one element each while the main goroutine keeps registering
	// further subscriptions, so growing the slice (append reassigns the slice
	// header) would race with those readers.
	counts := make([]int, total)
	unsubs := make([]func(), 0, total)
	next := 0

	subscribe := func(pattern string) {
		ch, unsub := r.Subscribe(pattern)
		unsubs = append(unsubs, unsub)
		idx := next
		next++
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
				counts[idx]++
			}
		}()
	}
	for i := 0; i < nStarSubs; i++ {
		subscribe("race.*")
	}
	for i := 0; i < nGTSubs; i++ {
		subscribe("race.>")
	}
	for i := 0; i < nExact; i++ {
		subscribe("race.one")
	}

	var pubWG sync.WaitGroup
	for i := 0; i < nPubs; i++ {
		pubWG.Add(2)
		go func() {
			defer pubWG.Done()
			if err := r.Publish("race.one", json.RawMessage(`{"n":11}`)); err != nil {
				t.Errorf("Publish race.one: %v", err)
			}
		}()
		go func() {
			defer pubWG.Done()
			// Deeper topic: matches race.> only.
			if err := r.Publish("race.one.two", json.RawMessage(`{"n":12}`)); err != nil {
				t.Errorf("Publish race.one.two: %v", err)
			}
		}()
	}
	pubWG.Wait()

	time.Sleep(200 * time.Millisecond)
	for _, unsub := range unsubs {
		unsub()
	}
	wg.Wait()

	starWant, gtWant, exactWant := nPubs, 2*nPubs, nPubs
	for i := 0; i < nStarSubs; i++ {
		if counts[i] != starWant {
			t.Errorf("race.* subscriber %d received %d, want %d", i, counts[i], starWant)
		}
	}
	for i := nStarSubs; i < nStarSubs+nGTSubs; i++ {
		if counts[i] != gtWant {
			t.Errorf("race.> subscriber %d received %d, want %d", i, counts[i], gtWant)
		}
	}
	for i := nStarSubs + nGTSubs; i < len(counts); i++ {
		if counts[i] != exactWant {
			t.Errorf("race.one subscriber %d received %d, want %d", i, counts[i], exactWant)
		}
	}
}
