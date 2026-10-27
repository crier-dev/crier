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
let outcomes = [];
async function call(path, options) {
  requests.push({path, body:JSON.parse(options.body)});
  if (timeout) { const e = new Error('browser wait elapsed'); e.name = 'TimeoutError'; throw e; }
  return {id:'original', outcomes};
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
  // A confirmed refusal/runtime failure must KEEP the draft and key too;
  // otherwise the next click silently appends another operator row.
  timeout=false; state.namespace='realm';
  for (const outcome of ['refused','failed','budget_cut']) {
    $('text').value='retry negative '+outcome;
    outcomes=[{target:'nimbus',outcome,detail:'first attempt negative'}];
    await send({preventDefault(){}});
    const first=requests.at(-1).body;
    assert.equal($('text').value,'retry negative '+outcome);
    assert.ok(notices.at(-1)[1].includes('outcomes will be replaced'));
    assert.equal(notices.at(-1)[2],true);
    outcomes=[{target:'nimbus',outcome:'delivered'}];
    await send({preventDefault(){}});
    assert.equal(requests.at(-1).body.idempotency_key,first.idempotency_key);
    assert.equal($('text').value,'');
  }
  // Busy is pending, not negative, and must not advise another send or mint a key.
  $('text').value='already in flight';
  outcomes=[{target:'nimbus',outcome:'in_progress',code:'DELIVERY_IN_PROGRESS',retryable:true,detail:'already in progress'}];
  await send({preventDefault(){}});
  const busyKey=requests.at(-1).body.idempotency_key;
  const pending=notices.at(-1);
  assert.match(pending[1],/in_progress: 1/);
  assert.doesNotMatch(pending[1],/not delivered|Send again unchanged|refused/);
  assert.match(pending[1],/Refresh the transcript/);
  assert.match(pending[1],/do not send again/);
  assert.notEqual(pending[2],true);
  assert.equal($('text').value,'already in flight');
  await send({preventDefault(){}});
  assert.equal(requests.at(-1).body.idempotency_key,busyKey);
  console.log('composer timeout and retry-key behavior PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "behavior PASS")
}

// DF-CRIER-467: an explicit-task send used to reach POST /tasks with NO
// idempotency_key, so the progress panel had nothing to identify this request's
// record by and fell back to an unrelated transcript message. The shipped send
// function is executed here for the task path: a task must carry its own 32-hex
// key, keep it across a lost-response retry (POST /tasks replays a matching key
// instead of minting a second task), and release it once the server answers.
func TestChatTaskSendCarriesRequestKey(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded composer")
	}
	source := string(chatClientHTML)
	start := strings.Index(source, "let pendingMessageRequest = null;")
	end := strings.Index(source, `$("add-participant-form").addEventListener`)
	require.Greater(t, start, 0)
	require.Greater(t, end, start)
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
  return {id:'task-message', outcomes:[]};
}
` + source[start:end] + `
(async () => {
  $('intent').value = 'task'; $('recipient').value = 'nimbus'; $('text').value = 'do the work';
  await send({preventDefault(){}});
  const first = requests.at(-1);
  assert.equal(first.path, '/sessions/room/tasks', 'an explicit task goes to the task route');
  assert.match(first.body.idempotency_key, /^[0-9a-f]{32}$/, 'an explicit task carries its own request key');
  const taskKey = first.body.idempotency_key;
  await send({preventDefault(){}});
  assert.equal(requests.at(-1).body.idempotency_key, taskKey, 'a lost task response retry replays the same task record');
  timeout = false;
  await send({preventDefault(){}});
  assert.equal(requests.at(-1).body.idempotency_key, taskKey, 'the answered retry keeps reporting its own record');
  $('text').value = 'do the work';
  await send({preventDefault(){}});
  assert.notEqual(requests.at(-1).body.idempotency_key, taskKey, 'an answered task releases its key: an unchanged repeat is a new task');
  console.log('task request-key behavior PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "task request-key behavior PASS")
}
