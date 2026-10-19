package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatMemberDeliveryDiagnostics(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required for the embedded client behavior test")
	source := string(chatClientHTML)
	if path := os.Getenv("DF347_CHAT_CONTROL"); path != "" {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		source = string(b)
	}
	start := strings.Index(source, "let pendingMessageRequest = null;")
	end := strings.Index(source, `$("add-participant-form").addEventListener`)
	require.Greater(t, start, 0)
	require.Greater(t, end, start)
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert=require('node:assert/strict');
const crypto=require('node:crypto').webcrypto;
const elements=new Map();
const $=id=>{if(!elements.has(id))elements.set(id,{value:'',hidden:true});return elements.get(id)};
const state={current:'room',namespace:'',agent:'operator',reply:null,sending:false,generation:0};
const notices=[];const notice=(...args)=>notices.push(args);
const composerReason=()=>'';const updateComposer=()=>{};const resetAudienceWarning=()=>{};const renderReply=()=>{};const refresh=async()=>{};
const sessionPath=(id,kind)=>'/sessions/'+id+'/'+kind;
let outcomes=[];async function call(){return {outcomes}}
` + source[start:end] + `
(async()=>{
  $('recipient').value='echo-agent';$('intent').value='message';$('text').value='hello';
  outcomes=[{target:'echo-agent',outcome:'delivered'},{target:'ghost',outcome:'refused',detail:'no such agent "ghost" (AGENT_NOT_FOUND): agent not found: "ghost"'}];
  await send({preventDefault(){}});
  const partial=notices.at(-1)[1];
  assert.equal(partial,'Message recorded; delivered: 1; refused: 1.\necho-agent: delivered\nghost: no such agent (465) — no such agent "ghost" (AGENT_NOT_FOUND): agent not found: "ghost"');
  assert.ok(!partial.includes('delivery refused'));
  assert.equal($('text').value,'','partial success must not invite duplicate delivery');
  $('text').value='again';outcomes=[{target:'echo-agent',outcome:'refused',detail:'delivery failed (403): policy denied'}];
  await send({preventDefault(){}});
  assert.ok(notices.at(-1)[1].includes('echo-agent: delivery refused — delivery failed (403): policy denied'));
  assert.ok(!notices.at(-1)[1].includes('no such agent'));
  assert.equal($('text').value,'again');
  assert.equal(outcomeLine({target:'operator',outcome:'refused',detail:'resolve delivery target "operator": agent not found: "operator"'}),'operator: no such agent (465) — resolve delivery target "operator": agent not found: "operator"');
  assert.equal(outcomeLine({target:'echo-agent',outcome:'delivered'}),'echo-agent: delivered');
  console.log(partial);console.log('member diagnostics PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Log(string(output))
	require.Contains(t, string(output), "member diagnostics PASS")
	// Both transcript re-reads and immediate send notices must use one formatter.
	require.Contains(t, source, "esc(outcomeLine(o))")
	require.Contains(t, source, "outcomes.map(outcomeLine)")
	require.Contains(t, source, `notice("participant-result", e.message, true)`)
}
