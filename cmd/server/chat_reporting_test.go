package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestStatusChatPosture(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "postgres", "future"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.Config{AuthToken: "test-only", A2AEnabled: true, Session: config.SessionConfig{Backend: "sqlite"}}
			var body map[string]any
			require.NoError(t, json.Unmarshal(httptestRecorder(t, cfg, backend), &body))
			require.Contains(t, body, "registry_durable")
			switch backend {
			case "memory":
				require.Equal(t, false, body["registry_durable"])
			case "sqlite", "postgres":
				require.Equal(t, true, body["registry_durable"])
			default:
				require.Nil(t, body["registry_durable"], "unrecognized backend durability is unknown")
			}
			require.Equal(t, "sqlite", body["session_backend"])
			require.NotContains(t, body, "a2a_enabled", "the A2A switch is deliberately absent from /status (A2A-OPTION.md 6.1)")
			require.Equal(t, true, body["auth_enabled"])
		})
	}
	var defaults map[string]any
	require.NoError(t, json.Unmarshal(httptestRecorder(t, config.Config{}, "memory"), &defaults))
	require.Equal(t, "jsonl", defaults["session_backend"])
	require.NotContains(t, defaults, "a2a_enabled", "the A2A switch is deliberately absent from /status (A2A-OPTION.md 6.1)")
	require.Equal(t, false, defaults["auth_enabled"])
}

