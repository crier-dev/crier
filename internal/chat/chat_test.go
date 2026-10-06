package chat

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		explicit Kind
		want     Kind
		wantErr  error
	}{
		{"plain body", "hello world", "", KindPlain, nil},
		{"tag body", "@a please look", "", KindAddressed, nil},
		{"explicit task wins", "@a do it", KindTask, KindTask, nil},
		{"explicit task no tags", "do it", KindTask, KindTask, nil},
		{"explicit plain", "@a hi", KindPlain, KindPlain, nil},
		{"explicit addressed", "hi", KindAddressed, KindAddressed, nil},
		{"invalid explicit kind", "hi", Kind("bogus"), "", ErrInvalidKind},
		{"instruction body with tag is still addressed", "please fix the build now", "", KindPlain, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify(tc.body, tc.explicit)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseAddressees(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"no tags here", nil},
		{"@a hello", []string{"a"}},
		{"hey @bob.smith and @x-1_2!", []string{"bob.smith", "x-1_2"}},
		{"@a @a @b", []string{"a", "b"}},
		{"email no@pe@example.com", []string{"pe", "example.com"}},
		{"punct stops it: @a, @b.", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := ParseAddressees(tc.body)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseAddressees(%q) = %v, want %v", tc.body, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseAddressees(%q) = %v, want %v", tc.body, got, tc.want)
			}
		}
	}
}

// A tag must be structurally incapable of creating work: constructing an
// ADDRESSED message with any task state is an error, and tag syntax alone
// never yields a TASK.
func TestTagNeverCreatesWork(t *testing.T) {
	for _, ts := range []TaskState{TaskOpen, TaskClaimed, TaskRunning, TaskDone, TaskFailed} {
		_, err := NewMessage(KindAddressed, "u", "t", "@a please fix X", []string{"a"}, ts)
		if !errors.Is(err, ErrTaskStateOnNonTask) {
			t.Fatalf("addressed with task state %q: err = %v, want ErrTaskStateOnNonTask", ts, err)
		}
	}
	_, err := NewMessage(KindPlain, "u", "t", "hi", nil, TaskOpen)
	if !errors.Is(err, ErrTaskStateOnNonTask) {
		t.Fatalf("plain with task state: err = %v, want ErrTaskStateOnNonTask", err)
	}

	// Tag syntax alone (no explicit kind) classifies as ADDRESSED, never TASK.
	k, err := Classify("@a please fix X", "")
	if err != nil || k != KindAddressed {
		t.Fatalf("tag body classified as %q err=%v, want addressed/nil", k, err)
	}

	// Constructing a TASK defaults its state to open.
	m, err := NewMessage(KindTask, "u", "t", "@a do X", nil, "")
	if err != nil {
		t.Fatalf("task construct: %v", err)
	}
	if m.TaskState != TaskOpen {
		t.Fatalf("task default state = %q, want open", m.TaskState)
	}
}

func TestTransition(t *testing.T) {
	newTask := func(state TaskState) Message {
		m, err := NewMessage(KindTask, "u", "t", "body", nil, state)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		return m
	}

	// Legal path.
	m := newTask("")
	for _, step := range []TaskState{TaskClaimed, TaskRunning, TaskDone} {
		next, err := Transition(m, step)
		if err != nil {
			t.Fatalf("transition to %q: %v", step, err)
		}
		m = next
	}

	// Illegal: open→done.
	if _, err := Transition(newTask(TaskOpen), TaskDone); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("open→done: err = %v, want ErrInvalidTransition", err)
	}
	// Backwards: running→claimed.
	if _, err := Transition(newTask(TaskRunning), TaskClaimed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("running→claimed: err = %v, want ErrInvalidTransition", err)
	}
	// Terminal: done→anything.
	if _, err := Transition(newTask(TaskDone), TaskFailed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("done→failed: err = %v, want ErrInvalidTransition", err)
	}
	// Same-state.
	if _, err := Transition(newTask(TaskOpen), TaskOpen); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("open→open: err = %v, want ErrInvalidTransition", err)
	}
	// Invalid target value.
	if _, err := Transition(newTask(TaskOpen), TaskState("bogus")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("open→bogus: err = %v, want ErrInvalidTransition", err)
	}
	// Non-task kinds cannot be transitioned.
	addr, err := NewMessage(KindAddressed, "u", "t", "@a", []string{"a"}, "")
	if err != nil {
		t.Fatalf("construct addressed: %v", err)
	}
	if _, err := Transition(addr, TaskOpen); !errors.Is(err, ErrNotATask) {
		t.Fatalf("transition addressed: err = %v, want ErrNotATask", err)
	}
	plain, err := NewMessage(KindPlain, "u", "t", "hi", nil, "")
	if err != nil {
		t.Fatalf("construct plain: %v", err)
	}
	if _, err := Transition(plain, TaskOpen); !errors.Is(err, ErrNotATask) {
		t.Fatalf("transition plain: err = %v, want ErrNotATask", err)
	}
}

