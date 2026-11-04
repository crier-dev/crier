package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/pi"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

// newJointSession writes one session record straight through the repository so
// the test controls the harness binding, which the public create route does not
// expose.
func newJointSession(t *testing.T, repo session.Repository, id, harnessName, harnessSessionID string) {
	t.Helper()
	sess := &session.Session{
		ID:               id,
		Namespace:        "default",
		Kind:             session.KindChannel,
		Title:            id,
		CreatedAt:        time.Now().UTC(),
		CreatedBy:        session.AuthorRef{Agent: "author"},
		State:            session.SessionOpen,
		Harness:          harnessName,
		HarnessSessionID: harnessSessionID,
	}
	if err := repo.CreateSession(context.Background(), sess, 1); err != nil {
		t.Fatalf("create session %q: %v", id, err)
	}
}

func newJointRepo(t *testing.T) session.Repository {
	t.Helper()
	repo, err := session.OpenStore(context.Background(), config.SessionBackendJSONL, session.StoreOptions{
		LogRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// TestHarnessDispatchSkipsUnboundSessions is the honest-reporting half of the
// joint: a session with no harness binding answers store/stored, never a fake
// "dispatched", and never an error.
func TestHarnessDispatchSkipsUnboundSessions(t *testing.T) {
	ctx := context.Background()
	repo := newJointRepo(t)
	newJointSession(t, repo, "s_plain", "", "")

	d := &harnessDispatch{reg: harness.NewRegistry(harness.RegistryOptions{Enabled: true}), repo: repo}
	report, err := d.Dispatch(ctx, ingress.DispatchRequest{SessionID: "s_plain", MessageID: "m1", Text: "hello"})
	if err != nil {
		t.Fatalf("dispatch returned an error for a store-only session: %v", err)
	}
	if report.Mode != "store" || report.State != "stored" {
		t.Fatalf("unbound session report = %+v, want store/stored", report)
	}
}

// TestHarnessDispatchRefusesWhenDisabled proves the wiring cannot smuggle a
// message past harness.enabled=false: a session that IS bound still reports
// store/stored when delivery is switched off.
func TestHarnessDispatchRefusesWhenDisabled(t *testing.T) {
	ctx := context.Background()
	repo := newJointRepo(t)
	newJointSession(t, repo, "s_bound", "pi", "crier-joint-disabled")

	d := &harnessDispatch{reg: harness.NewRegistry(harness.RegistryOptions{Enabled: false}), repo: repo}
	report, err := d.Dispatch(ctx, ingress.DispatchRequest{SessionID: "s_bound", MessageID: "m1", Text: "hello"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if report.Mode != "store" || report.State != "stored" {
		t.Fatalf("disabled-delivery report = %+v, want store/stored", report)
	}
}

// TestHarnessDispatchInboxModeIsStoreOnly proves the inbox branch never
// pretends a harness claimed an inbox write.
func TestHarnessDispatchInboxModeIsStoreOnly(t *testing.T) {
	ctx := context.Background()
	repo := newJointRepo(t)
	d := &harnessDispatch{reg: harness.NewRegistry(harness.RegistryOptions{Enabled: true}), repo: repo}

	report, err := d.Dispatch(ctx, ingress.DispatchRequest{Mode: ingress.ModeInbox, AgentID: "agent-1", MessageID: "e1", Text: "hi"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if report.Mode != "store" || report.State != "stored" {
		t.Fatalf("inbox-mode report = %+v, want store/stored", report)
	}
}

// TestHarnessDispatchDeliversIntoARealPiSession is the live half of the joint.
// It skips unless CRIER_TEST_LIVE_HARNESS=1 and pi is installed, then proves the
// whole path against the REAL harness: a courier session bound to pi gets the
// message written into pi's own session file.
func TestHarnessDispatchDeliversIntoARealPiSession(t *testing.T) {
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") == "" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the live harness joint (needs pi installed and signed in)")
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi not installed")
	}

	ctx := context.Background()
	root := t.TempDir()
	repo := newJointRepo(t)

	stamp := time.Now().Format("150405")
	token := "CRIER-JOINT-TOKEN-" + stamp
	sessID := "crier-joint-" + stamp
	newJointSession(t, repo, "s_live", "pi", sessID)

	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled:       true,
		Default:       "pi",
		Timeout:       180 * time.Second,
		MaxConcurrent: 1,
	})
	reg.Register(pi.New(pi.Config{Binary: piBin, Workdir: root, Timeout: 180 * time.Second}))

	d := &harnessDispatch{reg: reg, repo: repo}
	report, err := d.Dispatch(ctx, ingress.DispatchRequest{
		SessionID: "s_live",
		MessageID: "m_live_1",
		Text:      "Remember this token for later: " + token + ". Reply with just: ACK",
		Tags:      []string{"proof"},
		Author:    session.AuthorRef{Agent: "author"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if report.Mode != "harness" || report.State == "stored" || report.State == "failed" {
		t.Fatalf("live dispatch report = %+v, want a harness dispatch", report)
	}

	// pi's OWN transcript is the evidence.
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".pi", "agent", "sessions", "*", "*"+sessID+".jsonl"))
	if len(matches) == 0 {
		t.Fatalf("pi wrote no session file for %q; the delivery is not proven", sessID)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read pi session: %v", err)
	}
	if !strings.Contains(string(raw), token) {
		t.Fatalf("pi's transcript for %q does not contain the delivered token; file=%s", sessID, matches[0])
	}
	t.Logf("live joint proven: session %s, report %+v, transcript %s", sessID, report, matches[0])
}
