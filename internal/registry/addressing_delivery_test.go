package registry

// CR-CHAT-004 acceptance A6 — the addressing "to line" on the REAL send path.
//
// The row's deliverable on this surface is minimal and additive: a send that
// carries a MALFORMED addressing field is refused with the named
// 400 INVALID_ADDRESS (naming the offending token and the rule), and a send
// that carries NO addressing field is byte-identically what it was before the
// field existed.
//
// Two properties beyond the refusal are asserted here because they are the
// spec's whole point and a regression would be silent:
//
//   - a well-formed address list is ACCEPTED and nothing else happens: the
//     delivery reaches its target and no other agent (D12/§2.5 — a tag is
//     addressing, never an instruction, so there is no fan-out and nothing
//     executes), and
//   - the parser's shape checks (one `#` room, the 64-address bound) are
//     enforced on this path, not just in the package's unit tests.
//
// Every request goes through a gorilla mux router wired to the same handler
// cmd/server registers, exactly as internal/registry/permissions_test.go does,
// so nothing here re-implements a code path.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/addressing"
)

// addressingTest wires POST /agents/{id}/inbox — the shipped send path — over
// a fresh in-memory store.
type addressingTest struct {
	handler *Handler
	router  *mux.Router
	store   *MemoryStore
}

func newAddressingTest(t *testing.T) *addressingTest {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	return &addressingTest{handler: h, router: r, store: store}
}

// queueDepth reads an agent's durable inbox depth without leasing anything.
func (a *addressingTest) queueDepth(t *testing.T, agentID string) int {
	t.Helper()
	depth, leased, _, err := a.store.Stats(agentID)
	require.NoError(t, err)
	require.Zero(t, leased, "no test here leases a message")
	return depth
}

func (a *addressingTest) post(t *testing.T, agentID, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doRequest(t, a.router, http.MethodPost, "/agents/"+agentID+"/inbox", body)
	return rec, decodeJSONBody(t, rec)
}

// TestDeliverAddressing_MalformedRefusedNamed is A6's first half: the send is
// refused with the NAMED error and NOTHING is stored, and no other agent
// receives anything.
func TestDeliverAddressing_MalformedRefusedNamed(t *testing.T) {
	tests := []struct {
		name        string
		addressing  string
		wantReason  string
		wantAddress string
	}{
		{"empty_group_name", "@team:", addressing.RuleEmptyGroupName, "@team:"},
		{"empty_capability_name", "@cap:", addressing.RuleEmptyCapabilityName, "@cap:"},
		{"empty_namespace", "@ns/", addressing.RuleEmptyNamespace, "@ns/"},
		{"bare_sigil", "@", addressing.RuleEmptyToken, "@"},
		{"bad_charset", "@ghost!", addressing.RuleBadTokenCharset, "@ghost!"},
		{"no_sigil", "@atlas, atlas", addressing.RuleNotAnAddress, "atlas"},
		{"trailing_separator", "@atlas,", addressing.RuleTrailingSeparator, "@atlas,"},
		{"two_rooms", "#one #two", addressing.RuleMultipleSessions, "#two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAddressingTest(t)
			capabilityHolder(t, a.store, "quill")

			body := `{"payload":{"text":"hi"},"addressing":` + jsonString(tc.addressing) + `}`
			rec, got := a.post(t, "quill", body)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body = %s", rec.Body.String())
			require.Equal(t, "INVALID_ADDRESS", got["error"], "a malformed address is the named refusal, got %v", got)
			require.Equal(t, tc.wantReason, got["reason"], "the refusal must name the rule")
			require.Equal(t, tc.wantAddress, got["address"], "the refusal must echo the offending token")
			require.NotEmpty(t, got["detail"], "the §4 body carries a detail")
			require.Zero(t, a.queueDepth(t, "quill"), "a refused delivery stores nothing")
		})
	}
}

