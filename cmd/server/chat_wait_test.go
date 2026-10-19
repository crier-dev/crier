package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatSessionWaitDoesNotCancelServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded HTTP client")
	}
	source := string(chatClientHTML)
	start, end := strings.Index(source, "async function call("), strings.Index(source, "function notice(")
	require.Greater(t, end, start)
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert=require('node:assert/strict');
const state={agent:'operator',namespace:''};
const errorText=()=>'';
let expire, options, finish, drained=false, cleared=false;
const setTimeout=(fn,ms)=>{assert.equal(ms,12000);expire=fn;return 42;};
const clearTimeout=id=>{assert.equal(id,42);cleared=true;};
const fetch=async (path,opts)=>{
  options=opts;
  await new Promise(resolve=>finish=resolve);
  return {ok:true,text:async()=>{drained=true;return '{"id":"original"}';}};
};
` + source[start:end] + `
(async()=>{
  const pending=call('/sessions/room/messages',{method:'POST'},true);
  assert.equal(options.signal,undefined,'UI wait must not cancel the server HTTP context');
  expire();
  await assert.rejects(pending,{name:'TimeoutError'});
  assert.equal(cleared,true);
  finish();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(drained,true,'response must still be read after the UI stops waiting');
  const get=call('/sessions');
  assert.ok(options.signal instanceof AbortSignal,'ordinary GET keeps its abort timeout');
  finish();
  assert.deepEqual(await get,{id:'original'});
  console.log('wait-only timeout PASS; server request remains attached and body drains');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "wait-only timeout PASS")
}
