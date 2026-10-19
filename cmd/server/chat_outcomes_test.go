package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatOutcomeRenderingDistinguishesPending(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded renderer")
	}
	source := string(chatClientHTML)
	renderStart, renderEnd := strings.Index(source, "function renderMessages("), strings.Index(source, "function renderParticipants(")
	detailStart, detailEnd := strings.Index(source, "function outcomeLabel("), strings.Index(source, "async function send(")
	require.Greater(t, renderStart, 0)
	require.Greater(t, renderEnd, renderStart)
	require.Greater(t, detailStart, 0)
	require.Greater(t, detailEnd, detailStart)
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const transcript={scrollHeight:0,scrollTop:0,clientHeight:100,innerHTML:''};
const $=()=>transcript;
const esc=v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;');
const avatar=()=>''; const author=()=> 'operator'; const stamp=()=>''; const messageText=()=> 'test fixture';
const state={thread:'',messages:[]};
` + source[renderStart:renderEnd] + source[detailStart:detailEnd] + `
const outcomes=[
 {target:'receiver',outcome:'in_progress',code:'DELIVERY_IN_PROGRESS',retryable:true,detail:'a delivery with this idempotency_key is already in progress'},
 {target:'receiver',outcome:'refused',code:'TARGET_HTTP_503',retryable:true,detail:'target answered status 503; 2 attempts, 1.2s delivery budget exhausted'},
 {target:'receiver',outcome:'budget_cut',code:'DELIVERY_BUDGET_CUT',retryable:true,detail:'answer did not arrive inside the budget: 1 attempt, 200ms delivery budget exhausted; partial or late answer discarded'},
];
state.messages=outcomes.map((o,i)=>({id:String(i),seq:i+1,thread_id:String(i),message_kind:'plain',outcomes:[o]}));
renderMessages();
assert.ok(transcript.innerHTML.includes('<span class="warning">receiver: in progress — a delivery'));
assert.ok(transcript.innerHTML.includes('<span class="error">receiver: delivery refused — target answered status 503;'));
assert.ok(transcript.innerHTML.includes('<span class="error">receiver: budget_cut — answer did not arrive inside the budget:'));
assert.equal(outcomeLabel({outcome:'refused',code:'AGENT_NOT_FOUND',detail:'absent'}),'no such agent (465)');
assert.equal(deliverySummary([outcomes[0]]),'; delivered to 0 recipient(s); 1 recipient(s) in progress.');
assert.equal(deliverySummary([outcomes[0],outcomes[1],{outcome:'delivered'}]),'; delivered to 1 recipient(s); 1 recipient(s) in progress; 1 recipient(s) not delivered.');
console.log('pending/refused/cut rendered line and summary PASS');
`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "summary PASS")
}
