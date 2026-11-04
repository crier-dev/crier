package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/hermes"
	"github.com/crier-dev/crier/internal/harness/opencode"
)

// ---------------------------------------------------------------------------
// The fake harness
//
// One shell script stands in for every harness binary. It records its argv
// NUL-separated (so byte equality and argument boundaries are both observable),
// its working directory, its own pid and its child's, and it can fail, hang or
// report an unknown session on demand. Written to t.TempDir() and chmod 0755 so
// no test depends on a harness being installed.
// ---------------------------------------------------------------------------

const fakeHarnessScript = `#!/bin/bash
set -u
dir="${HARNESS_FAKE_DIR:?HARNESS_FAKE_DIR is unset}"
mode="${HARNESS_FAKE_MODE:-ok}"

if [ "${1:-}" = "--version" ]; then
  printf '%s\n' "fakeharness 1.2.3"
  exit 0
fi

rec="$(mktemp "$dir/argv.XXXXXX")"
printf '%s\0' "$@" > "$rec"
printf '%s\n' "$PWD" >> "$dir/pwd"

case "$mode" in
  unknown)
    printf '%s\n' "error: session not found" >&2
    exit 1
    ;;
  fail)
    printf '%s\n' "provider authentication failed" >&2
    exit 3
    ;;
  timeout)
    printf '%s\n' "$$" >> "$dir/pids"
    sleep 30 &
    printf '%s\n' "$!" >> "$dir/pids"
    wait
    ;;
  slow)
    s="$(date +%s%N)"
    printf 'start %s\n' "$s" >> "$dir/timeline"
    sleep 0.15
    e="$(date +%s%N)"
    printf 'end %s\n' "$e" >> "$dir/timeline"
    printf '%s\n' '{"sessionID":"ses_fake_created","events":[]}'
    ;;
  badjson)
    printf '%s\n' "not json at all"
    ;;
  *)
    printf '%s\n' '{"sessionID":"ses_fake_created","events":[]}'
    ;;
esac
exit 0
`

type fakeHarness struct {
	t   *testing.T
	dir string
	bin string

	gw       *httptest.Server
	gwMu     sync.Mutex
	gwBodies []string
}

func newFakeHarness(t *testing.T) *fakeHarness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-harness")
	if err := os.WriteFile(bin, []byte(fakeHarnessScript), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	f := &fakeHarness{t: t, dir: dir, bin: bin}
	f.gw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		f.gwMu.Lock()
		f.gwBodies = append(f.gwBodies, body.String())
		f.gwMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_gateway_1"}`))
	}))
	t.Cleanup(f.gw.Close)
	t.Setenv("HARNESS_FAKE_DIR", dir)
	t.Setenv("HARNESS_FAKE_MODE", "ok")
	return f
}

func (f *fakeHarness) mode(mode string) { f.t.Setenv("HARNESS_FAKE_MODE", mode) }

// invocations returns every recorded argv, in filename order.
func (f *fakeHarness) invocations() [][]string {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.dir, "argv.*"))
	if err != nil {
		f.t.Fatalf("glob argv: %v", err)
	}
	sort.Strings(paths)
	out := make([][]string, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.t.Fatalf("read %s: %v", p, err)
		}
		parts := bytes.Split(raw, []byte{0})
		args := make([]string, 0, len(parts))
		for _, part := range parts {
			if len(part) == 0 {
				continue
			}
			args = append(args, string(part))
		}
		out = append(out, args)
	}
	return out
}

func (f *fakeHarness) gatewayBodies() []string {
	f.gwMu.Lock()
	defer f.gwMu.Unlock()
	out := make([]string, len(f.gwBodies))
	copy(out, f.gwBodies)
	return out
}

// ---------------------------------------------------------------------------
// The contract table
// ---------------------------------------------------------------------------

