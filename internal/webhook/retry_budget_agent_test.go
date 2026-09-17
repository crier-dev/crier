package webhook

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// DF-CRIER-9 — the per-agent webhook `retries` knob must be REAL.
//
// The driver's redelivery budget came only from the server setting
// CR_WEBHOOK_MAX_RETRIES (Driver.MaxRetries), so an agent registered with
// retries:3 against that default was retried 5 times. The knob is now
// resolved per endpoint: absent or 0 -> the server budget; 1..10 -> the
// endpoint's own budget; above the server setting -> capped by it.

// retrySequence is the X-Crier-Retry value of every delivery POST the
// capture server saw, in arrival order (0 = first attempt, N = Nth retry).
// Probe posts are excluded: only real message deliveries count.
func (cs *captureServer) retrySequence() []int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []int
	for _, r := range cs.requests {
		if r.headers.Get("X-Crier-Event") != "message" {
			continue
		}
		n, err := strconv.Atoi(r.headers.Get("X-Crier-Retry"))
		if err != nil {
			n = -1
		}
		out = append(out, n)
	}
	return out
}

// budgetRun drives ONE async delivery against an always-503 endpoint through
// a driver with the given server max, and returns the attempt sequence the
// endpoint saw plus the captured driver log.
func budgetRun(t *testing.T, maxRetries int, mkCfg func(url string) *Config) ([]int, string) {
	t.Helper()
	logs := captureLogs(t)
	cs := newCaptureServer(t, 503)
	cfg := mkCfg(cs.server.URL)

	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries:       maxRetries,
		RedeliverEvery:   40 * time.Millisecond,
		ProbeEvery:       time.Hour,
		CircuitThreshold: 100, // keep the circuit closed so retries exhaust
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(string) (*Config, error) { return cfg, nil })

	env := &Envelope{Crier: EnvelopeMeta{
		Version: 1, MessageID: "msg-budget-1", Kind: KindMessage, Sender: "agent-a",
	}}
	delivered, err := d.Deliver("agent-b", cfg, env)
	if err != nil || delivered {
		t.Fatalf("async deliver: delivered=%v err=%v (want accepted, no err)", delivered, err)
	}

	// The drop line is the terminal event: wait for it, then let the queue
	// settle so an in-flight retry cannot be counted as "exhausted".
	waitFor(t, "retry-exhausted drop line", func() bool {
		return strings.Contains(logs.String(), "retries exhausted")
	})
	time.Sleep(150 * time.Millisecond)
	if n := d.queue.Len(); n != 0 {
		t.Fatalf("queue Len = %d after exhaustion, want 0", n)
	}
	return cs.retrySequence(), logs.String()
}

// TestDriver_AgentRetries_BoundsBudget: an agent with retries:3 under a
// server max of 5 gets exactly 4 POSTs (X-Crier-Retry 0..3), and the drop
// line names the budget it used AND the value the agent configured.
func TestDriver_AgentRetries_BoundsBudget(t *testing.T) {
	attempts, out := budgetRun(t, 5, func(url string) *Config {
		return &Config{URL: url, DeliveryMode: "async", Retries: 3, TimeoutMs: 5000}
	})

	if want := []int{0, 1, 2, 3}; !equalInts(attempts, want) {
		t.Fatalf("attempts = %v, want %v (4 POSTs: 1 initial + 3 retries)", attempts, want)
	}
	for _, want := range []string{
		"webhook: delivery dropped (retries exhausted)",
		"agent=agent-b",
		"retries=4", // existing key: the attempt count that exhausted the budget
		"budget=3",
		"agent_retries=3",
		"max_retries=5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drop line does not contain %q — log: %s", want, out)
		}
	}
}

// TestDriver_AgentRetries_ZeroAndAbsentUseServerBudget: retries absent (the
// JSON field never sent, so the decoded Config leaves it 0) and retries
// explicitly 0 both keep today's behaviour — the server budget, 6 POSTs.
func TestDriver_AgentRetries_ZeroAndAbsentUseServerBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(url string) *Config
	}{
		{"explicit zero", func(url string) *Config {
			return &Config{URL: url, DeliveryMode: "async", Retries: 0, TimeoutMs: 5000}
		}},
		{"field absent", func(url string) *Config {
			// Exactly what registration decodes from a webhook object that
			// omits `retries`: the premise asserted alongside the behaviour.
			var c Config
			raw := `{"url":` + strconv.Quote(url) + `,"delivery_mode":"async","timeout_ms":5000}`
			if err := json.Unmarshal([]byte(raw), &c); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if c.Retries != 0 {
				t.Fatalf("premise: absent retries decoded to %d, want 0", c.Retries)
			}
			if strings.Contains(raw, "retries") {
				t.Fatalf("premise: the registration body names retries: %s", raw)
			}
			return &c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, out := budgetRun(t, 5, tc.mk)
			if want := []int{0, 1, 2, 3, 4, 5}; !equalInts(attempts, want) {
				t.Fatalf("attempts = %v, want %v (server budget unchanged)", attempts, want)
			}
			for _, want := range []string{"retries=6", "budget=5", "agent_retries=0", "max_retries=5"} {
				if !strings.Contains(out, want) {
					t.Errorf("drop line does not contain %q — log: %s", want, out)
				}
			}
		})
	}
}

// TestDriver_AgentRetries_CappedByServerSetting: retries:10 under a server
// max of 5 is capped — 6 POSTs, not 11.
func TestDriver_AgentRetries_CappedByServerSetting(t *testing.T) {
	attempts, out := budgetRun(t, 5, func(url string) *Config {
		return &Config{URL: url, DeliveryMode: "async", Retries: 10, TimeoutMs: 5000}
	})

	if want := []int{0, 1, 2, 3, 4, 5}; !equalInts(attempts, want) {
		t.Fatalf("attempts = %v, want %v (10 capped at the server setting 5)", attempts, want)
	}
	for _, want := range []string{"retries=6", "budget=5", "agent_retries=10", "max_retries=5"} {
		if !strings.Contains(out, want) {
			t.Errorf("drop line does not contain %q — log: %s", want, out)
		}
	}
}

// TestRetryBudget_Resolution pins the resolver's boundaries directly: the
// server setting is the default, the ceiling, and the winner for every
// non-positive agent value.
func TestRetryBudget_Resolution(t *testing.T) {
	d := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DriverConfig{MaxRetries: 5})
	for _, tc := range []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil config", nil, 5},
		{"absent (zero value)", &Config{}, 5},
		{"explicit zero", &Config{Retries: 0}, 5},
		{"one", &Config{Retries: 1}, 1},
		{"three", &Config{Retries: 3}, 3},
		{"equal to the server setting", &Config{Retries: 5}, 5},
		{"above the server setting", &Config{Retries: 6}, 5},
		{"registration maximum", &Config{Retries: 10}, 5},
		{"negative (hand-built config)", &Config{Retries: -1}, 5},
	} {
		if got := d.retryBudget(tc.cfg); got != tc.want {
			t.Errorf("retryBudget(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	// A server setting of 1 still leaves an agent's smaller budget alone,
	// and never exceeds itself.
	d1 := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DriverConfig{MaxRetries: 1})
	if got := d1.retryBudget(&Config{Retries: 10}); got != 1 {
		t.Errorf("retryBudget(10) with max 1 = %d, want 1", got)
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
