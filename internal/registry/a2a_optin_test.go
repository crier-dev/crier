package registry

// INT-A2A-001 — the per-agent A2A opt-in block (specs/A2A-OPTION.md §4.2).
//
// These tests cover the WIRE contract of the optional `a2a` object on a
// registry row: the strict member decode (a misnamed key is a 400 naming the
// key, never a silently dropped opt-in), the round-trip through the real HTTP
// handlers, the three-state PATCH rule, and the omitempty promise that an
// agent registered without the block is byte-identical to a pre-A2A row.
//
// Nothing here asserts A2A BEHAVIOUR: this row ships no surface that consumes
// the block (spec §5).

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
)

// a2aAcceptedKeys is the member list the strict a2a decode prints on a
// rejection, in Config declaration order.
const a2aAcceptedKeys = `enabled`

// preA2AConfigKeys is the registration response's key set for an agent that
// carries no optional config at all — the shape that existed before this row,
// SORTED (the comparisons below sort both sides). Pinned literally: it is the
// contract the a2a block must not disturb.
var preA2AConfigKeys = []string{
	"capabilities", "id", "last_seen", "public_key", "registered_at", "status",
}

// agentKeysOf returns the sorted top-level key set of a decoded agent body.
func agentKeysOf(t *testing.T, body string) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode agent body %q: %v", body, err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// a2aOf returns the agent's serialized a2a member and whether it was present —
// the state every refusal and every PATCH must leave exactly as it found it.
func a2aOf(t *testing.T, r *mux.Router, id string) (json.RawMessage, bool) {
	t.Helper()
	code, body := doJSON(t, r, http.MethodGet, "/agents/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET /agents/%s = %d: %s", id, code, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode agent %q: %v", body, err)
	}
	raw, present := fields["a2a"]
	return raw, present
}

