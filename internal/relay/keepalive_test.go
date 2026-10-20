package relay

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// startSubscribeServer stands up a relay with the subscribe route mounted and
// its silence bound scaled to `wait`, so the reaper has to actually fire during
// the test instead of waiting out the 60s default.
func startSubscribeServer(t *testing.T, wait time.Duration) (*Relay, *httptest.Server, string) {
	t.Helper()
	rly := New(0)
	rly.SetPongWait(wait)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay/subscribe/agent.status"
	return rly, srv, wsURL
}

// waitCount polls SubscriberCount until it equals want or the budget passes.
func waitCount(t *testing.T, rly *Relay, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for rly.SubscriberCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber count = %d, want %d after %v",
				rly.SubscriberCount(), want, budget)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSubscribeSilentSubscriberReaped proves QA-CRIER-35: a subscriber that
// connects and then goes silent — never pongs, never sends a close frame, the
// socket left half-open — is eventually cut by the read deadline and its
// subscription unregistered. Before the fix this loop had no deadline at all,
// so a half-open subscriber stayed in r.subs (and its 64-slot event channel)
// forever and subscriber counts drifted.
//
// The client deliberately does NOT answer pings and does NOT close: that is
// exactly the laptop-sleep / NAT-drop shape, where no TCP FIN is ever sent.
func TestSubscribeSilentSubscriberReaped(t *testing.T) {
	const pongWait = 150 * time.Millisecond
	rly, _, wsURL := startSubscribeServer(t, pongWait)

	// Deliberately drop the connection handle: no pong handler, no read loop,
	// no Close. The client mimes a gone-silent peer — closing the raw socket
	// would send a FIN, which is the one thing the defect shape lacks.
	if _, _, err := websocket.DefaultDialer.Dial(wsURL, nil); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// FIRST the subscription must actually register. Without this pin the
	// cell is vacuous: the first count sample could observe 0 before the
	// server ever added the subscriber, "passing" without reaping anything.
	waitCount(t, rly, 1, pongWait)

	// THEN the silence bound must reap it. Well past the deadline
	// (wait = 150ms, ping period = 75ms), bounded so the test stays
	// -short-safe: worst case ~1.6s of polling, never the 60s default.
	waitCount(t, rly, 0, 10*pongWait)

	// The reap is not just a count drop: it carries a reason, and for a peer
	// that never answered anything that reason is the silence bound itself
	// (QA-CRIER-43). This pins the diagnosis, not only the outcome.
	rec, ok := rly.LastReap()
	if !ok {
		t.Fatalf("silent subscriber was unregistered without a reap record — the reaper cannot say why it cut the subscriber")
	}
	if rec.Reason != ReapSilence {
		t.Fatalf("silent subscriber reap reason = %q, want %q (record: %s)", rec.Reason, ReapSilence, rec)
	}
	if rec.PongsSeen != 0 {
		t.Fatalf("silent subscriber recorded %d pong(s), want 0 — the silent peer answered something (record: %s)", rec.PongsSeen, rec)
	}
}

// Keepalive control tuning (QA-CRIER-43). The control used to pin pongWait at
// 120ms and sleep 8 windows: a fixed wall-clock budget indistinguishable from a
// regression, because a relay read pump descheduled for longer than 120ms on a
// contended host reaps a subscriber that answered every ping. The budget is now
// derived from measurements taken on THIS host, and every reap is reported with
// the reaper's own reason and liveness counters.
const (
	// controlFloorPongWait is the silence bound this cell has always pinned:
	// short enough that the reaper fires inside a test, never the 60s
	// production default.
	controlFloorPongWait = 120 * time.Millisecond
	// stallSamples × stallPeriod is the scheduling probe: 50 sleeps of 2ms
	// cost ~100ms on an idle host and expose how late a goroutine is woken
	// when the host is busy. That lateness is exactly the time a descheduled
	// read pump can leave a pong unread in the socket buffer.
	stallSamples = 50
	stallPeriod  = 2 * time.Millisecond
	// stallHeadroom multiplies the measured stall into the initial silence
	// bound: 8× the worst wake lateness measured, on top of the floor.
	stallHeadroom = 8
	// controlMaxPongWait caps the scaled bound, and controlWindowBudget caps
	// each attempt (a scaled bound must not eat the package timeout: make test
	// runs the suite with -timeout 60s). controlMaxAttempts bounds the
	// calibration: the path itself may redden once and be re-derived once.
	controlMaxPongWait  = 2 * time.Second
	controlWindowBudget = 8 * time.Second
	controlMinWindows   = 4
	controlMaxWindows   = 8
	controlMaxAttempts  = 2
	controlPollFloor    = 2 * time.Millisecond
	controlPollCeiling  = 20 * time.Millisecond
	// Non-vacuity floors: survival proves nothing unless the relay actually
	// pinged and the client actually answered.
	controlMinPingsSeen     = 2
	controlMinPongsAnswered = 1
)

