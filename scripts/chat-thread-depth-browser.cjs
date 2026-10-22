#!/usr/bin/env node
// Real Chromium and a production binary with isolated scratch JSONL sessions.
// Build: go build -o /tmp/crier-depth-server ./cmd/server
// NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/crier-depth-server node scripts/chat-thread-depth-browser.cjs [evidence-dir]
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const {spawn} = require('node:child_process');
const {chromium} = require('playwright');
(async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'crier-thread-depth-'));
  const evidence = process.argv[2] || dir;
  fs.mkdirSync(evidence,{recursive:true});
  const probe = net.createServer();
  await new Promise(resolve => probe.listen(0,'127.0.0.1',resolve));
  const port = probe.address().port;
  await new Promise(resolve => probe.close(resolve));
  const env = Object.fromEntries(Object.entries(process.env).filter(([k])=>!k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(port),CR_REGISTRY_BACKEND:'memory',CR_SESSION_BACKEND:'jsonl',CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),CR_GROUP_ROOT:path.join(dir,'groups'),CR_REQUIRE_AGENT_SIG:'false',CR_GUARD_ENABLED:'false',CR_RATE_LIMIT_PER_MINUTE:'0'});
  const log=fs.openSync(path.join(dir,'server.log'),'w');
  const proc=spawn(process.env.CRIER_TEST_BINARY || '/tmp/crier-depth-server',[],{cwd:dir,env,stdio:['ignore',log,log]});fs.closeSync(log);
  const base=`http://127.0.0.1:${port}`;
  let browser;
  try {
    let ready=false;
    for(let i=0;i<100;i++) {
      if(proc.exitCode!==null)throw Error(fs.readFileSync(path.join(dir,'server.log'),'utf8'));
      try {if((await fetch(base+'/health')).ok){ready=true;break;}}catch{}
      await new Promise(resolve=>setTimeout(resolve,100));
    }
    assert.ok(ready,'scratch server readiness');assert.equal(proc.exitCode,null);
    async function api(method,route,body) {
      const r=await fetch(base+route,{method,headers:{'Content-Type':'application/json'},body:body && JSON.stringify(body)});
      const b=await r.json();assert.ok(r.ok,`${route}: ${r.status} ${JSON.stringify(b)}`);return b;
    }
    assert.equal((await api('GET','/sessions')).sessions.length,0,'scratch server begins empty');
    await api('POST','/agents',{id:'depth-operator'});
    const room=await api('POST','/sessions',{title:'Depth only where deliberately branched',kind:'channel',created_by:{agent:'depth-operator'},members:[{member_type:'agent',member_id:'depth-operator'}]});
    const post=(parent,text)=>api('POST',`/sessions/${room.id}/messages`,{sender:'depth-operator',parent_id:parent,targets:[{kind:'agent',id:'depth-operator'}],payload:{text}});
    const root=await post('', 'Root thread — ordinary replies remain flat.');
    const r1=await post(root.id,'First flat reply');const r2=await post(r1.id,'Reply to a reply — still depth zero');
    browser=await chromium.launch({executablePath:process.env.CHROME_BIN||'/usr/bin/google-chrome',headless:true,args:['--no-sandbox']});
    const page=await browser.newPage({viewport:{width:1440,height:1100}}), errors=[], posts=[];
    page.on('pageerror',e=>errors.push(e.message));
    page.on('request',r=>{if(r.method()==='POST' && r.url().endsWith('/messages'))posts.push(r.postDataJSON());});
    await page.goto(base+'/chat');
    const idle=()=>page.waitForFunction(()=>!state.busy && !state.sending && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));
    await idle();await page.locator('#agent').fill('depth-operator');await page.locator('#apply-identity').click();await idle();
    await page.locator(`[data-session="${room.id}"]`).click();await idle();
    await page.locator('#recipient').selectOption('depth-operator');
    const chain=[];let parent=r2;
    for(let depth=1;depth<=4;depth++) {
      if(depth===4)await page.locator(`[data-expand-branch="${parent.id}"]`).click();
      await page.locator(`[data-branch="${parent.id}"]`).click();
      await page.locator('#text').fill(`Deliberate branch at depth ${depth}`);
      await page.locator('#send-button').click();await idle();
      assert.equal(posts.at(-1).parent_id,parent.id);assert.equal(posts.at(-1).branch,true);
      const tr=await api('GET',`/sessions/${room.id}/messages`);const child=tr.messages.at(-1);
      assert.equal(child.anchor_message_id,parent.id);assert.equal(child.thread_depth,depth);assert.equal(child.parent_id,undefined);
      chain.push(child);parent=child;
      await page.locator('#thread-filter').selectOption('');
    }
    await post(chain[2].id,'Ordinary reply inside the depth-three branch');
    await page.locator('#refresh').click();await idle();
    // Reset the expansion from the fourth-branch creation through the real selector.
    await page.locator('#collapse-depth').selectOption('2');await page.locator('#collapse-depth').selectOption('3');
    const hidden=page.locator(`[data-expand-branch="${chain[2].id}"]`);
    assert.match(await hidden.textContent(),/^3 messages hidden/);
    const before=await page.locator('#transcript [data-message-id]').count();
    assert.equal(before,5);await page.screenshot({path:path.join(evidence,'chat-depth-collapsed.png')});
    await hidden.click();assert.equal(await page.locator('#transcript [data-message-id]').count()-before,3);
    await page.locator(`#message-${chain[0].id} [data-anchor="${r2.id}"]`).click();
    assert.equal(await page.evaluate(()=>document.activeElement.id),`message-${r2.id}`);
    assert.equal(await page.locator('#branch-rail [aria-current="location"]').getAttribute('data-anchor'),r2.id);
    const tr=await api('GET',`/sessions/${room.id}/messages`);
    assert.equal(tr.messages.find(m=>m.id===r2.id).parent_id,r1.id);
    assert.equal(tr.messages.find(m=>m.id===r2.id).thread_depth,0);
    await page.screenshot({path:path.join(evidence,'chat-depth-expanded.png')});
    await page.locator('#transcript').screenshot({path:path.join(evidence,'chat-depth-detail.png')});
    await page.setViewportSize({width:390,height:844});
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'no mobile horizontal page overflow');
    await page.screenshot({path:path.join(evidence,'chat-depth-mobile.png'),fullPage:true});
    assert.deepEqual(errors,[]);assert.equal(proc.exitCode,null);
    console.log('PASS browser: four deliberate levels via actual Branch/Send controls; API anchors/depth and flat reply chain verified');
    console.log('PASS browser: exactly 3 hidden/revealed messages; exact non-root focus/rail target; desktop/mobile; no page errors');
    console.log('Evidence: '+evidence+'; scratch server log: '+path.join(dir,'server.log'));
  } finally {
    if(browser)await browser.close();
    if(proc.exitCode===null){proc.kill('SIGTERM');await Promise.race([new Promise(resolve=>proc.once('exit',resolve)),new Promise(resolve=>setTimeout(resolve,5000))]);if(proc.exitCode===null)proc.kill('SIGKILL');}
  }
})().catch(e=>{console.error(e);process.exitCode=1;});
