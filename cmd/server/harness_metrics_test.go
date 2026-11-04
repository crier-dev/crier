package main

// harness_metrics_test.go — CRIER-OBS-1's live arm (criterion 3): the harness
// dispatch series must appear on GET /metrics of a REAL booted server after a
// real ingress push drove Registry.Deliver through a fake harness binary.
// The unit arm in internal/harness proves the wiring; this one proves the
// wiring is on the served surface — boot, push over HTTP, scrape /metrics.
//
// Two arms are driven against the booted server:
//
//   - a session bound to pi (the fake binary exits 0) really dispatches, and
//     crier_harness_dispatches_total{harness="pi",state="dispatched"} appears
//     with the duration histogram's _count;
//   - a session bound to an adapter that is not registered fails with its
//     class, and the failure is countable as
//     crier_harness_dispatch_errors_total{harness="nosuch",class="HARNESS_UNKNOWN"}
//     instead of living only in a log line.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/session"
)

// harnessMetricFamilies are the three CRIER-OBS-1 families; all must render
// HELP/TYPE on a fresh boot even before the first dispatch (the series are
// constructed at package init, and the exposition renders family metadata
// with zero samples — the series list is the contract).
var harnessMetricFamilies = []string{
	"crier_harness_dispatches_total",
	"crier_harness_dispatch_errors_total",
	"crier_harness_dispatch_duration_seconds",
}

// metricSample returns the one sample line of series whose label set is
// EXACTLY the given k="v" pairs (order as rendered, which is registration
// order), or "" when that sample is absent.
func metricSample(body, series string, labels ...string) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series+"{") {
			continue
		}
		end := strings.Index(line, "}")
		if end < 0 {
			continue
		}
		got := strings.Split(line[len(series)+1:end], ",")
		if len(got) != len(labels) {
			continue
		}
		matched := true
		for i := range labels {
			if got[i] != labels[i] {
				matched = false
				break
			}
		}
		if matched {
			return line
		}
	}
	return ""
}

// TestHarnessDispatchMetricsOnBootedServer is criterion 3 against the real
// server: the harness registry the boot builds from CR_HARNESS_* env (see
// harnesswiring.go) performs the dispatch on an ingress push, and the
// process-wide /metrics exposition carries the labelled samples afterwards.
func TestHarnessDispatchMetricsOnBootedServer(t *testing.T) {
	parallelBoot(t)

	// The session root is shared by hand: the sessions are written through a
	// repository handle BEFORE boot — the public create route does not
	// expose a harness binding — and the server then opens its own handle
	// over the same JSONL root (the two-handle composition
	// session_lifecycle_test.go already relies on).
	root := t.TempDir()
	workdir := t.TempDir()
	ctx := context.Background()
	seed, err := session.OpenStore(ctx, config.SessionBackendJSONL, session.StoreOptions{LogRoot: root})
	require.NoError(t, err)
	now := time.Now().UTC()
	newBoundSession := func(id, harnessName string) {
		t.Helper()
		sess := &session.Session{
			ID:        id,
			Namespace: "default",
			Kind:      session.KindChannel,
			Title:     id,
			CreatedAt: now,
			CreatedBy: session.AuthorRef{Agent: "author"},
			State:     session.SessionOpen,
			Harness:   harnessName,
		}
		require.NoError(t, seed.CreateSession(ctx, sess, 1))
		// The author joins at seq 2 so the mode=session participant gate
		// admits the push below.
		require.NoError(t, seed.AddMember(ctx, &session.Member{
			SessionID:  id,
			MemberType: session.MemberAgent,
			MemberID:   "author",
			Role:       session.RoleMember,
			AddedAt:    now,
		}, 2, nil))
	}
	newBoundSession("s_obs_dispatched", "pi")
	newBoundSession("s_obs_unknown", "nosuch")
	require.NoError(t, seed.Close())

	// The fake pi binary answers every invocation with exit 0 (the CLI fake
	// from harness_cli_test.go), so the delivery really launches a process
	// and the receipt comes back dispatched.
	fake := writeCLIFakeHarness(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":  "false",
		"CR_SESSION_BACKEND":    "jsonl",
		"CR_SESSION_LOG_ROOT":   root,
		"CR_GROUP_ROOT":         t.TempDir(),
		"CR_ENABLE_METRICS":     "true",
		"CR_HARNESS_ENABLED":    "true",
		"CR_HARNESS_DEFAULT":    "pi",
		"CR_HARNESS_PI_BINARY":  fake,
		"CR_HARNESS_PI_WORKDIR": workdir,
	})
	client := &http.Client{Timeout: 10 * time.Second}

	status, ctype, fresh := get(t, client, base+"/metrics", "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, ctype, "text/plain", "scrape: %s", ctype)
	for _, name := range harnessMetricFamilies {
		require.Contains(t, fresh, "# HELP "+name+" ", "family %s has no HELP on a fresh boot", name)
		require.Contains(t, fresh, "# TYPE "+name+" ", "family %s has no TYPE on a fresh boot", name)
	}

	var pushed struct {
		Delivery struct {
			Mode    string `json:"mode"`
			Harness string `json:"harness"`
			State   string `json:"state"`
		} `json:"delivery"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_obs_dispatched"},
		"author": map[string]string{"agent": "author"},
		"text":   "metrics probe",
	}, &pushed)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, "harness", pushed.Delivery.Mode, "the push must have been dispatched, not stored: %s", raw)
	require.Equal(t, "pi", pushed.Delivery.Harness, "body: %s", raw)
	require.Equal(t, "dispatched", pushed.Delivery.State, "body: %s", raw)

	// The failure arm: an adapter that is not registered fails with its
	// error class — countable, not only a log line.
	var refused struct {
		Delivery struct {
			Mode  string `json:"mode"`
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"delivery"`
	}
	code, raw = sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_obs_unknown"},
		"author": map[string]string{"agent": "author"},
		"text":   "metrics probe",
	}, &refused)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, "failed", refused.Delivery.State, "body: %s", raw)
	require.Contains(t, refused.Delivery.Error, "HARNESS_UNKNOWN", "body: %s", raw)

	_, _, scrape := get(t, client, base+"/metrics", "")
	// The label values under "pi" and "nosuch" are written by no other test
	// in this package, so the exact sample values are deterministic
	// regardless of test order.
	require.Equal(t,
		`crier_harness_dispatches_total{harness="pi",state="dispatched"} 1`,
		metricSample(scrape, "crier_harness_dispatches_total", `harness="pi"`, `state="dispatched"`),
		"dispatch counter sample:\n%s", scrape)
	require.Equal(t,
		`crier_harness_dispatches_total{harness="nosuch",state="failed"} 1`,
		metricSample(scrape, "crier_harness_dispatches_total", `harness="nosuch"`, `state="failed"`),
		"failed-dispatch sample:\n%s", scrape)
	require.Equal(t,
		`crier_harness_dispatch_errors_total{harness="nosuch",class="HARNESS_UNKNOWN"} 1`,
		metricSample(scrape, "crier_harness_dispatch_errors_total", `harness="nosuch"`, `class="HARNESS_UNKNOWN"`),
		"error-class sample:\n%s", scrape)
	require.Equal(t,
		`crier_harness_dispatch_duration_seconds_count{harness="pi"} 1`,
		metricSample(scrape, "crier_harness_dispatch_duration_seconds_count", `harness="pi"`),
		"duration histogram sample:\n%s", scrape)
}
