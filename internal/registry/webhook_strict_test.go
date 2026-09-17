package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// strictAcceptedKeys is the member list the strict webhook decode prints on a
// rejection, in Config declaration order (DF-CRIER-150).
const strictAcceptedKeys = `url, auth_type, auth_value_ref, schema_template, custom_schema, delivery_mode, batch, retries, timeout_ms`

// newStrictWebhookRouter wires the three surfaces these tests need: register,
// read-back, and update. Webhook configs are only validated where the source
// accepts them, so the checks all run through the real HTTP handlers.
func newStrictWebhookRouter() *mux.Router {
	store := NewMemoryStore()
	h := NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods("POST")
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", h.HandleUpdateAgent).Methods("PATCH")
	return r
}

func doJSON(t *testing.T, r *mux.Router, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// errorOf extracts the {"error": "..."} body the handlers answer with.
func errorOf(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return out.Error
}

// webhookOf returns the agent's serialized webhook as raw JSON plus whether
// the member was present at all — the state a rejected request must leave
// untouched.
func webhookOf(t *testing.T, r *mux.Router, id string) (json.RawMessage, bool) {
	t.Helper()
	code, body := doJSON(t, r, http.MethodGet, "/agents/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET /agents/%s = %d: %s", id, code, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode agent %q: %v", body, err)
	}
	raw, present := fields["webhook"]
	return raw, present
}

// TestRegister_WebhookMemberStrictness: POST /agents refuses an unknown or
// misnamed key inside the webhook object — naming the key and the accepted
// keys — and registers nothing on the refusal; valid objects still register.
func TestRegister_WebhookMemberStrictness(t *testing.T) {
	cases := []struct {
		name        string
		webhook     string // raw webhook member; "" = no webhook member at all
		wantErr     string // non-empty ⇒ expect 400 with exactly this error
		wantCode    int
		wantPresent bool // on 201: the response must carry the webhook member
	}{
		{
			name:        "valid blocking object registers",
			webhook:     `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"blocking"}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:        "valid object with nested config registers",
			webhook:     `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"batch","batch":{"max_messages":5,"flush_interval_s":1},"custom_schema":{"response_map":"raw"}}`,
			wantCode:    http.StatusCreated,
			wantPresent: true,
		},
		{
			name:    "misnamed mode is refused",
			webhook: `{"url":"http://127.0.0.1:18971/hook","mode":"blocking"}`,
			wantErr: `webhook: unknown field "mode" (accepted: ` + strictAcceptedKeys + `)`,
		},
		{
			name:    "typo alongside a valid key is refused",
			webhook: `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"blocking","retrys":3}`,
			wantErr: `webhook: unknown field "retrys" (accepted: ` + strictAcceptedKeys + `)`,
		},
		{
			name:    "unknown key nested in batch is refused",
			webhook: `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"batch","batch":{"max_msgs":5}}`,
			wantErr: `webhook.batch: unknown field "max_msgs" (accepted: max_messages, flush_interval_s)`,
		},
		{
			name:        "no webhook member still registers",
			wantCode:    http.StatusCreated,
			wantPresent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hexKey, _ := newTestPubKey(t)
			r := newStrictWebhookRouter()
			body := `{"id":"agent-1","public_key":"` + hexKey + `","capabilities":["relay"]`
			if tc.webhook != "" {
				body += `,"webhook":` + tc.webhook
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
				// A refused registration must not have registered anything.
				if got, _ := doJSON(t, r, http.MethodGet, "/agents/agent-1", ""); got != http.StatusNotFound {
					t.Errorf("agent was registered despite the refusal: GET /agents/agent-1 = %d", got)
				}
				return
			}

			if code != tc.wantCode {
				t.Fatalf("POST /agents = %d: %s; want %d", code, respBody, tc.wantCode)
			}
			var reg map[string]json.RawMessage
			if err := json.Unmarshal([]byte(respBody), &reg); err != nil {
				t.Fatalf("decode registration body %q: %v", respBody, err)
			}
			if _, present := reg["webhook"]; present != tc.wantPresent {
				t.Errorf("registration response webhook present = %v, want %v: %s", present, tc.wantPresent, respBody)
			}
			if raw, present := webhookOf(t, r, "agent-1"); present != tc.wantPresent {
				t.Errorf("stored agent webhook present = %v, want %v (%s)", present, tc.wantPresent, raw)
			}
		})
	}
}

// TestRegister_TopLevelObjectStaysPermissive: strictness is scoped to the
// webhook object. An unknown key at the top level, and an unknown key inside
// the guard object, must still register — those surfaces are deliberately out
// of scope for this row.
func TestRegister_TopLevelObjectStaysPermissive(t *testing.T) {
	hexKey, _ := newTestPubKey(t)
	r := newStrictWebhookRouter()
	body := `{"id":"agent-1","public_key":"` + hexKey + `","surprise_top_level":true,` +
		`"guard":{"policies":[{"id":"p1"}],"unknown_guard_key":"x"},` +
		`"webhook":{"url":"http://127.0.0.1:18971/hook","delivery_mode":"blocking"}}`
	code, respBody := doJSON(t, r, http.MethodPost, "/agents", body)
	if code != http.StatusCreated {
		t.Fatalf("POST /agents with out-of-scope extra keys = %d: %s; want 201", code, respBody)
	}
	if raw, present := webhookOf(t, r, "agent-1"); !present || !strings.Contains(string(raw), "blocking") {
		t.Errorf("webhook not registered as sent: present=%v raw=%s", present, raw)
	}
}

// TestPatch_WebhookMemberStrictness: PATCH refuses the same misnamed keys and
// leaves the agent's stored webhook byte-identical, while valid objects update
// it and an absent-or-null member still removes it.
func TestPatch_WebhookMemberStrictness(t *testing.T) {
	hexKey, _ := newTestPubKey(t)
	r := newStrictWebhookRouter()

	registered := `{"id":"agent-1","public_key":"` + hexKey + `",` +
		`"webhook":{"url":"http://127.0.0.1:18971/hook","delivery_mode":"blocking","retries":2}}`
	if code, body := doJSON(t, r, http.MethodPost, "/agents", registered); code != http.StatusCreated {
		t.Fatalf("setup register = %d: %s", code, body)
	}
	before, present := webhookOf(t, r, "agent-1")
	if !present {
		t.Fatal("setup: agent registered without a webhook")
	}

	refusals := []struct {
		name string
		body string
		want string
	}{
		{
			name: "misnamed mode",
			body: `{"webhook":{"url":"http://127.0.0.1:18971/other","mode":"async"}}`,
			want: `webhook: unknown field "mode" (accepted: ` + strictAcceptedKeys + `)`,
		},
		{
			name: "unknown key nested in batch",
			body: `{"webhook":{"url":"http://127.0.0.1:18971/other","delivery_mode":"batch","batch":{"max_msgs":2}}}`,
			want: `webhook.batch: unknown field "max_msgs" (accepted: max_messages, flush_interval_s)`,
		},
		{
			name: "unknown key nested in custom_schema",
			body: `{"webhook":{"url":"http://127.0.0.1:18971/other","custom_schema":{"request_shape":{"bodd":{}}}}}`,
			want: `webhook.custom_schema.request_shape: unknown field "bodd" (accepted: method, headers, body)`,
		},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			code, body := doJSON(t, r, http.MethodPatch, "/agents/agent-1", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("PATCH = %d: %s; want 400", code, body)
			}
			if got := errorOf(t, body); got != tc.want {
				t.Fatalf("error = %q\nwant      %q", got, tc.want)
			}
			after, present := webhookOf(t, r, "agent-1")
			if !present {
				t.Fatal("rejected PATCH removed the webhook")
			}
			if string(after) != string(before) {
				t.Fatalf("rejected PATCH modified the webhook:\nbefore %s\nafter  %s", before, after)
			}
		})
	}

	t.Run("valid object updates", func(t *testing.T) {
		body := `{"webhook":{"url":"http://127.0.0.1:18971/updated","delivery_mode":"batch","batch":{"max_messages":3,"flush_interval_s":1}}}`
		code, respBody := doJSON(t, r, http.MethodPatch, "/agents/agent-1", body)
		if code != http.StatusOK {
			t.Fatalf("PATCH = %d: %s; want 200", code, respBody)
		}
		var reg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(respBody), &reg); err != nil {
			t.Fatalf("decode PATCH body %q: %v", respBody, err)
		}
		if !strings.Contains(string(reg["webhook"]), `"delivery_mode":"batch"`) {
			t.Errorf("PATCH response webhook = %s", reg["webhook"])
		}
		after, present := webhookOf(t, r, "agent-1")
		if !present || !strings.Contains(string(after), "updated") {
			t.Errorf("stored webhook after update: present=%v raw=%s", present, after)
		}
	})

	t.Run("explicit null removes", func(t *testing.T) {
		code, respBody := doJSON(t, r, http.MethodPatch, "/agents/agent-1", `{"webhook":null}`)
		if code != http.StatusOK {
			t.Fatalf("PATCH webhook:null = %d: %s; want 200", code, respBody)
		}
		if raw, present := webhookOf(t, r, "agent-1"); present {
			t.Errorf("explicit null left a webhook behind: %s", raw)
		}
	})

	t.Run("absent removes (unchanged semantics)", func(t *testing.T) {
		code, respBody := doJSON(t, r, http.MethodPatch, "/agents/agent-1",
			`{"webhook":{"url":"http://127.0.0.1:18971/again","delivery_mode":"async"}}`)
		if code != http.StatusOK {
			t.Fatalf("setup PATCH = %d: %s", code, respBody)
		}
		if _, present := webhookOf(t, r, "agent-1"); !present {
			t.Fatal("setup PATCH did not install a webhook")
		}
		code, respBody = doJSON(t, r, http.MethodPatch, "/agents/agent-1", `{"capabilities":["relay"]}`)
		if code != http.StatusOK {
			t.Fatalf("PATCH without a webhook member = %d: %s; want 200", code, respBody)
		}
		if raw, present := webhookOf(t, r, "agent-1"); present {
			t.Errorf("webhook absent from the PATCH body left a webhook behind: %s", raw)
		}
	})
}