// TestDeliverAddressing_MalformedRefusedViaHeader is the same refusal when the
// value arrives as the X-Crier-Addressing header rather than the body field.
func TestDeliverAddressing_MalformedRefusedViaHeader(t *testing.T) {
	a := newAddressingTest(t)
	capabilityHolder(t, a.store, "quill")

	req := httptest.NewRequest(http.MethodPost, "/agents/quill/inbox", strings.NewReader(`{"payload":{"text":"hi"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderAddressing, "@team:")
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)

	got := decodeJSONBody(t, rec)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "INVALID_ADDRESS", got["error"])
	require.Equal(t, addressing.RuleEmptyGroupName, got["reason"])
	require.Zero(t, a.queueDepth(t, "quill"))
}

// TestDeliverAddressing_TooManyRefusedNamesLimitAndCount pins the bound at the
// HTTP boundary: a 65-address list is refused (never truncated) and the body
// names both the limit and the count received.
func TestDeliverAddressing_TooManyRefusedNamesLimitAndCount(t *testing.T) {
	a := newAddressingTest(t)
	capabilityHolder(t, a.store, "quill")

	tooMany := strings.TrimSuffix(strings.Repeat("@a,", addressing.MaxAddressesPerWrite+1), ",")
	rec, got := a.post(t, "quill", `{"payload":{"text":"hi"},"addressing":`+jsonString(tooMany)+`}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "INVALID_ADDRESS", got["error"])
	require.Equal(t, addressing.RuleTooManyAddresses, got["reason"])
	detail, _ := got["detail"].(string)
	require.Contains(t, detail, "64")
	require.Contains(t, detail, "65")
	require.Zero(t, a.queueDepth(t, "quill"))
}

// TestDeliverAddressing_WellFormedAcceptedNoFanOut is A6's second half plus
// D12: a well-formed list is accepted, the target's inbox gets the message,
// and an agent NAMED in the addressing field that is not the path target
// receives nothing — the tag did not fan out because nothing executes a tag.
func TestDeliverAddressing_WellFormedAcceptedNoFanOut(t *testing.T) {
	a := newAddressingTest(t)
	capabilityHolder(t, a.store, "quill")
	capabilityHolder(t, a.store, "atlas") // named in `addressing`, must receive nothing

	rec, got := a.post(t, "quill", `{"payload":{"text":"hi"},"addressing":"#build-plan @atlas @cap:research @ns/acme/*"}`)

	require.Equal(t, http.StatusCreated, rec.Code, "body = %s", rec.Body.String())
	require.Equal(t, "inbox", got["transport"])
	require.Equal(t, 1, a.queueDepth(t, "quill"), "the target received the message")
	require.Zero(t, a.queueDepth(t, "atlas"), "D12: an addressed tag is not a delivery — no fan-out")
}

// TestDeliverAddressing_AbsentFieldUnchanged is A6's "still succeeds" half and
// the additive contract: the same request without the field is accepted
// exactly as before CR-CHAT-004, and an EMPTY field behaves like an absent one
// so a client that always sends the key is not broken.
func TestDeliverAddressing_AbsentFieldUnchanged(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"field_absent", `{"payload":{"text":"hi"}}`},
		{"field_empty", `{"payload":{"text":"hi"},"addressing":""}`},
		{"field_whitespace", `{"payload":{"text":"hi"},"addressing":"   "}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAddressingTest(t)
			capabilityHolder(t, a.store, "quill")

			rec, got := a.post(t, "quill", tc.body)
			require.Equal(t, http.StatusCreated, rec.Code, "body = %s", rec.Body.String())
			require.Equal(t, "inbox", got["transport"])
			require.NotContains(t, got, "error")
			require.Equal(t, 1, a.queueDepth(t, "quill"))
		})
	}
}

// jsonString encodes s as a JSON string literal for embedding in a request
// body. The addressing fixtures contain only ASCII, so a hand-rolled quoter
// keeps the test free of a second encoder.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