func TestWireRoundTrip(t *testing.T) {
	cases := []Message{
		{Kind: KindPlain, From: "u", Thread: "t", Body: "hello"},
		{Kind: KindAddressed, From: "u", Thread: "t", Body: "@a hi", Addressees: []string{"a"}},
		{Kind: KindTask, From: "u", Thread: "t", Body: "do it", TaskState: TaskOpen},
	}
	for _, m := range cases {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %s: %v", m.Kind, err)
		}
		// The kind field is never omitted.
		var probe map[string]any
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatal(err)
		}
		if _, ok := probe["kind"]; !ok {
			t.Fatalf("kind field missing for %s: %s", m.Kind, data)
		}
		var back Message
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", m.Kind, err)
		}
		if back.Kind != m.Kind || back.From != m.From || back.Body != m.Body || back.TaskState != m.TaskState {
			t.Fatalf("round trip mismatch for %s: %+v vs %+v", m.Kind, back, m)
		}
	}
}

func TestUnmarshalMessageRejectsBadKind(t *testing.T) {
	if _, err := UnmarshalMessage([]byte(`{"kind":"bogus"}`)); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("err = %v, want ErrInvalidKind", err)
	}
}

func TestRenderTranscript(t *testing.T) {
	plain, _ := NewMessage(KindPlain, "alice", "t", "hello", nil, "")
	addr, _ := NewMessage(KindAddressed, "alice", "t", "@a,@b look", []string{"a", "b"}, "")
	task, _ := NewMessage(KindTask, "alice", "t", "do it", nil, TaskOpen)
	done, _ := Transition(task, TaskClaimed)
	done, _ = Transition(done, TaskRunning)
	done, _ = Transition(done, TaskDone)

	got := RenderTranscript(plain)
	if got != "alice: hello" {
		t.Fatalf("plain render = %q", got)
	}
	got = RenderTranscript(addr)
	if got != "alice → a,b: @a,@b look" {
		t.Fatalf("addressed render = %q", got)
	}
	for _, sub := range []string{"[TASK open]", "alice"} {
		if !contains(RenderTranscript(task), sub) {
			t.Fatalf("task render %q missing %q", RenderTranscript(task), sub)
		}
	}
	if !contains(RenderTranscript(done), "[TASK done]") {
		t.Fatalf("done render %q missing [TASK done]", RenderTranscript(done))
	}
	// The three renders are pairwise distinct.
	rs := []string{RenderTranscript(plain), RenderTranscript(addr), RenderTranscript(task)}
	if rs[0] == rs[1] || rs[1] == rs[2] || rs[0] == rs[2] {
		t.Fatalf("renders not distinct: %v", rs)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestNewMessageValidation(t *testing.T) {
	if _, err := NewMessage(Kind("bogus"), "u", "t", "b", nil, ""); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("err = %v, want ErrInvalidKind", err)
	}
	if _, err := NewMessage(KindTask, "u", "t", "b", nil, TaskState("bogus")); !errors.Is(err, ErrInvalidTaskState) {
		t.Fatalf("err = %v, want ErrInvalidTaskState", err)
	}
	// Addressees only on ADDRESSED.
	if _, err := NewMessage(KindPlain, "u", "t", "b", []string{"a"}, ""); !errors.Is(err, ErrAddresseesOnNonAddressed) {
		t.Fatalf("err = %v, want ErrAddresseesOnNonAddressed", err)
	}
	if _, err := NewMessage(KindTask, "u", "t", "b", []string{"a"}, ""); !errors.Is(err, ErrAddresseesOnNonAddressed) {
		t.Fatalf("err = %v, want ErrAddresseesOnNonAddressed", err)
	}
	// ADDRESSED with addressees is fine.
	if _, err := NewMessage(KindAddressed, "u", "t", "@a", []string{"a"}, ""); err != nil {
		t.Fatalf("addressed construct: %v", err)
	}
}