// pingPath counts what the client observed on the wire: pings the relay sent,
// pongs the client answered, and the largest gap between two pings. The relay
// can say why its own read pump stopped; only the client can say what it put on
// the wire, and the pair is what tells a starved host from a broken renewal.
type pingPath struct {
	mu     sync.Mutex
	pings  int
	pongs  int
	last   time.Time
	maxGap time.Duration
}

// observePing records one inbound ping and the interval since the previous one.
func (p *pingPath) observePing() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.last.IsZero() {
		if gap := now.Sub(p.last); gap > p.maxGap {
			p.maxGap = gap
		}
	}
	p.last = now
	p.pings++
}

// observePong records one pong the client successfully wrote.
func (p *pingPath) observePong() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pongs++
}

// snapshot returns the counters and the largest observed ping interval.
func (p *pingPath) snapshot() (pings, pongs int, maxGap time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pings, p.pongs, p.maxGap
}

// measureWakeStall returns the largest overshoot observed when waking from a
// short sleep: the wall-clock quantity that decides whether a silence bound is
// meaningful on this host. A descheduled read pump cannot process a pong any
// sooner than this, so a bound shorter than it reaps a peer that answered.
func measureWakeStall(samples int, period time.Duration) time.Duration {
	var worst time.Duration
	for i := 0; i < samples; i++ {
		start := time.Now()
		time.Sleep(period)
		if late := time.Since(start) - period; late > worst {
			worst = late
		}
	}
	return worst
}

// scaleSilenceWindow derives the initial silence bound from the measured wake
// stall: the original floor plus stallHeadroom × the worst lateness this host
// was measured to produce. A quiet host keeps the historical 120ms, so the cell
// still exercises the reaper on its own terms; a busy host gets the headroom its
// own measurement asked for. capped reports that the ceiling bound the result.
func scaleSilenceWindow(stall time.Duration) (pongWait time.Duration, capped bool) {
	if stall < 0 {
		stall = 0
	}
	pongWait = controlFloorPongWait + stallHeadroom*stall
	if pongWait > controlMaxPongWait {
		return controlMaxPongWait, true
	}
	return pongWait, false
}

// controlWindows returns how many silence windows fit in one attempt's
// wall-clock budget. A quiet host gets the historical 8; a host that forced a
// large silence bound gets fewer, but never fewer than controlMinWindows, so the
// cell always proves the subscriber outlived SEVERAL windows, not one.
func controlWindows(pongWait time.Duration) int {
	if pongWait <= 0 {
		return controlMaxWindows
	}
	windows := int(controlWindowBudget / pongWait)
	if windows > controlMaxWindows {
		windows = controlMaxWindows
	}
	if windows < controlMinWindows {
		windows = controlMinWindows
	}
	return windows
}

// controlAttempt is one measured run of the control cell.
type controlAttempt struct {
	pongWait time.Duration
	windows  int
	count    int
	survived bool
	pings    int
	pongs    int
	maxGap   time.Duration
	elapsed  time.Duration
	reap     KeepaliveReap
	hasReap  bool
}