// contractCase is one adapter's shape of the SAME contract (§7). Adding an
// adapter means adding a row here; a row that cannot satisfy the table is an
// adapter with a different contract, which is the thing this suite exists to
// prevent.
type contractCase struct {
	name string
	// build returns the adapter under test pointed at binary, the fake harness
	// in f, and a per-delivery timeout.
	build func(t *testing.T, binary string, f *fakeHarness, timeout time.Duration) harness.Adapter
	// createRunsProcess is false for an adapter whose create path is an HTTP
	// gateway call, not a process.
	createRunsProcess bool
	// createState is the receipt state a successful create reports.
	createState string
	// createAbsentToken must NOT appear in the create argv (the token that
	// would mean "continue this session").
	createAbsentToken string
	// continueFlag is the argv token immediately followed by the session id on
	// the continue path.
	continueFlag string
}

func contractCases() []contractCase {
	return []contractCase{
		{
			name: "hermes",
			build: func(t *testing.T, binary string, f *fakeHarness, timeout time.Duration) harness.Adapter {
				return hermes.New(hermes.Config{
					Binary:     binary,
					GatewayURL: f.gw.URL,
					Timeout:    timeout,
				})
			},
			createRunsProcess: false,
			createState:       harness.StateAccepted,
			createAbsentToken: "--resume",
			continueFlag:      "--resume",
		},
		{
			name: "opencode",
			build: func(t *testing.T, binary string, f *fakeHarness, timeout time.Duration) harness.Adapter {
				return opencode.New(opencode.Config{Binary: binary, Timeout: timeout})
			},
			createRunsProcess: true,
			createState:       harness.StateDispatched,
			createAbsentToken: "-s",
			continueFlag:      "-s",
		},
	}
}

