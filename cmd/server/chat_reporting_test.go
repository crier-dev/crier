package main

import (
	"encoding/json"
	"os/exec"
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
			require.Equal(t, true, body["a2a_enabled"])
			require.Equal(t, true, body["auth_enabled"])
		})
	}
	var defaults map[string]any
	require.NoError(t, json.Unmarshal(httptestRecorder(t, config.Config{}, "memory"), &defaults))
	require.Equal(t, "jsonl", defaults["session_backend"])
	require.Equal(t, false, defaults["a2a_enabled"])
	require.Equal(t, false, defaults["auth_enabled"])
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