// TestRegister_A2AMemberStrictness: POST /agents refuses an unknown or misnamed
// key inside the a2a object — naming the key and the accepted keys — registers
// nothing on the refusal, and accepts every legal shape.
func TestRegister_A2AMemberStrictness(t *testing.T) {
	cases := []struct {
		name        string
		a2a         string // raw a2a member; "" = no a2a member at all
		wantErr     string // non-empty ⇒ expect 400 with exactly this error
		wantCode    int
		wantPresent bool // on 201: the response must carry the a2a member
	}{
		{
			name:        "valid opt-in registers",
			a2a:         `{"enabled":true}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:        "empty block registers — the zero value is off",
			a2a:         `{}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:        "explicit false registers",
			a2a:         `{"enabled":false}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:        "explicit null registers as no block",
			a2a:         `null`,
			wantCode:    http.StatusCreated,
			wantPresent: false,
		},
		{
			name:        "member name matches case-insensitively, like the decoder",
			a2a:         `{"ENABLED":true}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:    "misnamed enabled is refused",
			a2a:     `{"enabld":true}`,
			wantErr: `a2a: unknown field "enabld" (accepted: ` + a2aAcceptedKeys + `)`,
		},
		{
			name:    "an unknown key alongside the declared one is refused",
			a2a:     `{"enabled":true,"card":"x"}`,
			wantErr: `a2a: unknown field "card" (accepted: ` + a2aAcceptedKeys + `)`,
		},
		{
			// A block that is not an object cannot even be represented in the
			// request type, so the OUTER decode refuses it first — the same
			// 400-with-"invalid json" an ill-typed `webhook` member gets. What
			// matters for this row is the pair of properties the subtest
			// asserts below for every rejection: the request is refused, and
			// nothing was registered.
			name:    "a non-object block is refused",
			a2a:     `true`,
			wantErr: `invalid json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hexKey, _ := newTestPubKey(t)
			r := newStrictWebhookRouter()
			body := `{"id":"agent-1","public_key":"` + hexKey + `","capabilities":["relay"]`
			if tc.a2a != "" {
				body += `,"a2a":` + tc.a2a
			}
			body += `}`

			code, respBody := doJSON(t, r, http.MethodPost, "/agents", body)
			if tc.wantErr != "" {
				if code != http.StatusBadRequest {
					t.Fatalf("POST /agents = %d: %s; want 400", code, respBody)
				}
				if got := errorOf(t, respBody); got != tc.wantErr {
					t.Fatalf("error = %q\nwant      %q", got, tc.wantErr)
				}
				// A refused registration must not have registered anything —
				// and, crucially, must not have silently dropped the block and
				// registered the agent anyway (the defect the strict scan
				// exists for).
				if got, _ := doJSON(t, r, http.MethodGet, "/agents/agent-1", ""); got != http.StatusNotFound {
					t.Errorf("agent was registered despite the refusal: GET /agents/agent-1 = %d", got)
				}
				return
			}

			if code != tc.wantCode {
				t.Fatalf("POST /agents = %d: %s; want %d", code, respBody, tc.wantCode)
			}
			raw, present := a2aOf(t, r, "agent-1")
			if present != tc.wantPresent {
				t.Errorf("stored agent a2a present = %v, want %v (%s)", present, tc.wantPresent, raw)
			}
		})
	}
}

// TestRegister_A2ABlockRoundTripsWithCoreFieldsUnchanged is deliverable 4's
// round-trip: an agent registered WITH the block and one WITHOUT both read back
// with identical core fields, and only the opted-in agent carries the member.
func TestRegister_A2ABlockRoundTripsWithCoreFieldsUnchanged(t *testing.T) {
	r := newStrictWebhookRouter()
	hexKey, _ := newTestPubKey(t)

	withBlock := `{"id":"opted-in","public_key":"` + hexKey + `","capabilities":["relay","solver"],"a2a":{"enabled":true}}`
	withoutBlock := `{"id":"plain","public_key":"` + hexKey + `","capabilities":["relay","solver"]}`

	for id, body := range map[string]string{"opted-in": withBlock, "plain": withoutBlock} {
		if code, respBody := doJSON(t, r, http.MethodPost, "/agents", body); code != http.StatusCreated {
			t.Fatalf("POST /agents (%s) = %d: %s", id, code, respBody)
		}
	}

	// Core fields are identical and unchanged by the presence of the block.
	core := func(id string) map[string]json.RawMessage {
		code, body := doJSON(t, r, http.MethodGet, "/agents/"+id, "")
		require.Equal(t, http.StatusOK, code, "GET /agents/%s: %s", id, body)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(body), &fields))
		for _, key := range []string{"a2a", "webhook", "guard"} {
			delete(fields, key)
		}
		delete(fields, "registered_at")
		delete(fields, "last_seen")
		fields["id"] = json.RawMessage(`"«id»"`)
		return fields
	}
	optedIn, plain := core("opted-in"), core("plain")
	if !reflect.DeepEqual(optedIn, plain) {
		t.Errorf("core fields differ between an agent with an a2a block and one without:\n with = %v\nwithout = %v", optedIn, plain)
	}

	// Only the opted-in agent carries the member, and it is the block that was
	// sent — not a coerced or enriched shape.
	raw, present := a2aOf(t, r, "opted-in")
	if !present {
		t.Fatal("the opted-in agent's a2a block did not survive the round trip")
	}
	if string(raw) != `{"enabled":true}` {
		t.Errorf("stored a2a block = %s, want {\"enabled\":true}", raw)
	}
	if raw, present := a2aOf(t, r, "plain"); present {
		t.Errorf("an agent registered without an a2a block carries one anyway: %s", raw)
	}
}

// TestRegister_WithoutA2ABlockKeepsThePreA2AShape pins the byte-level promise:
// registering with no optional config produces exactly the key set it produced
// before this option existed. A new required key, a serialized empty block or a
// renamed field would all fail here.
func TestRegister_WithoutA2ABlockKeepsThePreA2AShape(t *testing.T) {
	r := newStrictWebhookRouter()
	hexKey, _ := newTestPubKey(t)

	code, body := doJSON(t, r, http.MethodPost, "/agents", `{"id":"plain","public_key":"`+hexKey+`","capabilities":[]}`)
	require.Equal(t, http.StatusCreated, code, body)

	keys := agentKeysOf(t, body)
	if !reflect.DeepEqual(keys, preA2AConfigKeys) {
		t.Fatalf("registration response keys = %v, want %v (the pre-A2A shape; the a2a block must be omitted when absent)", keys, preA2AConfigKeys)
	}

	code, got := doJSON(t, r, http.MethodGet, "/agents/plain", "")
	require.Equal(t, http.StatusOK, code, got)
	readKeys := agentKeysOf(t, got)
	if !reflect.DeepEqual(readKeys, preA2AConfigKeys) {
		t.Fatalf("GET /agents/plain keys = %v, want %v", readKeys, preA2AConfigKeys)
	}
}

// TestUpdate_A2AThreeState: PATCH follows the webhook object's rule — present
// replaces, explicit null clears, absent leaves the agent's opt-in untouched.
func TestUpdate_A2AThreeState(t *testing.T) {
	r := newStrictWebhookRouter()
	hexKey, _ := newTestPubKey(t)

	code, body := doJSON(t, r, http.MethodPost, "/agents", `{"id":"a1","public_key":"`+hexKey+`","capabilities":[],"a2a":{"enabled":true}}`)
	require.Equal(t, http.StatusCreated, code, body)

	// 1. absent member → the block is untouched.
	code, body = doJSON(t, r, http.MethodPatch, "/agents/a1", `{"capabilities":["relay"]}`)
	require.Equal(t, http.StatusOK, code, body)
	if raw, present := a2aOf(t, r, "a1"); !present || string(raw) != `{"enabled":true}` {
		t.Errorf("PATCH without an a2a member changed the block: present=%v raw=%s", present, raw)
	}

	// 2. present member → replaces the block (false is a rewrite, not a clear).
	code, body = doJSON(t, r, http.MethodPatch, "/agents/a1", `{"a2a":{"enabled":false}}`)
	require.Equal(t, http.StatusOK, code, body)
	if raw, present := a2aOf(t, r, "a1"); !present || string(raw) != `{}` {
		t.Errorf("PATCH with an a2a block = present=%v raw=%s, want present with {}", present, raw)
	}

	// 3. explicit null → clears the block (opt back out without re-registering).
	code, body = doJSON(t, r, http.MethodPatch, "/agents/a1", `{"a2a":null}`)
	require.Equal(t, http.StatusOK, code, body)
	if raw, present := a2aOf(t, r, "a1"); present {
		t.Errorf("PATCH {\"a2a\":null} did not clear the block: %s", raw)
	}

	// ...and a cleared block can be re-added by the same three-state rule.
	code, body = doJSON(t, r, http.MethodPatch, "/agents/a1", `{"a2a":{"enabled":true}}`)
	require.Equal(t, http.StatusOK, code, body)
	if raw, present := a2aOf(t, r, "a1"); !present || string(raw) != `{"enabled":true}` {
		t.Errorf("re-adding the block failed: present=%v raw=%s", present, raw)
	}
}

// TestUpdate_A2AMalformedLeavesAgentUntouched: a malformed block on PATCH is a
// 400 that changes nothing — not the block, not the capabilities riding in the
// same body.
func TestUpdate_A2AMalformedLeavesAgentUntouched(t *testing.T) {
	r := newStrictWebhookRouter()
	hexKey, _ := newTestPubKey(t)

	code, body := doJSON(t, r, http.MethodPost, "/agents", `{"id":"a1","public_key":"`+hexKey+`","capabilities":["relay"],"a2a":{"enabled":true}}`)
	require.Equal(t, http.StatusCreated, code, body)

	code, body = doJSON(t, r, http.MethodPatch, "/agents/a1", `{"capabilities":["solver"],"a2a":{"mode":"on"}}`)
	require.Equal(t, http.StatusBadRequest, code, body)
	if got, want := errorOf(t, body), `a2a: unknown field "mode" (accepted: `+a2aAcceptedKeys+`)`; got != want {
		t.Fatalf("PATCH error = %q, want %q", got, want)
	}

	_, got := doJSON(t, r, http.MethodGet, "/agents/a1", "")
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got), &fields))
	if string(fields["a2a"]) != `{"enabled":true}` {
		t.Errorf("a rejected PATCH changed the a2a block: %s", fields["a2a"])
	}
	var caps []string
	require.NoError(t, json.Unmarshal(fields["capabilities"], &caps))
	if !reflect.DeepEqual(caps, []string{"relay"}) {
		t.Errorf("a rejected PATCH changed capabilities: %v", caps)
	}
}

// TestAgent_A2ASerializationOmitempty is the store-level half of the shape
// promise: the zero value serializes without the member, a set block with it.
func TestAgent_A2ASerializationOmitempty(t *testing.T) {
	plain, err := json.Marshal(&Agent{ID: "a", Capabilities: []string{}})
	require.NoError(t, err)
	if keys := agentKeysOf(t, string(plain)); !reflect.DeepEqual(keys, preA2AConfigKeys) {
		t.Fatalf("Agent without an a2a block serialized as %v, want %v", keys, preA2AConfigKeys)
	}

	optedIn, err := json.Marshal(&Agent{ID: "a", Capabilities: []string{}, A2A: &a2a.Config{Enabled: true}})
	require.NoError(t, err)
	if keys := agentKeysOf(t, string(optedIn)); !reflect.DeepEqual(keys, append([]string{"a2a"}, preA2AConfigKeys...)) {
		t.Fatalf("Agent with an a2a block serialized as %v, want a2a plus %v", keys, preA2AConfigKeys)
	}
	if !strings.Contains(string(optedIn), `"a2a":{"enabled":true}`) {
		t.Fatalf("serialized a2a block = %s, want {\"enabled\":true}", optedIn)
	}
}