// TestAdapterContract runs the §7 contract against every registered adapter.
func TestAdapterContract(t *testing.T) {
	for _, tc := range contractCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ProbeReportsBinaryAndVersion", func(t *testing.T) {
				f := newFakeHarness(t)
				cap, err := tc.build(t, f.bin, f, 0).Probe(context.Background())
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				if !cap.Available {
					t.Fatalf("want available, got reason %q", cap.Reason)
				}
				if cap.Version != "1.2.3" {
					t.Fatalf("version: want 1.2.3, got %q", cap.Version)
				}
				if !cap.Features["continue"] || !cap.Features["create"] {
					t.Fatalf("features must advertise continue+create: %v", cap.Features)
				}
			})

			// TestProbeMissingBinary: a wrong path is unavailable, and the
			// reason names it.
			t.Run("ProbeMissingBinary", func(t *testing.T) {
				f := newFakeHarness(t)
				const missing = "/nonexistent/harness-binary"
				cap, err := tc.build(t, missing, f, 0).Probe(context.Background())
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				if cap.Available {
					t.Fatalf("want unavailable for %s", missing)
				}
				if !strings.Contains(cap.Reason, missing) {
					t.Fatalf("reason must name the binary %q: %q", missing, cap.Reason)
				}
			})

			t.Run("DeliverCreatesSession", func(t *testing.T) {
				f := newFakeHarness(t)
				a := tc.build(t, f.bin, f, 0)
				msg := harness.Message{ID: "m_1", SessionID: "s_1", Author: "ci", Text: "start a session"}
				rc, err := a.Deliver(context.Background(), harness.SessionRef{}, msg)
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != tc.createState {
					t.Fatalf("state: want %q, got %q (error %q tail %q)", tc.createState, rc.State, rc.Error, rc.Tail)
				}
				if !rc.Created {
					t.Fatalf("create path must report Created=true: %+v", rc)
				}
				if tc.createRunsProcess {
					args := f.invocations()
					if len(args) != 1 {
						t.Fatalf("want 1 invocation, got %d: %v", len(args), args)
					}
					for _, arg := range args[0] {
						if arg == tc.createAbsentToken {
							t.Fatalf("create argv must not carry %q: %v", tc.createAbsentToken, args[0])
						}
					}
					if !containsElement(args[0], "--title") {
						t.Fatalf("create argv must carry --title: %v", args[0])
					}
					if rc.SessionID != "ses_fake_created" {
						t.Fatalf("session id: want ses_fake_created, got %q", rc.SessionID)
					}
					return
				}
				if got := f.invocations(); len(got) != 0 {
					t.Fatalf("gateway create must not run a process, got %v", got)
				}
				bodies := f.gatewayBodies()
				if len(bodies) != 1 {
					t.Fatalf("want 1 gateway request, got %d", len(bodies))
				}
				if !strings.Contains(bodies[0], msg.Text) {
					t.Fatalf("gateway body must carry the message: %q", bodies[0])
				}
				if rc.SessionID != "ses_gateway_1" {
					t.Fatalf("session id: want ses_gateway_1, got %q", rc.SessionID)
				}
			})

			// TestDeliverContinuesSession: the exact id is in argv.
			t.Run("DeliverContinuesSession", func(t *testing.T) {
				f := newFakeHarness(t)
				a := tc.build(t, f.bin, f, 0)
				const sid = "ses_existing_42"
				msg := harness.Message{ID: "m_2", SessionID: "s_1", Author: "ci", Text: "next turn"}
				rc, err := a.Deliver(context.Background(), harness.SessionRef{ID: sid}, msg)
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != harness.StateDispatched {
					t.Fatalf("state: want dispatched, got %q (error %q tail %q)", rc.State, rc.Error, rc.Tail)
				}
				if rc.SessionID != sid || rc.Created {
					t.Fatalf("receipt must continue %q and not claim creation: %+v", sid, rc)
				}
				args := f.invocations()
				if len(args) != 1 {
					t.Fatalf("want 1 invocation, got %d: %v", len(args), args)
				}
				idx := indexOf(args[0], tc.continueFlag)
				if idx < 0 || idx+1 >= len(args[0]) || args[0][idx+1] != sid {
					t.Fatalf("continue argv must carry %q %q: %v", tc.continueFlag, sid, args[0])
				}
			})

			// TestDeliverUnknownSessionFails: failed, named, and NO second
			// invocation — a silent create would fork the transcript.
			t.Run("DeliverUnknownSessionFails", func(t *testing.T) {
				f := newFakeHarness(t)
				f.mode("unknown")
				a := tc.build(t, f.bin, f, 0)
				msg := harness.Message{ID: "m_3", SessionID: "s_1", Author: "ci", Text: "ghost turn"}
				rc, err := a.Deliver(context.Background(), harness.SessionRef{ID: "ses_missing"}, msg)
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != harness.StateFailed {
					t.Fatalf("state: want failed, got %q", rc.State)
				}
				if rc.Error != harness.CodeSessionUnknown {
					t.Fatalf("error: want %s, got %q (tail %q)", harness.CodeSessionUnknown, rc.Error, rc.Tail)
				}
				if got := f.invocations(); len(got) != 1 {
					t.Fatalf("unknown session must not trigger a replacement create; got %d invocations: %v", len(got), got)
				}
			})

			t.Run("DeliverExitNonZeroCarriesTail", func(t *testing.T) {
				f := newFakeHarness(t)
				f.mode("fail")
				a := tc.build(t, f.bin, f, 0)
				rc, err := a.Deliver(context.Background(),
					harness.SessionRef{ID: "ses_live"}, harness.Message{ID: "m_4", SessionID: "s_1", Text: "x"})
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != harness.StateFailed || rc.Error != harness.CodeExitNonZero {
					t.Fatalf("want failed/%s, got %+v", harness.CodeExitNonZero, rc)
				}
				if !strings.Contains(rc.Tail, "provider authentication failed") {
					t.Fatalf("tail must carry the harness stderr: %q", rc.Tail)
				}
			})

			// TestDeliverTimeoutKillsProcessGroup: the child AND its helper die.
			t.Run("DeliverTimeoutKillsProcessGroup", func(t *testing.T) {
				f := newFakeHarness(t)
				f.mode("timeout")
				a := tc.build(t, f.bin, f, 400*time.Millisecond)
				start := time.Now()
				rc, err := a.Deliver(context.Background(),
					harness.SessionRef{ID: "ses_live"}, harness.Message{ID: "m_5", SessionID: "s_1", Text: "hang"})
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("timeout did not cut the delivery short: %s", elapsed)
				}
				if rc.State != harness.StateFailed || rc.Error != harness.CodeTimeout {
					t.Fatalf("want failed/%s, got %+v", harness.CodeTimeout, rc)
				}
				pids := readPIDs(t, filepath.Join(f.dir, "pids"))
				if len(pids) < 2 {
					t.Fatalf("fake harness should have recorded itself and its child, got %v", pids)
				}
				for _, pid := range pids {
					waitDead(t, pid, 5*time.Second)
				}
			})

			// TestMessageTextNeverInterpolatedIntoShell: one argv element,
			// byte-identical, with no shell in between.
			t.Run("MessageTextNeverInterpolatedIntoShell", func(t *testing.T) {
				f := newFakeHarness(t)
				a := tc.build(t, f.bin, f, 0)
				text := "build 4412 failed; rm -rf / $(id) `whoami` && echo pwned\nsecond line"
				msg := harness.Message{ID: "m_6", SessionID: "s_1", Author: "ci", Text: text}
				rc, err := a.Deliver(context.Background(), harness.SessionRef{ID: "ses_live"}, msg)
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != harness.StateDispatched {
					t.Fatalf("state: want dispatched, got %q (%q)", rc.State, rc.Error)
				}
				args := f.invocations()
				if len(args) != 1 {
					t.Fatalf("want 1 invocation, got %d: %v", len(args), args)
				}
				if last := args[0][len(args[0])-1]; last != text {
					t.Fatalf("message text must arrive byte-identical as ONE argv element:\nwant %q\ngot  %q", text, last)
				}
				for _, arg := range args[0] {
					if arg == "sh" || arg == "bash" || arg == "-c" {
						t.Fatalf("argv must contain no shell: %v", args[0])
					}
				}
			})

			// TestPerSessionSerialization: two deliveries to one courier
			// session never overlap.
			t.Run("PerSessionSerialization", func(t *testing.T) {
				f := newFakeHarness(t)
				f.mode("slow")
				a := tc.build(t, f.bin, f, 10*time.Second)
				reg := harness.NewRegistry(harness.RegistryOptions{
					Enabled: true, Default: tc.name, Timeout: 10 * time.Second, MaxConcurrent: 4,
				})
				reg.Register(a)
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						rc := reg.Deliver(context.Background(), harness.DispatchRequest{
							Harness:    tc.name,
							SessionRef: harness.SessionRef{ID: "ses_serial"},
							Message: harness.Message{
								ID:        fmt.Sprintf("m_serial_%d", i),
								SessionID: "s_serial",
								Text:      "turn",
							},
						})
						if rc.State != harness.StateDispatched {
							t.Errorf("delivery %d: state %q error %q", i, rc.State, rc.Error)
						}
					}(i)
				}
				wg.Wait()
				spans := readTimeline(t, filepath.Join(f.dir, "timeline"))
				if len(spans) != 2 {
					t.Fatalf("want 2 spans, got %d: %v", len(spans), spans)
				}
				for i := 1; i < len(spans); i++ {
					if spans[i].start < spans[i-1].end {
						t.Fatalf("deliveries overlapped: %v", spans)
					}
				}
			})

			// TestConcurrencyCap: 10 deliveries, cap 2, never more than 2 at
			// once.
			t.Run("ConcurrencyCap", func(t *testing.T) {
				f := newFakeHarness(t)
				f.mode("slow")
				a := tc.build(t, f.bin, f, 10*time.Second)
				reg := harness.NewRegistry(harness.RegistryOptions{
					Enabled: true, Default: tc.name, Timeout: 10 * time.Second, MaxConcurrent: 2,
				})
				reg.Register(a)
				var wg sync.WaitGroup
				for i := 0; i < 10; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						rc := reg.Deliver(context.Background(), harness.DispatchRequest{
							Harness:    tc.name,
							SessionRef: harness.SessionRef{ID: "ses_cap"},
							Message: harness.Message{
								ID:        fmt.Sprintf("m_cap_%d", i),
								SessionID: fmt.Sprintf("s_cap_%d", i),
								Text:      "turn",
							},
						})
						if rc.State != harness.StateDispatched {
							t.Errorf("delivery %d: state %q error %q", i, rc.State, rc.Error)
						}
					}(i)
				}
				wg.Wait()
				spans := readTimeline(t, filepath.Join(f.dir, "timeline"))
				if len(spans) != 10 {
					t.Fatalf("want 10 spans, got %d", len(spans))
				}
				if peak := maxOverlap(spans); peak > 2 {
					t.Fatalf("concurrency cap 2 exceeded: peak %d, spans %v", peak, spans)
				}
			})

			// TestReceiptPersistedOnMessage: the receipt is evidence, so it
			// must survive the JSON trip onto the stored message. (The
			// GET /delivery-log read route arrives with CRIER-INGRESS; there is
			// no delivery-log route in this tree to read it back from.)
			t.Run("ReceiptPersistedOnMessage", func(t *testing.T) {
				f := newFakeHarness(t)
				a := tc.build(t, f.bin, f, 0)
				rc, err := a.Deliver(context.Background(),
					harness.SessionRef{ID: "ses_live"},
					harness.Message{ID: "m_7", SessionID: "s_1", Text: "persist me"})
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				raw, err := json.Marshal(rc)
				if err != nil {
					t.Fatalf("marshal receipt: %v", err)
				}
				var back harness.Receipt
				if err := json.Unmarshal(raw, &back); err != nil {
					t.Fatalf("unmarshal receipt: %v", err)
				}
				if back.State != rc.State || back.SessionID != rc.SessionID || back.Harness != tc.name {
					t.Fatalf("receipt lost fields on the wire: %+v vs %+v", back, rc)
				}
				if !strings.Contains(string(raw), `"state":"`+harness.StateDispatched+`"`) {
					t.Fatalf("receipt JSON must carry the state: %s", raw)
				}
			})

			t.Run("DeliverWorkdirMissing", func(t *testing.T) {
				f := newFakeHarness(t)
				a := tc.build(t, f.bin, f, 0)
				rc, err := a.Deliver(context.Background(),
					harness.SessionRef{ID: "ses_live", Workdir: "/nonexistent/workdir"},
					harness.Message{ID: "m_8", SessionID: "s_1", Text: "x"})
				if err != nil {
					t.Fatalf("deliver: %v", err)
				}
				if rc.State != harness.StateFailed || rc.Error != harness.CodeWorkdirMissing {
					t.Fatalf("want failed/%s, got %+v", harness.CodeWorkdirMissing, rc)
				}
				if got := f.invocations(); len(got) != 0 {
					t.Fatalf("a missing workdir must not run the harness: %v", got)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsElement(args []string, want string) bool { return indexOf(args, want) >= 0 }

func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pids: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("bad pid %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// waitDead fails if pid is still signalable after budget.
func waitDead(t *testing.T, pid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive %s after the timeout kill — process group was not killed", pid, budget)
}

type span struct{ start, end int64 }

func readTimeline(t *testing.T, path string) []span {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	type event struct {
		at  int64
		end bool
	}
	var events []event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ns, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("bad timestamp %q: %v", fields[1], err)
		}
		events = append(events, event{at: ns, end: fields[0] == "end"})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })
	var spans []span
	var open []int64
	for _, ev := range events {
		if !ev.end {
			open = append(open, ev.at)
			continue
		}
		if len(open) == 0 {
			continue
		}
		spans = append(spans, span{start: open[len(open)-1], end: ev.at})
		open = open[:len(open)-1]
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans
}

// maxOverlap is the largest number of spans in flight at once.
func maxOverlap(spans []span) int {
	type event struct {
		at  int64
		del bool
	}
	var events []event
	for _, s := range spans {
		events = append(events, event{at: s.start}, event{at: s.end, del: true})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at == events[j].at {
			// A start at the same nanosecond as an end is not an overlap.
			return events[i].del
		}
		return events[i].at < events[j].at
	})
	cur, peak := 0, 0
	for _, ev := range events {
		if ev.del {
			cur--
		} else {
			cur++
			if cur > peak {
				peak = cur
			}
		}
	}
	return peak
}

// processAlive reports whether pid can be signalled (Signal 0). EPERM means the
// process exists but belongs to another user — still alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
