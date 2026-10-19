#!/usr/bin/env node
// CR-CHAT-038: real Chromium + stock embedded server. All seeded records are
// confined to temporary test storage. No fixtures are shipped in chat.html.
// Usage: NODE_PATH=/tmp/crier-browser/node_modules node scripts/chat-ui-browser.cjs
// Install the harness dependency outside the repo: npm install --prefix /tmp/crier-browser playwright
// Build first: go build -o /tmp/cr-chat-038-server ./cmd/server
// CHAT_TEST_PORT optionally pins the first scratch listener (e.g. 18874);
// CRIER_TEST_BINARY selects the rebuilt binary, also used for both agent keygens.
// CHAT_MATRIX_ONLY=1 runs just the DF-CRIER-360 registry/matrix browser regressions.
const assert = require('node:assert/strict');
const {spawn,spawnSync} = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const {randomBytes} = require('node:crypto');
const {pathToFileURL} = require('node:url');
// Unique by default: a failed repeat must not leave a previous PASS report.
const evidence = process.env.CHAT_EVIDENCE || fs.mkdtempSync(path.join(os.tmpdir(),'cr-chat-browser-evidence-'));
fs.mkdirSync(evidence,{recursive:true});
const apiChecks = [], browserChecks = [];
function verified(action) { browserChecks.push(action); console.log('BROWSER VERIFIED: '+action); }
function skip(reason) { console.error('SKIP: '+reason+'; exit code 77 (no browser assertions ran)'); process.exitCode=77; }
const processes = [], tempdirs = [];
let browser;
async function freePort() {
  const server = net.createServer();
  await new Promise((resolve,reject) => server.once('error',reject).listen(0,'127.0.0.1',resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}
async function start(token = '') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'cr-chat-038-')); tempdirs.push(dir);
  const port = processes.length === 0 && process.env.CHAT_TEST_PORT ? Number(process.env.CHAT_TEST_PORT) : await freePort();
  // Binding must succeed before spawning, so readiness cannot measure a stranger.
  const probe = net.createServer();
  await new Promise((resolve,reject) => probe.once('error',reject).listen(port,'127.0.0.1',resolve));
  await new Promise(resolve => probe.close(resolve));
  // Keep only host/toolchain essentials. Ambient production CR_* config must
  // never bind a test to a real database, permission log or peer.
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(port),CR_SESSION_BACKEND:'jsonl',CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),CR_GROUP_ROOT:path.join(dir,'groups'),CR_AUTH_TOKEN:token,CR_GUARD_ENABLED:'false',CR_RATE_LIMIT_PER_MINUTE:'0'});
  const log = fs.openSync(path.join(dir,'server.log'),'w');
  const proc = spawn(process.env.CRIER_TEST_BINARY || '/tmp/cr-chat-038-server',[],{cwd:dir,env,stdio:['ignore',log,log]}); processes.push(proc); fs.closeSync(log);
  const url = 'http://127.0.0.1:' + port;
  for (let i=0;i<100;i++) {
    if (proc.exitCode !== null) throw new Error('Scratch server exited: ' + fs.readFileSync(path.join(dir,'server.log'),'utf8'));
    try { if ((await fetch(url+'/health')).ok) { assert.equal(proc.exitCode,null); return url; } } catch {}
    await new Promise(r => setTimeout(r,100));
  }
  throw new Error('Scratch server readiness failed: ' + dir);
}
async function api(base, method, endpoint, body, headers = {}) {
  const response = await fetch(base+endpoint,{method,headers:{'Content-Type':'application/json',...headers},body:body === undefined ? undefined : JSON.stringify(body)});
  const raw = await response.text();
  const json = raw ? JSON.parse(raw) : null;
  assert.ok(response.ok, method+' '+endpoint+' '+response.status+' '+JSON.stringify(json));
  apiChecks.push({method,url:base+endpoint,status:response.status});
  console.log('API VERIFIED (not a browser action): '+method+' '+base+endpoint+' -> '+response.status);
  return json;
}
async function waitIdle(page) { await page.waitForFunction(() => !state.sending && !state.busy && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh')); }
async function refresh(page) { await page.locator('#refresh').click(); await waitIdle(page); }
async function run() {
  let chromium;
  try { require.resolve('playwright'); }
  catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error;
    skip('Playwright is not installed; npm install --prefix /tmp/crier-browser playwright and set NODE_PATH=/tmp/crier-browser/node_modules');
    return;
  }
  ({chromium} = require('playwright'));
  const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
  try { fs.accessSync(executablePath,fs.constants.X_OK); }
  catch (error) { skip('Chromium executable unavailable at '+executablePath+' ('+error.code+'); set CHROME_BIN to an executable browser'); return; }
  // Launch before creating any scratch records; launch crashes are FAIL, not SKIP.
  browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  console.log('BROWSER: real headless Chromium at '+executablePath);
  console.log('FIXTURES: temporary JSONL sessions, in-memory registry, generated agents/messages; no production records.');
  console.log('INJECTIONS: matrix registry/hostile data, storage denial, UI-state gates, canned HTTP faults and delayed reads are test-only, not live server behavior.');
  console.log('LIMITATIONS: no agent runtime executes the task; completion uses explicit browser fetch, not an evidence-entry UI (none exists). Protected /chat checks are HTTP fetches, not browser sign-in.');
  const base = await start();
  // These are deliberately identified as test records, not demo product data.
  const operator = 'ui-test-operator', agent = 'ui-test-recipient';
  const keyDir = fs.mkdtempSync(path.join(os.tmpdir(),'cr-chat-keys-')); tempdirs.push(keyDir);
  let keypair, public_key;
  for (const id of [operator,agent]) {
    const keyFile = path.join(keyDir,id+'.pem');
    const generated = spawnSync(process.env.CRIER_TEST_BINARY || '/tmp/cr-chat-038-server',['keygen','-out',keyFile,'-id',id,'-server',base,'-json'],{encoding:'utf8'});
    assert.equal(generated.status,0,generated.stderr || generated.stdout);
    const config = JSON.parse(generated.stdout);
    await api(base,'POST','/agents',{...JSON.parse(config.registration),capabilities:['ui-test-analysis']});
    if (id === agent) { public_key = config.public_key; keypair = {privateKey:require('node:crypto').createPrivateKey(fs.readFileSync(keyFile))}; }
  }
  const session = await api(base,'POST','/sessions',{title:'Browser verification channel',kind:'channel',created_by:{agent:operator},members:[{member_type:'agent',member_id:operator},{member_type:'agent',member_id:agent}]});
  const message = await api(base,'POST',`/sessions/${session.id}/messages`,{sender:operator,payload:{text:'Initial request for the browser test'},message_kind:'plain'});
  const other = await api(base,'POST',`/sessions/${session.id}/messages`,{sender:operator,payload:{text:'Independent second thread'},message_kind:'plain'});
  // A long unbroken body and malicious-looking markup must remain readable text.
  const long = 'UNBROKEN_'+'x'.repeat(800)+'\n<img src=x onerror="window.__injected=true">';
  await api(base,'POST',`/sessions/${session.id}/messages`,{sender:agent,parent_id:message.id,payload:{text:long},message_kind:'addressed'});
  const context = await browser.newContext({viewport:{width:1280,height:1000}});
  const page = await context.newPage(); const errors = []; page.on('pageerror',e => errors.push(e.message));
  await page.goto(base+'/chat'); await waitIdle(page);
  assert.equal(await page.locator('[data-ui-revision]').getAttribute('data-ui-revision'),'CR-CHAT-038','/chat UI revision must be CR-CHAT-038');
  assert.equal(await page.locator('#send-button').isDisabled(),true);
  await require('./chat-capability-matrix.cjs')(page,evidence);
  verified(base+'/chat: revision CR-CHAT-038, identity-gated send; declared-capability matrix (intercepted registry fixtures)');
  if (process.env.CHAT_MATRIX_ONLY === '1') { console.log('PASS: MATRIX ONLY; task lifecycle and all remaining full-suite assertions NOT RUN; exit code 0'); return; }
  // DF-CRIER-332: no silent persistence; exactly one opted-in attribution slot.
  const storage = () => page.evaluate(() => Object.fromEntries(Object.entries(sessionStorage)));
  const attributionKey = 'crier.chat.attribution-id';
  assert.equal(await page.locator('#identity-gate').isVisible(),true);
  assert.equal(await page.locator('#remember-agent').isChecked(),false);
  assert.deepEqual(await storage(),{});
  await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await waitIdle(page);
  assert.deepEqual(await storage(),{});
  await page.reload(); await waitIdle(page);
  assert.equal(await page.locator('#identity-gate').isVisible(),true);
  await page.locator('#remember-agent').check();
  assert.equal(await page.locator('#remember-agent').isChecked(),true);
  assert.deepEqual(await storage(),{}); // checking alone never saves a draft ID
  await page.locator('#agent').fill('  '+operator+'  '); await page.locator('#namespace').fill('default'); await page.locator('#apply-identity').click(); await waitIdle(page);
  assert.deepEqual(await storage(),{[attributionKey]:operator});
  await page.reload(); await waitIdle(page);
  assert.equal(await page.locator('#agent').inputValue(),operator);
  assert.equal(await page.locator('#namespace').inputValue(),''); // no realm persisted
  assert.equal(await page.locator('#identity-gate').isVisible(),false);
  assert.equal(await page.locator('#remember-agent').isChecked(),true);
  assert.match(await page.locator('#remember-status').innerText(),/Attribution ID restored/);
  assert.equal(await page.evaluate(() => state.agent),operator);
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle(page);
  assert.equal(await page.locator('#text').isDisabled(),false);
  await page.screenshot({path:path.join(evidence,'chat-attribution-restored.png'),fullPage:true});
  await page.locator('.identity').screenshot({path:path.join(evidence,'chat-attribution-detail.png')});
  // Independent tab has no opt-in (browser opener/duplicate-tab copying is outside this guarantee).
  const freshTab = await context.newPage(); await freshTab.goto(base+'/chat'); await waitIdle(freshTab);
  assert.equal(await freshTab.locator('#identity-gate').isVisible(),true); await freshTab.close();
  await page.locator('#forget-agent').click();
  assert.deepEqual(await storage(),{});
  assert.equal(await page.locator('#remember-agent').isChecked(),false);
  assert.equal(await page.evaluate(() => state.agent),operator); // forget is not logout
  await page.reload(); await waitIdle(page);
  assert.equal(await page.locator('#identity-gate').isVisible(),true);
  await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await waitIdle(page);
  await page.locator('#remember-agent').check(); assert.deepEqual(await storage(),{[attributionKey]:operator});
  await page.locator('#remember-agent').uncheck(); assert.deepEqual(await storage(),{});
  // Storage denial is a disclosed degradation, not a broken composer or login.
  const denied = await browser.newContext();
  await denied.addInitScript(() => Object.defineProperty(window,'sessionStorage',{get(){throw new DOMException('test denied','SecurityError');}}));
  const blocked = await denied.newPage(); const storageErrors = []; blocked.on('pageerror',e => storageErrors.push(e.message));
  await blocked.goto(base+'/chat'); await waitIdle(blocked);
  assert.match(await blocked.locator('#remember-status').innerText(),/Tab storage unavailable/);
  await blocked.locator('#agent').fill(operator); await blocked.locator('#apply-identity').click(); await waitIdle(blocked);
  await blocked.locator('#remember-agent').click();
  assert.equal(await blocked.locator('#remember-agent').isChecked(),false);
  assert.equal(await blocked.evaluate(() => state.agent),operator);
  assert.deepEqual(storageErrors,[]); await denied.close();
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle(page);
  assert.equal(await page.locator('.msg').count(),3);
  assert.match(await page.locator('#participants').innerText(),/ui-test-recipient/);
  assert.match(await page.locator('#participants').innerText(),/online/);
  assert.match(await page.locator('#permissions').innerText(),/allowed/);
  assert.match(await page.locator('#permissions').innerText(),/admin: denied/);
  assert.match(await page.locator('#capabilities').innerText(),/ui-test-analysis/);
  assert.equal(await page.locator('#transcript img').count(),0);
  assert.equal(await page.evaluate(() => window.__injected),undefined);
  await page.locator('#thread-filter').selectOption(message.thread_id);
  assert.equal(await page.locator('.msg').count(),2);
  assert.ok(!(await page.locator('#transcript').innerText()).includes('Independent second thread'));
  await page.locator('[data-reply]').first().click();
  await page.locator('#text').fill('Browser test reply stays in thread');
  await page.locator('#send-button').click(); await waitIdle(page);
  const transcript = await api(base,'GET',`/sessions/${session.id}/messages`);
  const reply = transcript.messages.find(m => m.payload?.text === 'Browser test reply stays in thread');
  assert.ok(reply); assert.equal(reply.parent_id,message.id); assert.equal(reply.thread_id,message.thread_id);
  assert.equal(reply.message_kind,'plain');
  await page.locator('#thread-filter').selectOption('');
  // Addressing alone must never create a lifecycle.
  await page.locator('#recipient').selectOption(agent);
  await page.locator('#text').fill('@'+agent+' this addresses you, it is not work');
  await page.locator('#send-button').click(); await waitIdle(page);
  assert.equal((await api(base,'GET',`/sessions/${session.id}/tasks`)).count,0);
  assert.match(await page.locator('#transcript').innerText(),/Addressed · no execution/);
  // Tasks require a separate intent and an explicit recipient.
  await page.locator('#intent').selectOption('task'); await page.locator('#recipient').selectOption('');
  await page.locator('#text').fill('Explicit browser verification task'); await page.locator('#send-button').click();
  assert.match(await page.locator('#send-result').innerText(),/Choose an explicit agent/);
  assert.equal((await api(base,'GET',`/sessions/${session.id}/tasks`)).count,0);
  await page.locator('#recipient').selectOption(agent); await page.locator('#send-button').click(); await waitIdle(page);
  const tasks = await api(base,'GET',`/sessions/${session.id}/tasks`); assert.equal(tasks.count,1);
  assert.match(await page.locator('#tasks').innerText(),new RegExp(tasks.tasks[0].task_id));
  assert.match(await page.locator('.msg.task').innerText(),/Explicit task/);
  await page.locator('#tasks [data-thread]').click(); assert.equal(await page.locator('#thread-filter').inputValue(),tasks.tasks[0].thread_id);
  await page.locator('#thread-filter').selectOption('');
  // DF-CRIER-331: real claim/complete writes, not just task-source markers.
  const taskID = tasks.tasks[0].task_id;
  const transitions = [{state:tasks.tasks[0].state,owner:tasks.tasks[0].owner || ''}];
  assert.equal(transitions[0].state,'open');
  // Test-only UI-state changes exercise the SAME reason gate, without changing
  // the server's session lifecycle or inventing a close route.
  const assertActionGates = async action => {
    for (const precondition of ['identity','session','closed']) {
      await page.evaluate(precondition => {
        window.__gateSaved = {agent:state.agent,session:state.session};
        if (precondition === 'identity') state.agent = '';
        if (precondition === 'session') state.session = null;
        if (precondition === 'closed') state.session = {...state.session,state:'closed'};
        updateComposer();
      },precondition);
      assert.equal(await page.locator('[data-task-action="'+action+'"]').isDisabled(),true);
      assert.equal(await page.locator('[data-task-action="'+action+'"]').getAttribute('title'),await page.locator('#composer-why').innerText());
      await page.evaluate(() => { Object.assign(state,window.__gateSaved); delete window.__gateSaved; updateComposer(); });
    }
  };
  // Act as the recipient for BOTH transitions, never spoof ownership in storage.
  await page.locator('#agent').fill(agent); await page.locator('#apply-identity').click(); await waitIdle(page);
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle(page);
  const taskPath = `/sessions/${session.id}/tasks/${taskID}`;
  let taskEvidence;
  for (const [action,expected] of [['claim','claimed'],['complete','done']]) {
    await assertActionGates(action);
    const response = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith(taskPath+'/'+action));
    await page.locator('[data-task-action="'+action+'"]').click();
    const wire = await response;
    const result = await wire.json(); await waitIdle(page);
    assert.equal(JSON.parse(wire.request().postData()).actor,agent,'task '+action+' browser actor');
    if (action === 'claim') {
      assert.equal(wire.status(),201,'POST '+taskPath+'/claim: '+JSON.stringify(result));
      assert.equal(result.state,expected); assert.equal(result.owner,agent);
      verified('UI Claim button POST '+base+taskPath+'/claim -> 201; owner '+agent);
      await page.locator('#tasks').screenshot({path:path.join(evidence,'chat-task-claimed.png')});
    } else {
      // The stock UI has no evidence field. Its bare Complete button must be
      // refused, not called a successful completion or hidden by a canned 201.
      assert.equal(wire.status(),409,'bare UI Complete must require evidence: '+JSON.stringify(result));
      assert.equal(result.error,'TASK_EVIDENCE_REQUIRED','bare Complete refusal code');
      assert.match(await page.locator('#task-result').innerText(),/TASK_EVIDENCE_REQUIRED/);
      const unchanged = await api(base,'GET',taskPath);
      assert.equal(unchanged.state,'claimed'); assert.equal(unchanged.owner,agent);
      verified('UI Complete button POST '+base+taskPath+'/complete -> real 409 TASK_EVIDENCE_REQUIRED; refusal rendered; API task unchanged');
      const artifact = path.join(evidence,'task-evidence.json');
      // Evidence is what this browser actually measured BEFORE completion, not
      // a promised future run, invented runtime result or canned server reply.
      const observed = {taskID,owner:agent,chatURL:base+'/chat',checks:[...browserChecks],screenshot:pathToFileURL(path.join(evidence,'chat-task-claimed.png')).href};
      fs.writeFileSync(artifact,JSON.stringify(observed,null,2));
      taskEvidence = {artifact:pathToFileURL(artifact).href,observed};
      const completion = await page.evaluate(async ({endpoint,actor,payload}) => {
        const response = await fetch(endpoint,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({actor,actor_kind:'agent',payload})});
        return {status:response.status,body:await response.json()};
      },{endpoint:taskPath+'/complete',actor:agent,payload:taskEvidence});
      assert.equal(completion.status,201,'browser fetch POST '+taskPath+'/complete: '+JSON.stringify(completion.body));
      assert.equal(completion.body.state,expected); assert.equal(completion.body.owner,agent);
      await refresh(page);
      verified('BROWSER FETCH (not Complete form) POST '+base+taskPath+'/complete -> 201; same owner '+agent+'; real evidence '+taskEvidence.artifact);
    }
    const stored = await api(base,'GET',taskPath);
    assert.equal(stored.state,expected); assert.equal(stored.owner,agent);
    if (action === 'complete') assert.deepEqual(stored.evidence,taskEvidence,'GET task must retain the exact evidence sent');
    assert.match(await page.locator('#task-result').innerText(),new RegExp(action === 'claim' ? expected : 'TASK_EVIDENCE_REQUIRED'));
    assert.match(await page.locator('#tasks').innerText(),new RegExp(expected));
    transitions.push({state:stored.state,owner:stored.owner});
  }
  verified('task open -> claimed -> done; exact stored evidence; refreshed task DOM done');
  await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await waitIdle(page);
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle(page);
  assert.equal(await page.locator('[data-task-action]').count(),0); // terminal, no invalid actions
  await page.locator('#tasks').screenshot({path:path.join(evidence,'chat-task-done.png')});
  // Real per-target refusal: remove a previously selected registered recipient.
  await page.locator('#intent').selectOption('message'); await page.locator('#recipient').selectOption(agent);
  const deletePath = '/agents/'+agent, ts = String(Math.floor(Date.now()/1000));
  const sig = require('node:crypto').sign(null,Buffer.from('DELETE\n'+deletePath+'\n'+ts),keypair.privateKey).toString('hex');
  await api(base,'DELETE',deletePath,undefined,{'X-Agent-ID':agent,'X-Agent-Ts':ts,'X-Agent-Sig':sig});
  await refresh(page);
  assert.equal(await page.locator('#recipient').inputValue(),agent); // do not silently broadcast
  await page.locator('#text').fill('This delivery must be refused'); await page.locator('#send-button').click(); await waitIdle(page);
  assert.match(await page.locator('#send-result').innerText(),/no such agent \(465\).*AGENT_NOT_FOUND/);
  assert.match(await page.locator('#transcript').innerText(),/no such agent \(465\).*AGENT_NOT_FOUND/);
  const refusal = (await api(base,'GET',`/sessions/${session.id}/messages`)).messages.find(m => m.payload?.text === 'This delivery must be refused');
  assert.equal(refusal.outcomes[0].outcome,'refused');
  // Controlled transport faults exist only in this test browser, not product code.
  for (const [code,body,expected] of [[401,{error:'missing Authorization header'},'HTTP 401'],[403,{error:{code:'TASK_FORBIDDEN',message:'Test refusal'}},'TASK_FORBIDDEN'],[500,{error:{code:'STORE_ERROR',message:'Test storage failure'}},'STORE_ERROR']]) {
    await page.route('**/sessions/*/messages',route => route.request().method() === 'POST' ? route.fulfill({status:code,contentType:'application/json',body:JSON.stringify(body)}) : route.continue());
    await page.locator('#text').fill('Keep this draft on failure '+code); await page.locator('#send-button').click(); await waitIdle(page);
    assert.match(await page.locator('#send-result').innerText(),new RegExp(expected));
    assert.equal(await page.locator('#text').inputValue(),'Keep this draft on failure '+code);
    await page.unroute('**/sessions/*/messages');
  }
  await page.locator('#text').fill('');
  await page.route('**/sessions/*/messages',route => route.fulfill({status:500,contentType:'application/json',body:JSON.stringify({error:{code:'STORE_ERROR',message:'Read failure'}})}));
  await refresh(page); assert.match(await page.locator('#transcript').innerText(),/STORE_ERROR/); assert.equal(await page.locator('#send-button').isDisabled(),true);
  await page.unroute('**/sessions/*/messages'); await refresh(page);
  // Delayed old-session reads must never overwrite the newly selected session.
  const second = await api(base,'POST','/sessions',{title:'Second channel for race test',kind:'channel',created_by:{agent:operator},members:[{member_type:'agent',member_id:operator}]});
  await refresh(page);
  await page.route(`**/sessions/${session.id}/messages`,async route => { await new Promise(r => setTimeout(r,300)); await route.continue(); });
  await page.locator('#refresh').click(); await page.locator(`[data-session="${second.id}"]`).click(); await waitIdle(page);
  await page.waitForTimeout(500);
  assert.equal(await page.locator('#conversation-title').innerText(),second.title);
  assert.equal(await page.locator('.msg').count(),0);
  await page.unroute(`**/sessions/${session.id}/messages`);
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle(page);
  const measurements = [];
  for (const width of [390,768,1280]) {
    await page.setViewportSize({width,height:1000});
    const result = await page.evaluate(() => ({viewport:innerWidth,scrollWidth:document.documentElement.scrollWidth,bodyWidth:document.body.scrollWidth,overflows:[...document.querySelectorAll('body *')].filter(el => el.getClientRects().length && el.scrollWidth > el.clientWidth+1 && !['SELECT','TEXTAREA'].includes(el.tagName) && getComputedStyle(el).overflowX !== 'hidden' && !el.classList.contains('skip')).map(el => ({id:el.id,tag:el.tagName,class:el.className,client:el.clientWidth,scroll:el.scrollWidth}))}));
    assert.equal(result.viewport,width); assert.ok(result.scrollWidth<=width,JSON.stringify(result)); assert.ok(result.bodyWidth<=width,JSON.stringify(result)); assert.deepEqual(result.overflows,[]);
    await page.locator('#text').scrollIntoViewIfNeeded();
    for (const selector of ['#text','#recipient','#intent','#send-button']) { const b = await page.locator(selector).boundingBox(); assert.ok(b && b.x >= 0 && b.x+b.width <= width && b.width >= 42, selector+' clipped'); }
    await page.evaluate(() => { const panel = document.getElementById('transcript'); panel.scrollTop = panel.scrollHeight; });
    const lastMessage = await page.evaluate(() => { const el = document.getElementById('transcript'); const panel = el.getBoundingClientRect(); const message = document.querySelector('#transcript .msg:last-child').getBoundingClientRect(); return {scrollTop:el.scrollTop,visible:message.top >= panel.top - 1 && message.bottom <= panel.bottom + 1}; });
    assert.equal(lastMessage.visible,true,'last message must be reachable at '+width);
    result.lastMessage = lastMessage;
    await page.screenshot({path:path.join(evidence,`chat-${width}.png`),fullPage:true});
    measurements.push(result);
  }
  await page.setViewportSize({width:1280,height:1000});
  await page.locator('#composer').screenshot({path:path.join(evidence,'chat-composer-detail.png')});
  // Empty states and creation through the actual browser form.
  await page.locator('#session-search').fill('no-such-session'); assert.match(await page.locator('#sessions').innerText(),/No matching/);
  await page.locator('#session-search').fill(''); await page.locator('#new-session').click();
  // Re-register the deliberately removed fixture, then choose it in the real form.
  await api(base,'POST','/agents',{id:agent,public_key,capabilities:['ui-test-analysis']});
  await refresh(page);
  await page.locator('#initial-members').selectOption([operator,agent]); // self is deduplicated
  await refresh(page);
  assert.deepEqual(await page.locator('#initial-members').evaluate(el => Array.from(el.selectedOptions,o => o.value)),[operator,agent]);
  await page.locator('#session-title').fill('Created by browser form'); await page.locator('#create-button').click(); await waitIdle(page);
  const createdID = await page.evaluate(() => state.current);
  const createdMembers = (await api(base,'GET',`/sessions/${createdID}/participants`)).participants;
  assert.deepEqual(createdMembers.map(p => p.member_id).sort(),[operator,agent].sort());
  await page.locator('#text').fill('Plain send to the initial members'); await page.locator('#send-button').click(); await waitIdle(page);
  const createdMessages = (await api(base,'GET',`/sessions/${createdID}/messages`)).messages;
  const plain = createdMessages.find(m => m.payload?.text === 'Plain send to the initial members');
  assert.ok(plain); assert.equal(plain.message_kind,'plain');
  assert.ok(plain.outcomes.some(o => o.target === agent && o.outcome !== 'refused'),JSON.stringify(plain.outcomes));
  assert.equal(await page.locator('#conversation-title').innerText(),'Created by browser form');
  assert.ok((await api(base,'GET','/sessions')).sessions.some(s => s.title === 'Created by browser form'));
  // DF-CRIER-331: operator-only channel, fresh audience, first click NEVER POSTs.
  await page.locator('#new-session').click();
  await page.locator('#initial-members').selectOption([]);
  await page.locator('#session-title').fill('DF-CRIER-331 note to self');
  await page.locator('#create-button').click(); await waitIdle(page);
  const soloID = await page.evaluate(() => state.current);
  let messagePosts = 0;
  page.on('request',request => { if (request.method() === 'POST' && request.url().endsWith('/sessions/'+soloID+'/messages')) messagePosts++; });
  await page.locator('#text').fill('Note to self with no receiving agent');
  await page.locator('#send-button').click(); await waitIdle(page);
  assert.equal(await page.locator('#audience-warning').isVisible(),true);
  const warningText = await page.locator('#audience-warning').innerText();
  assert.match(warningText,/no agent will receive this — the session has no agent member/);
  assert.equal(messagePosts,0);
  assert.equal((await api(base,'GET',`/sessions/${soloID}/messages`)).messages.length,0);
  await page.screenshot({path:path.join(evidence,'chat-no-agent-warning.png'),fullPage:true});
  await page.locator('.composer-wrap').screenshot({path:path.join(evidence,'chat-no-agent-detail.png')});
  // Changing the draft must disarm the first confirmation; the next click arms again.
  await page.locator('#text').fill('Revised note to self');
  assert.equal(await page.locator('#audience-warning').isVisible(),false);
  await page.locator('#send-button').click(); await waitIdle(page); assert.equal(messagePosts,0);
  await page.locator('#send-button').click(); await waitIdle(page); assert.equal(messagePosts,1);
  const note = (await api(base,'GET',`/sessions/${soloID}/messages`)).messages[0];
  assert.equal(note.payload.text,'Revised note to self'); assert.deepEqual(note.audience.targets || [],[]);
  assert.deepEqual(note.outcomes || [],[]); assert.equal(await page.locator('#audience-warning').isVisible(),false);
  assert.equal((await page.locator('#registered-agents option').evaluateAll(options => options.map(o => o.value))).includes(agent),true);
  // The server's message is shown verbatim, not replaced by a generic failure.
  await page.route(`**/sessions/${soloID}/participants`,route => route.request().method() === 'POST' ? route.fulfill({status:403,contentType:'application/json',body:JSON.stringify({error:{code:'TEST_REFUSAL',message:'Exact participant refusal from test server.'}})}) : route.continue());
  await page.locator('#participant-agent').fill(agent); await page.locator('#add-participant').click(); await waitIdle(page);
  assert.equal(await page.locator('#participant-result').innerText(),'HTTP 403: TEST_REFUSAL: Exact participant refusal from test server.');
  await page.unroute(`**/sessions/${soloID}/participants`);
  const addResponse = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/sessions/'+soloID+'/participants'));
  await page.locator('#add-participant').click(); const addedWire = await addResponse;
  assert.equal(addedWire.status(),201); const added = await addedWire.json(); await waitIdle(page);
  const roster = (await api(base,'GET',`/sessions/${soloID}/participants`)).participants;
  assert.ok(roster.some(p => p.member_id === agent && p.member_type === 'agent' && p.role === 'member' && p.active));
  assert.match(await page.locator('#participants').innerText(),/ui-test-recipient/);
  assert.match(await page.locator('#participants').innerText(),/agent · member/);
  await page.screenshot({path:path.join(evidence,'chat-participant-added.png'),fullPage:true});
  await page.locator('#participants').screenshot({path:path.join(evidence,'chat-participant-detail.png')});
  // Populated audience sends on FIRST click, without a leftover warning.
  await page.locator('#text').fill('Now the added member receives this');
  await page.locator('#send-button').click(); await waitIdle(page); assert.equal(messagePosts,2);
  const delivered = (await api(base,'GET',`/sessions/${soloID}/messages`)).messages.find(m => m.payload?.text === 'Now the added member receives this');
  assert.ok(delivered.outcomes.some(o => o.target === agent && o.outcome !== 'refused'));
  const completion = {session:session.id,taskID,transitions,evidence:taskEvidence,completionMethod:'browser fetch with evidence; bare UI Complete refused 409',soloSession:soloID,warningText,firstClickPosts:0,secondClickPosts:1,noteAudience:note.audience,addedParticipant:added,storedParticipant:roster.find(p => p.member_id === agent),recipientOutcomes:delivered.outcomes};
  fs.writeFileSync(path.join(evidence,'completion-results.json'),JSON.stringify(completion,null,2));
  // Attribute escaping: inject hostile-looking values into the read response
  // only, then prove no extra element or event attribute reached the DOM.
  await page.route('**/sessions',route => route.request().method() === 'GET' ? route.fulfill({contentType:'application/json',body:JSON.stringify({sessions:[{id:'\" onmouseover=\"window.__injected=true',title:'<img src=x onerror=window.__injected=true>',kind:'channel',state:'open',message_count:0}]})}) : route.continue());
  await refresh(page); assert.equal(await page.locator('#sessions img, #sessions [onmouseover]').count(),0);
  await page.unroute('**/sessions');
  // Read errors remain visible, never masquerading as empty sessions.
  await page.route('**/sessions',route => route.fulfill({status:401,contentType:'application/json',body:'{"error":"missing Authorization header"}'}));
  await refresh(page); assert.match(await page.locator('#sessions').innerText(),/Unavailable — HTTP 401/);
  await page.unroute('**/sessions');
  const token = randomBytes(24).toString('hex'); const protectedBase = await start(token);
  const locked = await fetch(protectedBase+'/chat'); assert.equal(locked.status,401);
  const authorized = await fetch(protectedBase+'/chat',{headers:{Authorization:'Bearer '+token}}); assert.equal(authorized.status,200);
  assert.match(await authorized.text(),/data-ui-revision="CR-CHAT-038"/);
  assert.deepEqual(errors,[]);
  verified(base+'/chat: attribution opt-in/reload/forget/storage denial; thread isolation and reply; addressing vs task intent; refusal and draft retention; stale-read isolation; no DOM XSS; create and participant forms; empty search; zero-recipient confirmation; controls and transcript reachable at 390/768/1280; no page errors');
  const report = {apiChecks,browserChecks,protectedBase,limitations:['No runtime task execution; evidence describes harness observations only','Complete UI has no evidence entry: button refusal verified, successful completion is explicit browser fetch','Auth boundary is Node HTTP fetch, not browser login','Capability matrix, transport failures, hostile responses and storage denial use test-only fixtures','CHAT_MATRIX_ONLY=1 omits full-suite checks'],method:'Real headless Chromium, stock Go binary with embedded HTML; scratch JSONL sessions and in-memory registry. Faults are explicitly browser-intercepted test responses.',base,measurements,pageErrors:errors,authBoundary:{noHeader:locked.status,bearerHeader:authorized.status},completion,checks:['participant datalist + browser POST + API roster readback; verbatim refusal','task Claim UI and evidence-bearing browser fetch; bare Complete real 409; exact evidence API readback and refreshed task DOM; both action gates use the composer reason','zero-recipient first click zero POSTs; changed draft rearms; second click records; added agent receives on first click','opt-in off by default; unsaved reload re-gates','one attribution-only tab key; reload applies it; selected-session composer enabled','Forget and uncheck clear immediately; independent tab forgets','storage denial keeps attribution usable','initial members and plain-send outcome read back from API','rendered server data','thread isolation and reply API readback','addressing creates no task','explicit task API readback','real delivery refusal','401/403/500 retain draft','read failure disables send','stale request isolation','no DOM XSS','create form readback','empty search','controls fit all three widths']};
  fs.writeFileSync(path.join(evidence,'browser-results.json'),JSON.stringify(report,null,2));
  console.log(JSON.stringify(report,null,2)); console.log('PASS: full browser suite and 3 viewport checks; exit code 0. Evidence: '+evidence);
}
run().catch(error => {console.error('FAIL: browser harness; exit code 1'); console.error(error); process.exitCode=1;}).finally(async () => {
  if (browser) await browser.close();
  for (const proc of processes) { proc.kill('SIGTERM'); await Promise.race([new Promise(r => proc.once('exit',r)),new Promise(r => setTimeout(r,3000))]); if (proc.exitCode === null) proc.kill('SIGKILL'); }
  // Keep scratch logs on failure for attribution; never clean shared paths.
  if (process.exitCode === 1 && tempdirs.length) console.error('Scratch server logs: '+tempdirs.join(', '));
});