// runControlAttempt dials a subscriber that answers every ping, holds it for the
// window, and reports what happened — including the relay's own reap record when
// the subscriber disappeared, so the caller can explain it rather than count it.
func runControlAttempt(t *testing.T, pongWait time.Duration, windows int) controlAttempt {
	t.Helper()
	window := time.Duration(windows) * pongWait
	rly, _, wsURL := startSubscribeServer(t, pongWait)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	path := &pingPath{}
	conn.SetPingHandler(func(appData string) error {
		path.observePing()
		if err := conn.WriteControl(websocket.PongMessage, []byte(appData),
			time.Now().Add(time.Second)); err != nil {
			return err
		}
		path.observePong()
		return nil
	})
	// Drain inbound frames so control-frame reads keep flowing; reads also
	// drive the pong dispatch above. The deadline covers the whole window plus
	// slack, so a slow host cannot kill the client mid-measurement.
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(window + 5*time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// FIRST the subscription must actually register, the same pin the silent
	// cell uses. Without it the survival poll below could exit on the
	// pre-registration zero and report a "reap" that never happened.
	waitCount(t, rly, 1, window)

	// THEN poll across the whole window instead of sleeping blind, so a reap is
	// stamped with WHEN it happened, not only that it did.
	poll := pongWait / 8
	if poll < controlPollFloor {
		poll = controlPollFloor
	}
	if poll > controlPollCeiling {
		poll = controlPollCeiling
	}
	start := time.Now()
	for rly.SubscriberCount() == 1 && time.Since(start) < window {
		time.Sleep(poll)
	}

	res := controlAttempt{
		pongWait: pongWait,
		windows:  windows,
		count:    rly.SubscriberCount(),
		elapsed:  time.Since(start),
	}
	res.pings, res.pongs, res.maxGap = path.snapshot()
	res.survived = res.count == 1
	res.reap, res.hasReap = rly.LastReap()
	return res
}

// unpumpedReap reports whether the reap record says the read pump processed NO
// pong at all — the only silence shape a busy host produces (the pump never
// ran, or it never got far enough to see a ping go out). A silence reap that
// DID process pongs is the product-shaped shape: the deadline renewal itself did
// not keep up, and calibrating that away would hide a real regression. That is
// why the split is on pongs PROCESSED, not on the client's count.
func unpumpedReap(res controlAttempt) bool {
	return res.hasReap && res.reap.Reason == ReapSilence && res.reap.PongsSeen == 0
}

// reapReport renders the relay's own account of why the subscriber went away,
// plus the verdict that account supports. It never hides a reap: the caller
// still fails on anything it cannot attribute to the host.
func reapReport(res controlAttempt) string {
	if !res.hasReap {
		return "relay recorded no reap — the subscription left the relay without its read pump ending (not a silence reap)"
	}
	var verdict string
	switch {
	case res.reap.Reason == ReapSilence && res.reap.PingsSent > 0 && res.reap.PongsSeen == 0 && res.pongs > 0:
		verdict = "the relay processed NONE of the pongs this client sent before the deadline: its read pump never " +
			"ran, so the wall-clock window was tighter than this host's scheduling — host contention, not a product regression"
	case res.reap.Reason == ReapSilence && res.reap.PongsSeen > 0:
		verdict = "the read pump DID process pongs and still hit the deadline: the deadline renewal itself did not " +
			"keep up — product-shaped, needs a product fix (this is deliberately NOT calibrated away)"
	case res.reap.Reason == ReapSilence:
		verdict = "no pong reached the read pump before the deadline (the relay sent " +
			fmt.Sprintf("%d ping(s)", res.reap.PingsSent) + "): a scheduling artefact, not a product regression"
	case res.reap.Reason == ReapPeerClose:
		verdict = "the peer hung up (close frame or EOF): the client side of this cell died, not the keepalive path"
	default:
		verdict = "the socket failed for a non-silence reason, so the silence bound was never the deciding factor"
	}
	return fmt.Sprintf("%s — %s (windows=%d, deadline per window=%v)", res.reap, verdict, res.windows, res.pongWait)
}

// oneMinuteLoad returns the host's 1-minute loadavg, or "?" when unavailable. It
// is recorded with every measurement so a red cell can be read against the load
// that produced it — the same figure scripts/load-repro.sh gates on.
func oneMinuteLoad() string {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "?"
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "?"
	}
	return fields[0]
}

