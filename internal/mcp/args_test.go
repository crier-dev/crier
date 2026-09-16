package mcp

// Unit tests for the strict top-level arguments decode (DF-CRIER-190).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeArgs_DeclaredMembersAccepted(t *testing.T) {
	var in RetrieveInboxInput
	if err := decodeArgs(json.RawMessage(`{"agent_id":"a","max_messages":5,"lease_seconds":20}`), &in); err != nil {
		t.Fatalf("decodeArgs rejected declared members: %v", err)
	}
	if in.AgentID != "a" || in.MaxMessages != 5 || in.LeaseSeconds != 20 {
		t.Fatalf("decoded %+v", in)
	}
}

func TestDecodeArgs_EmptyFormsAreEmptyInput(t *testing.T) {
	for _, args := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage("  {}  ")} {
		var in GetMessagesInput
		if err := decodeArgs(args, &in); err != nil {
			t.Errorf("decodeArgs(%q) = %v, want nil", args, err)
		}
	}
}

func TestDecodeArgs_AbsentArgumentsForNoArgumentTools(t *testing.T) {
	for _, args := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		if err := decodeArgs(args, &struct{}{}); err != nil {
			t.Errorf("decodeArgs(%q) into an empty schema = %v, want nil", args, err)
		}
	}
	if err := decodeArgs(json.RawMessage(`{"max":1}`), &struct{}{}); err == nil {
		t.Fatal("a member of another tool's schema was accepted by a tool that declares none")
	}
}

// TestDecodeArgs_CaseInsensitiveMembersStillMatch guards AC4 at the level where
// the risk lives: the strict decode must keep encoding/json's own matching
// rules, so a differently-cased spelling of a DECLARED member keeps working
// exactly as it did before.
func TestDecodeArgs_CaseInsensitiveMembersStillMatch(t *testing.T) {
	var in GetAgentInput
	if err := decodeArgs(json.RawMessage(`{"ID":"agent-1"}`), &in); err != nil {
		t.Fatalf("decodeArgs rejected a declared member in another case: %v", err)
	}
	if in.ID != "agent-1" {
		t.Fatalf("ID = %q", in.ID)
	}
}

func TestDecodeArgs_RejectsUnknownMembersByName(t *testing.T) {
	cases := []struct {
		name string
		args string
		out  any
		want []string
	}{
		{"invented member", `{"id":"x","totally_bogus_key":12345}`, &GetAgentInput{}, []string{"totally_bogus_key"}},
		{"typo", `{"agent_id":"a","max_messges":1}`, &RetrieveInboxInput{}, []string{"max_messges"}},
		{"wrong name", `{"agent_id":"a","payload":{},"timeout_ms":1000}`, &AskAgentInput{}, []string{"timeout_ms"}},
		{"every unknown member, in document order", `{"alpha_bogus":1,"id":"x","zeta_bogus":2}`, &GetAgentInput{}, []string{"alpha_bogus", "zeta_bogus"}},
		{"member of another tool declared by a no-property tool", `{"agent_id":"a"}`, &struct{}{}, []string{"agent_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decodeArgs(json.RawMessage(tc.args), tc.out)
			if err == nil {
				t.Fatalf("decodeArgs(%s) accepted an unknown member", tc.args)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "invalid arguments: unknown argument") {
				t.Errorf("error does not follow the package style: %s", msg)
			}
			prev := -1
			for _, want := range tc.want {
				idx := strings.Index(msg, want)
				if idx < 0 {
					t.Fatalf("error does not name %q: %s", want, msg)
				}
				if idx < prev {
					t.Errorf("members are not reported in document order: %s", msg)
				}
				prev = idx
			}
		})
	}
}

// TestDecodeArgs_PremiseGoDefaultIsSilent pins the premise of the whole task:
// encoding/json's default decode ACCEPTS an unknown member. If a future Go
// release made it strict by default, this test fails and tells the reader the
// strictness tests above are no longer proving anything.
func TestDecodeArgs_PremiseGoDefaultIsSilent(t *testing.T) {
	if err := json.Unmarshal([]byte(`{"id":"x","totally_bogus_key":1}`), &GetAgentInput{}); err != nil {
		t.Fatalf("premise changed: json.Unmarshal now rejects unknown members: %v", err)
	}
	if err := decodeArgs(json.RawMessage(`{"id":"x","totally_bogus_key":1}`), &GetAgentInput{}); err == nil {
		t.Fatal("decodeArgs accepted what json.Unmarshal accepts silently")
	}
}