// TestChatPageCarriesA2ASwitch pins the RE-SOURCING of the panel's A2A
// indicator (DF-CRIER-494). specs/A2A-OPTION.md §6.1 keeps the option's own
// state off GET /status ("the switch is deliberately absent from /status"), so
// the /chat posture sentence takes it from the page the server SERVES: the
// handler resolves one authored seam per boot with config.A2AEnabled. This test
// drives the REAL handler over a recorder, asserts the resolved page carries the
// boot's switch (and no longer the authored seam), asserts /status carries no
// a2a-prefixed key in the same boot, and then executes the shipped renderPosture
// on that page to prove the sentence reads enabled/disabled from the page and is
// NOT moved by an /status body that happens to carry the old key.
func TestChatPageCarriesA2ASwitch(t *testing.T) {
	node := nodeOrSkip(t)
	marker := chatA2ASwitchMarker
	require.Equal(t, 1, strings.Count(string(chatClientHTML), marker),
		"the authored page must carry exactly one A2A switch injection seam")

	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{"switch off (default)", false, "A2A: disabled"},
		{"switch on (CR_A2A_ENABLED=true)", true, "A2A: enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleChatClient(tc.enabled)(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
			require.Equal(t, http.StatusOK, rec.Code)
			page := rec.Body.String()
			require.Contains(t, page, "const servedA2AEnabled = "+strconv.FormatBool(tc.enabled)+";",
				"the served page must carry the boot's A2A switch")
			require.NotContains(t, page, marker,
				"the served page must not retain the authored seam")

			// The same boot's /status: no a2a-prefixed key, raw body included.
			status := httptestRecorder(t, config.Config{A2AEnabled: tc.enabled}, registryBackendMemory)
			require.NotContains(t, string(status), "a2a",
				"/status must never carry the switch (A2A-OPTION.md 6.1)")

			// Execute the SHIPPED renderPosture from the SERVED page, with the
			// real /status body, then again with the switch's OPPOSITE injected
			// into that body: the sentence must not move.
			a := strings.Index(page, "function renderPosture(")
			b := strings.Index(page, "async function refresh()")
			require.Greater(t, b, a)
			opposite := strconv.FormatBool(!tc.enabled)
			cmd := exec.Command(node, "--input-type=commonjs")
			cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const lines = {};
const notice = (id, text) => { lines[id] = text; };
` + page[a:b] + `
const status = ` + string(status) + `;
renderPosture(status);
assert.ok(lines.posture.includes(` + strconv.Quote(tc.want) + `), 'served page must report the boot switch: '+lines.posture);
renderPosture({...status, a2a_enabled: ` + opposite + `});
assert.ok(lines.posture.includes(` + strconv.Quote(tc.want) + `), 'an /status a2a key must not move the sentence: '+lines.posture);
console.log('served A2A switch rendered PASS');
`)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			require.Contains(t, string(out), "served A2A switch rendered PASS")
		})
	}
}

// Execute the embedded production functions, not copies of their predicates.
func TestChatClientOutcomeReporting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded chat client")
	}
	source := string(chatClientHTML)
	extract := func(start, end string) string {
		a, b := strings.Index(source, start), strings.Index(source, end)
		require.Greater(t, b, a)
		return source[a:b]
	}
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const elements = new Map();
const $ = id => { if (!elements.has(id)) elements.set(id,{value:'',textContent:'',className:'',innerHTML:'',scrollHeight:0,scrollTop:0,clientHeight:100}); return elements.get(id); };
const state = {current:'room',agent:'operator',namespace:'',session:{state:'open'},reply:'parent',generation:0,messages:[],thread:''};
const esc = String, author = () => 'operator', stamp = String, avatar = () => 'OP', messageText = m => m.payload.text;
const composerReason = () => '', updateComposer = () => {}, resetAudienceWarning = () => {}, renderReply = () => {}, refresh = async () => {};
const notice = (id,text,error=false) => { $(id).textContent=text; $(id).className=error?'note error':'note'; };
const crypto = require('node:crypto').webcrypto;
const sessionPath = (id,suffix) => '/sessions/'+id+'/'+suffix;
let outcomes;
const call = async () => ({outcomes});
` + extract("function renderMessages(", "function renderParticipants(") + extract("let pendingMessageRequest = null;", "$(\"add-participant-form\").addEventListener") + `
(async () => {
  for (const outcome of ['delivered','accepted','held','leased','acked','expired','refused','failed','canned','budget_cut','unreadable_reply','future']) {
    outcomes = [{target:'agent',outcome}];
    const summary = deliverySummary(outcomes);
    assert.equal(summary, outcome === 'accepted' ? '; accepted: 1; delivered: 0.' : outcome === 'held' ? '; delivered: 0; held: 1.' : '; '+outcome+': 1.', outcome+' must retain its own summary vocabulary');
    $('text').value = 'keep this draft'; $('recipient').value = 'agent'; $('intent').value = 'message';
    state.reply = 'parent'; pendingMessageRequest = null;
    await send({preventDefault(){}});
    const negative = ['refused','failed','canned','budget_cut','unreadable_reply'].includes(outcome);
    assert.equal($('send-result').className.includes('error'),negative,outcome+' notice style');
    assert.equal($('text').value,negative?'keep this draft':'',outcome+' draft retention');
    if (negative) assert.equal(state.reply,'parent',outcome+' reply context retained');
    if (['canned','unreadable_reply'].includes(outcome)) {
      assert.equal(pendingMessageRequest,null,'non-retryable non-answer must not gain a retry grant');
      assert.match($('send-result').textContent,/Draft kept/);
      assert.doesNotMatch($('send-result').textContent,/Send again unchanged to retry/);
    }
    state.messages = [{id:'m',thread_id:'m',message_kind:'plain',author:{agent:'operator'},payload:{text:'test'},outcomes}];
    renderMessages();
    assert.ok($('transcript').innerHTML.includes('<span class="'+(negative?'error':'note')+'">agent:'),outcome+' transcript style');
  }
  assert.equal(deliverySummary([{outcome:'leased'},{outcome:'expired'},{outcome:'delivered'},{outcome:'leased'}]),'; delivered: 1; expired: 1; leased: 2.');
  assert.equal(deliverySummary([{outcome:'delivered'},{outcome:'accepted'},{outcome:'accepted'},{outcome:'held'}]),'; accepted: 2; delivered: 1; held: 1.');
  // An accepted receipt can also be an unsubstantiated delivery claim. Keep
  // the exact accepted counter and the visible diagnostic, never a retry grant.
  outcomes = [{target:'peer',outcome:'accepted',code:'PEER_CLAIMED_DELIVERY',retryable:false,detail:'peer claimed delivery but no reply observed; transport accepted, not confirmed execution'}];
  $('text').value='claim probe'; $('recipient').value='peer';
  await send({preventDefault(){}});
  assert.match($('send-result').textContent,/; accepted: 1; delivered: 0\./);
  assert.match($('send-result').textContent,/peer claimed delivery but no reply observed/);
  assert.doesNotMatch($('send-result').textContent,/Send again unchanged to retry/);
  assert.equal(pendingMessageRequest,null);
  assert.equal($('text').value,'');
  outcomes = [{target:'a',outcome:'delivered'},{target:'b',outcome:'unreadable_reply'}];
  $('text').value='mixed draft'; await send({preventDefault(){}});
  assert.equal($('text').value,'mixed draft','mixed non-answer retains draft without enabling redelivery');
  assert.equal(pendingMessageRequest,null);
  console.log('outcome reporting PASS');
})().catch(e => { console.error(e); process.exitCode=1; });
`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "outcome reporting PASS")
}