// TestSubscribePongingSubscriberSurvives is the control: a live subscriber
// that answers every ping is still registered after many silence windows have
// passed. This is the cell that fails if anyone "fixes" the leak by refusing
// to keep subscribers alive at all.
//
// QA-CRIER-43: the window is MEASURED, not pinned. A fixed pongWait is a
// wall-clock budget a busy host can beat without any product defect — the read
// pump is descheduled, pongs sit unread in the socket buffer, the deadline
// expires and a subscriber that answered every ping is reaped. So the cell
// measures this host's worst wake stall, scales the silence bound from it,
// prints the scaling, and calibrates once against the path itself: a first
// attempt reaped with "the relay processed no pong" is re-derived from that
// measurement and retried, with both attempts logged. A silence reap where the
// pump DID process pongs is never retried — that is product-shaped and fails
// immediately.
//
// The assertion that a ponging subscriber survives is unchanged — it is
// strengthened, not weakened: the cell also fails if no ping was ever exchanged,
// so survival can never be vacuous.
func TestSubscribePongingSubscriberSurvives(t *testing.T) {
	stall := measureWakeStall(stallSamples, stallPeriod)
	pongWait, capped := scaleSilenceWindow(stall)
	windows := controlWindows(pongWait)
	capNote := ""
	if capped {
		capNote = " (capped at " + controlMaxPongWait.String() + ")"
	}
	t.Logf("keepalive control: measured wake stall %v over %d sleeps of %v (1m loadavg %s) → "+
		"pongWait %v = floor %v + %d×stall%s; pingPeriod %v; %d silence windows = %v",
		stall, stallSamples, stallPeriod, oneMinuteLoad(),
		pongWait, controlFloorPongWait, stallHeadroom, capNote, pongWait/2, windows, time.Duration(windows)*pongWait)

	for attempt := 1; attempt <= controlMaxAttempts; attempt++ {
		res := runControlAttempt(t, pongWait, windows)
		if res.survived {
			// Non-vacuity: surviving proves nothing if the relay never pinged.
			// The design pings twice per window (pingPeriod = pongWait/2), so
			// these floors are far below what a working path produces.
			if res.pings < controlMinPingsSeen {
				t.Fatalf("keepalive control is vacuous: relay sent %d ping(s) in %v (largest gap %v), want at least %d — "+
					"the silence bound was never exercised, so survival proves nothing (pongWait %v, %d windows, attempt %d)",
					res.pings, res.elapsed, res.maxGap, controlMinPingsSeen, pongWait, windows, attempt)
			}
			if res.pongs < controlMinPongsAnswered {
				t.Fatalf("keepalive control is vacuous: client answered %d of %d ping(s), want at least %d — "+
					"the pong path under test never ran (pongWait %v, %d windows, attempt %d)",
					res.pongs, res.pings, controlMinPongsAnswered, pongWait, windows, attempt)
			}
			t.Logf("keepalive control: survived %d windows of %v (pingPeriod %v) — %d pings, %d pongs, "+
				"largest ping gap %v, attempt %d/%d",
				windows, pongWait, pongWait/2, res.pings, res.pongs, res.maxGap, attempt, controlMaxAttempts)
			return
		}

		report := reapReport(res)
		if unpumpedReap(res) && attempt < controlMaxAttempts && !capped {
			// The only fact the failed attempt establishes is a LOWER bound:
			// this host stalled the read pump for longer than the derived
			// window. It cannot say by how much, so the retry uses the ceiling
			// instead of a guess one doubling away. The retry still asserts
			// survival across several windows with pings and pongs counted.
			next := controlMaxPongWait
			t.Logf("keepalive control: attempt %d/%d was reaped at %v (%.1f windows in) — %s; "+
				"re-deriving the silence bound from that measurement: %v is below this host's stall, "+
				"retrying at the ceiling %v",
				attempt, controlMaxAttempts, res.elapsed, float64(res.elapsed)/float64(pongWait),
				report, pongWait, next)
			capped = true
			pongWait = next
			windows = controlWindows(pongWait)
			continue
		}

		t.Fatalf("ponging subscriber was reaped: count = %d, want 1 after %d windows of %v "+
			"(reaped at %v, %.1f windows in, attempt %d/%d); client saw %d pings (largest ping gap %v) "+
			"and answered %d; relay reaper: %s",
			res.count, res.windows, pongWait, res.elapsed, float64(res.elapsed)/float64(pongWait),
			attempt, controlMaxAttempts, res.pings, res.maxGap, res.pongs, report)
	}
}

// TestSubscribePongWaitDefault pins the configured default: a fresh relay has
// the production silence bound, and SetPongWait(0) restores it rather than
// disabling the bound.
func TestSubscribePongWaitDefault(t *testing.T) {
	rly := New(0)
	if got := rly.PongWait(); got != defaultPongWait {
		t.Fatalf("default PongWait = %v, want %v", got, defaultPongWait)
	}
	rly.SetPongWait(time.Second)
	if got := rly.PongWait(); got != time.Second {
		t.Fatalf("after SetPongWait: PongWait = %v, want 1s", got)
	}
	rly.SetPongWait(0)
	if got := rly.PongWait(); got != defaultPongWait {
		t.Fatalf("zero override must restore default: PongWait = %v, want %v", got, defaultPongWait)
	}
}