func TestDecodeArgs_RejectsTrailingData(t *testing.T) {
	for _, args := range []string{
		`{"id":"x"} {"id":"y"}`,
		`{"id":"x"} trailing`,
		`{"id":"x"} 5`,
		`{"id":"x"} []`,
	} {
		t.Run(args, func(t *testing.T) {
			err := decodeArgs(json.RawMessage(args), &GetAgentInput{})
			if err == nil {
				t.Fatalf("decodeArgs(%s) accepted trailing data", args)
			}
			if !strings.Contains(err.Error(), "invalid arguments") {
				t.Errorf("error style: %v", err)
			}
		})
	}
}

func TestDecodeArgs_RejectsNonObjectArguments(t *testing.T) {
	for _, args := range []string{`[]`, `["id"]`, `"id"`, `42`, `true`} {
		t.Run(args, func(t *testing.T) {
			var in GetAgentInput
			err := decodeArgs(json.RawMessage(args), &in)
			if err == nil {
				t.Fatalf("decodeArgs(%s) accepted a non-object arguments value", args)
			}
			if !strings.Contains(err.Error(), "invalid arguments") {
				t.Errorf("error style: %v", err)
			}
		})
	}
}

func TestDecodeArgs_RejectsMalformedJSON(t *testing.T) {
	for _, args := range []string{`{"id":`, `{"id" "x"}`, `{`, `}`} {
		t.Run(args, func(t *testing.T) {
			var in GetAgentInput
			if err := decodeArgs(json.RawMessage(args), &in); err == nil {
				t.Fatalf("decodeArgs(%s) accepted malformed JSON", args)
			}
		})
	}
}

// TestDecodeArgs_LeavesOpaqueValuesUntouched is the helper-level half of AC3:
// nested values reach the handler byte-for-byte, with every unknown key intact.
func TestDecodeArgs_LeavesOpaqueValuesUntouched(t *testing.T) {
	t.Run("raw payload", func(t *testing.T) {
		const payload = `{"unknown_inside_payload": {"deep": [1, 2, 3]}, "kind": "task"}`
		var in DeliverMessageInput
		if err := decodeArgs(json.RawMessage(`{"agent_id":"a","payload":`+payload+`}`), &in); err != nil {
			t.Fatalf("decodeArgs: %v", err)
		}
		if string(in.Payload) != payload {
			t.Fatalf("payload was not passed through byte-for-byte:\n got %s\nwant %s", in.Payload, payload)
		}
	})

	t.Run("payload map", func(t *testing.T) {
		var in AskAgentInput
		if err := decodeArgs(json.RawMessage(`{"agent_id":"a","payload":{"unknown_inside_payload":{"deep":1}},"timeout_s":2}`), &in); err != nil {
			t.Fatalf("decodeArgs: %v", err)
		}
		if _, ok := in.Payload["unknown_inside_payload"]; !ok {
			t.Fatalf("nested unknown key was dropped: %+v", in.Payload)
		}
		if in.TimeoutS != 2 {
			t.Fatalf("timeout_s = %d", in.TimeoutS)
		}
	})

	t.Run("mesh body", func(t *testing.T) {
		var in MeshRequestInput
		if err := decodeArgs(json.RawMessage(`{"target":"a","method":"PING","path":"/p","body":{"unknown_inside_body":{"deep":1}}}`), &in); err != nil {
			t.Fatalf("decodeArgs: %v", err)
		}
		body, ok := in.Body.(map[string]any)
		if !ok {
			t.Fatalf("body = %T", in.Body)
		}
		if _, ok := body["unknown_inside_body"]; !ok {
			t.Fatalf("nested unknown key was dropped: %+v", body)
		}
	})

	t.Run("capabilities and nested arrays", func(t *testing.T) {
		var in RegisterAgentInput
		args := `{"id":"a","public_key":"` + strings.Repeat("ab", 32) + `","capabilities":["coding","has_unknown_tag_label"]}`
		if err := decodeArgs(json.RawMessage(args), &in); err != nil {
			t.Fatalf("decodeArgs: %v", err)
		}
		if len(in.Capabilities) != 2 || in.Capabilities[1] != "has_unknown_tag_label" {
			t.Fatalf("capabilities = %v", in.Capabilities)
		}
	})
}
