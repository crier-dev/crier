package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatMessageTimeoutKeepsRequestKey(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded composer")
	}
	source := string(chatClientHTML)
	start := strings.Index(source, "let pendingMessageRequest = null;")
	end := strings.Index(source, `$("add-participant-form").addEventListener`)
	require.Greater(t, start, 0)
	require.Greater(t, end, start)
	// Execute the shipped send function, not a hand-copied timeout/key model.
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const crypto = require('node:crypto').webcrypto;
const elements = new Map();
const $ = id => { if (!elements.has(id)) elements.set(id, {value:'', hidden:true}); return elements.get(id); };
const state = {current:'room', namespace:'realm', agent:'atlas', reply:null, sending:false, generation:0};
const notices = [];
const notice = (...args) => notices.push(args);
const composerReason = () => '';
const updateComposer = () => {};
const resetAudienceWarning = () => {};
const renderReply = () => {};
const refresh = async () => {};
const sessionPath = (id, kind) => '/sessions/'+id+'/'+kind;
const requests = [];
let timeout = true;
async function call(path, options) {
  requests.push({path, body:JSON.parse(options.body)});
  if (timeout) { const e = new Error('browser wait elapsed'); e.name = 'TimeoutError'; throw e; }
  return {id:'original', outcomes:[]};
}
` + source[start:end] + `
(async () => {
  $('text').value = 'slow message'; $('recipient').value = 'nimbus'; $('intent').value = 'message';
  await send({preventDefault(){}});
  assert.equal(requests.length,1);
  const key = requests[0].body.idempotency_key;
  assert.match(key,/^[0-9a-f]{32}$/);
  assert.equal($('text').value, 'slow message');
  const timeoutNotice = notices.find(n => n[0] === 'send-result' && n[1].includes('Still delivering'));
  assert.ok(timeoutNotice, 'timeout must explain delivery remains unconfirmed');
  assert.notEqual(timeoutNotice[2],true, 'timeout is not a failed-send verdict');
  timeout = false;
  await send({preventDefault(){}});
  assert.equal(requests[1].body.idempotency_key,key, 'lost response retry must reuse key');
  assert.equal($('text').value,'');
  $('text').value = 'slow message';
  await send({preventDefault(){}});
  assert.notEqual(requests[2].body.idempotency_key,key, 'confirmed send starts a new request');
  timeout = true; $('text').value='changed draft';
  await send({preventDefault(){}});
  const changedKey=requests[3].body.idempotency_key;
  assert.notEqual(changedKey,key);
  state.namespace='other realm';
  await send({preventDefault(){}});
  assert.notEqual(requests[4].body.idempotency_key,changedKey,'realm change must get a new key');
  console.log('composer timeout and retry-key behavior PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "behavior PASS")
}