// timeoutError is a net.Error whose Timeout() reports true, i.e. the shape a
// read deadline produces.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestKeepaliveReapClassification pins the reaper's verdict: the reason a
// subscriber was cut must be named from the error that ended the read pump, not
// guessed from a count. It is the verdict the control cell quotes, so a
// misclassification would make the explanation lie.
func TestKeepaliveReapClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ReapReason
	}{
		{"read deadline", timeoutError{}, ReapSilence},
		{"wrapped read deadline", fmt.Errorf("read pump: %w", timeoutError{}), ReapSilence},
		{"close frame", &websocket.CloseError{Code: websocket.CloseNormalClosure}, ReapPeerClose},
		{"clean eof", io.EOF, ReapPeerClose},
		{"unexpected eof", io.ErrUnexpectedEOF, ReapPeerClose},
		{"other socket failure", errors.New("connection reset by peer"), ReapReadError},
		{"no error at all", nil, ReapReadError},
	}
	for _, tc := range cases {
		if got := classifyReapReason(tc.err); got != tc.want {
			t.Errorf("%s: classifyReapReason(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
	// A silence reap must be able to carry its own error text: that is what
	// makes the record explainable without reproducing the failure.
	if got := reapDetail(timeoutError{}); got != "i/o timeout" {
		t.Errorf("reapDetail = %q, want the error text", got)
	}
	if got := reapDetail(nil); got == "" {
		t.Error("reapDetail(nil) must still explain the exit, got empty")
	}
	if got := reapDetail(errors.New(strings.Repeat("x", maxReapDetail+50))); len(got) > maxReapDetail+4 {
		t.Errorf("reapDetail length = %d, want bounded near %d", len(got), maxReapDetail)
	}
}

// TestKeepaliveReapLedgerBounded pins the ledger's read side: it stays bounded,
// keeps the most recent records, and reports them oldest first so a failure can
// be read chronologically.
func TestKeepaliveReapLedgerBounded(t *testing.T) {
	rly := New(0)
	if _, ok := rly.LastReap(); ok {
		t.Fatal("a fresh relay must report no reap")
	}
	if got := rly.Reaps(); got != nil {
		t.Fatalf("fresh relay Reaps() = %v, want nil", got)
	}
	for i := 0; i < maxReapRecords+8; i++ {
		rly.recordReap(KeepaliveReap{
			Topic:     fmt.Sprintf("topic.%d", i),
			Reason:    ReapSilence,
			PongWait:  time.Second,
			PingsSent: 2,
			PongsSeen: 0,
		})
	}
	reaps := rly.Reaps()
	if len(reaps) != maxReapRecords {
		t.Fatalf("ledger length = %d, want the bound %d", len(reaps), maxReapRecords)
	}
	if got, want := reaps[0].Topic, fmt.Sprintf("topic.%d", 8); got != want {
		t.Fatalf("oldest kept record topic = %q, want %q (the ledger must keep the NEWEST records)", got, want)
	}
	if got, want := reaps[len(reaps)-1].Topic, fmt.Sprintf("topic.%d", maxReapRecords+7); got != want {
		t.Fatalf("newest record topic = %q, want %q", got, want)
	}
	last, ok := rly.LastReap()
	if !ok || last.Topic != reaps[len(reaps)-1].Topic {
		t.Fatalf("LastReap = %v, %v; want the newest record %q", last.Topic, ok, reaps[len(reaps)-1].Topic)
	}
	// The copy must not alias the ledger.
	reaps[0].Topic = "mutated"
	if again := rly.Reaps(); again[0].Topic == "mutated" {
		t.Fatal("Reaps() returned a slice aliasing the ledger")
	}
}

// TestKeepaliveControlWindowScaling pins the derived budget: the floor is
// preserved on a quiet host, the measured stall is scaled with headroom, the
// ceiling is a cap (not a silent clamp that hides the measurement), and the
// attempt always spans several silence windows.
func TestKeepaliveControlWindowScaling(t *testing.T) {
	if got, capped := scaleSilenceWindow(0); got != controlFloorPongWait || capped {
		t.Fatalf("scaleSilenceWindow(0) = %v capped=%v, want the floor %v uncapped", got, capped, controlFloorPongWait)
	}
	stall := 50 * time.Millisecond
	want := controlFloorPongWait + stallHeadroom*stall
	if got, capped := scaleSilenceWindow(stall); got != want || capped {
		t.Fatalf("scaleSilenceWindow(%v) = %v capped=%v, want %v uncapped", stall, got, capped, want)
	}
	if got, capped := scaleSilenceWindow(time.Hour); got != controlMaxPongWait || !capped {
		t.Fatalf("scaleSilenceWindow(1h) = %v capped=%v, want the ceiling %v capped", got, capped, controlMaxPongWait)
	}
	for _, pongWait := range []time.Duration{
		controlFloorPongWait, 400 * time.Millisecond, time.Second, controlMaxPongWait,
	} {
		windows := controlWindows(pongWait)
		if windows < controlMinWindows || windows > controlMaxWindows {
			t.Fatalf("controlWindows(%v) = %d, want within [%d,%d]", pongWait, windows, controlMinWindows, controlMaxWindows)
		}
		if window := time.Duration(windows) * pongWait; window < controlMinWindows*controlFloorPongWait {
			t.Fatalf("controlWindows(%v) window = %v, too short to prove repeated renewal", pongWait, window)
		}
	}
	if windows := controlWindows(controlFloorPongWait); windows != controlMaxWindows {
		t.Fatalf("quiet-host windows = %d, want the historical %d", windows, controlMaxWindows)
	}
}

// TestKeepaliveReapVerdicts pins the discriminator the control cell uses to
// decide between "this host was busy" (re-derive the window and retry) and a
// product-shaped reap (fail immediately). Calibrating a product bug away would
// be the failure mode this whole change exists to prevent.
func TestKeepaliveReapVerdicts(t *testing.T) {
	answeredButUnprocessed := controlAttempt{
		pongWait: 120 * time.Millisecond,
		windows:  8,
		pongs:    16,
		hasReap:  true,
		reap:     KeepaliveReap{Reason: ReapSilence, PingsSent: 16, PongsSeen: 0, PongWait: 120 * time.Millisecond},
	}
	if !unpumpedReap(answeredButUnprocessed) {
		t.Fatal("a silence reap with pongs answered by the client and none processed must read as host starvation")
	}
	if got := reapReport(answeredButUnprocessed); !strings.Contains(got, "host contention") {
		t.Fatalf("starvation verdict = %q, want it to name host contention", got)
	}

	processedButReaped := answeredButUnprocessed
	processedButReaped.reap.PongsSeen = 12
	if unpumpedReap(processedButReaped) {
		t.Fatal("a silence reap that DID process pongs must NOT be attributed to the host — that is the product-shaped shape")
	}
	if got := reapReport(processedButReaped); !strings.Contains(got, "product-shaped") {
		t.Fatalf("renewal verdict = %q, want it to name a product-shaped failure", got)
	}

	peerGone := controlAttempt{
		hasReap: true,
		reap:    KeepaliveReap{Reason: ReapPeerClose, PingsSent: 3, PongsSeen: 2},
		pongs:   2,
	}
	if unpumpedReap(peerGone) {
		t.Fatal("a peer close must not be attributed to host starvation")
	}
	if got := reapReport(peerGone); !strings.Contains(got, "peer hung up") {
		t.Fatalf("peer-close verdict = %q, want it to name the client hangup", got)
	}

	noRecord := controlAttempt{}
	if unpumpedReap(noRecord) {
		t.Fatal("an absent reap record must not read as starvation")
	}
	if got := reapReport(noRecord); !strings.Contains(got, "no reap") {
		t.Fatalf("no-record verdict = %q, want it to say no reap was recorded", got)
	}

	// A host so busy that not even one ping went out is also calibratable: the
	// silence path was never exercised, which cannot be a product regression in
	// the renewal path.
	neverPinged := controlAttempt{
		pongWait: 120 * time.Millisecond,
		windows:  8,
		hasReap:  true,
		reap:     KeepaliveReap{Reason: ReapSilence, PingsSent: 0, PongsSeen: 0},
	}
	if !unpumpedReap(neverPinged) {
		t.Fatal("a silence reap where no ping reached the wire must read as a scheduling artefact")
	}
	if got := reapReport(neverPinged); !strings.Contains(got, "scheduling artefact") {
		t.Fatalf("never-pinged verdict = %q, want it to name a scheduling artefact", got)
	}
}
