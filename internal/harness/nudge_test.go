package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
)

// TestSessionStartHintPrepended pins §7.7: with mcp.nudge.session_start armed,
// the delivered text STARTS with the prefix and the original text is intact
// after it, byte for byte.
func TestSessionStartHintPrepended(t *testing.T) {
	const body = "please review the ingress change\nsecond line kept"
	stub := &stubAdapter{name: "stub"}
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled: true,
		Default: "stub",
		Nudge: harness.NudgeHint{
			SessionStart: true,
			MaxCount:     999,
			Counts: func(context.Context, harness.Message) (int, []string) {
				return 3, []string{"build", "urgent"}
			},
		},
	})
	reg.Register(stub)

	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Message: harness.Message{ID: "m1", SessionID: "ses_1", Text: body},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("delivery must still succeed: %+v", rc)
	}
	prefix := "[courier] 3 unread (build, urgent) — call courier_unread_list to read."
	if !strings.HasPrefix(stub.lastMsg.Text, prefix) {
		t.Fatalf("delivered text must start with the prefix, got %q", stub.lastMsg.Text)
	}
	if got := strings.TrimPrefix(stub.lastMsg.Text, prefix+"\n"); got != body {
		t.Fatalf("the original text must be byte-intact after the prefix:\n got %q\nwant %q", got, body)
	}
}

// TestSessionStartHintDisabled pins §7.8: the shipped default is OFF, so the
// delivered text is byte-identical to a build without this feature — even when
// a Counts hook is wired.
func TestSessionStartHintDisabled(t *testing.T) {
	const body = "untouched \x00 bytes and ; $(id) survive"
	calls := 0
	stub := &stubAdapter{name: "stub"}
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled: true,
		Default: "stub",
		Nudge: harness.NudgeHint{
			SessionStart: false,
			Counts: func(context.Context, harness.Message) (int, []string) {
				calls++
				return 5, []string{"build"}
			},
		},
	})
	reg.Register(stub)

	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Message: harness.Message{ID: "m1", SessionID: "ses_1", Text: body},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("delivery must succeed: %+v", rc)
	}
	if stub.lastMsg.Text != body {
		t.Fatalf("default config must deliver the text unchanged: got %q", stub.lastMsg.Text)
	}
	if calls != 0 {
		t.Fatalf("a disabled hint must not even ask for counts (calls=%d)", calls)
	}
}

// TestSessionStartHintZeroDeliversNoPrefix pins the "no mail, no noise" rule on
// the delivery path: a zero count leaves the text untouched.
func TestSessionStartHintZeroDeliversNoPrefix(t *testing.T) {
	const body = "hello"
	stub := &stubAdapter{name: "stub"}
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled: true,
		Default: "stub",
		Nudge: harness.NudgeHint{
			SessionStart: true,
			Counts: func(context.Context, harness.Message) (int, []string) {
				return 0, nil
			},
		},
	})
	reg.Register(stub)
	reg.Deliver(context.Background(), harness.DispatchRequest{
		Message: harness.Message{ID: "m1", SessionID: "ses_1", Text: body},
	})
	if stub.lastMsg.Text != body {
		t.Fatalf("zero unread must not prefix anything: %q", stub.lastMsg.Text)
	}
}
