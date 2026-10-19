#!/usr/bin/env node
// DF-CRIER-325: real Chromium running production chat.html. API responses are
// explicitly intercepted test fixtures; no server/runtime execution is claimed.
// NODE_PATH=/tmp/crier-browser/node_modules node scripts/chat-task-link.cjs
// CHAT_TEST_HTML selects an immutable historical HTML copy for the RED control.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
let chromium;
try { ({chromium} = require('playwright')); }
catch (error) {
  if (error.code !== 'MODULE_NOT_FOUND') throw error;
  console.error('SKIP: Playwright unavailable; install outside the repo and set NODE_PATH; exit code 77');
  process.exit(77);
}
const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
try { fs.accessSync(executablePath,fs.constants.X_OK); }
catch { console.error('SKIP: Chromium unavailable at '+executablePath+'; exit code 77'); process.exit(77); }
const evidence = process.env.CHAT_EVIDENCE || fs.mkdtempSync(path.join(os.tmpdir(),'crier-task-link-'));
const html = fs.readFileSync(process.env.CHAT_TEST_HTML || path.join(__dirname,'../cmd/server/chat.html'),'utf8');
const taskID = 'task-fixture-325', thread = 'thread-fixture-325';
const session = {id:'session-fixture-325',title:'Task link browser fixture',kind:'channel',state:'open'};
const message = {id:'message-fixture-325',seq:1,message_kind:'task',thread_id:thread,author:{agent:'operator'},payload:{text:'Task lifecycle discoverability fixture'},task:{task_id:taskID},outcomes:[]};
const other = {id:'other-message',seq:2,message_kind:'plain',thread_id:'other-thread',author:{agent:'operator'},payload:{text:'Independent thread'}};
let browser;
(async () => {
  fs.mkdirSync(evidence,{recursive:true});
  browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  // Optional scratch Go server supplied by TestChatClientTaskMessageLinkBrowser.
  // This positive control consumes the real wire shape, not hand-built task JSON.
  if (process.argv[2]) {
    const base = process.argv[2];
    const api = async (endpoint,body) => {
      const response = await fetch(base+endpoint,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      const result = await response.json();
      assert.equal(response.status,201,endpoint+': '+JSON.stringify(result));
      return result;
    };
    const operator = 'task-link-operator', agent = 'task-link-recipient';
    for (const id of [operator,agent]) await api('/agents',{id});
    const room = await api('/sessions',{title:'Live task link regression',kind:'channel',created_by:{agent:operator},members:[{member_type:'agent',member_id:operator},{member_type:'agent',member_id:agent}]});
    const task = await api('/sessions/'+room.id+'/tasks',{sender:operator,payload:{text:'Real wire task link'},targets:[{kind:'agent',id:agent}]});
    const live = await browser.newPage({viewport:{width:1280,height:1000}});
    await live.goto(base+'/chat'); await live.waitForFunction(() => !state.busy);
    await live.locator('[data-session="'+room.id+'"]').click(); await live.waitForFunction(() => !state.busy);
    const control = live.locator('.msg.task').getByRole('button',{name:'Open in Tasks',exact:true});
    assert.equal(await control.count(),1,'real transcript task.task_id must match the loaded Tasks row');
    await control.click();
    assert.equal(await live.locator('#thread-filter').inputValue(),task.thread_id);
    assert.equal(await live.locator('[data-task-row="'+task.task_id+'"] .task-link').evaluate(el => el === document.activeElement),true);
    await live.screenshot({path:path.join(evidence,'task-live-server.png'),fullPage:true});
    await live.close();
    console.log('LIVE PASS: stock Go server task creation, task.task_id mapping, bubble button, matching Tasks row focus and thread filter');
  }
  const page = await browser.newPage({viewport:{width:1280,height:1000}});
  const errors = []; page.on('pageerror',error => errors.push(error.message));
  let tasks = [{task_id:taskID,thread_id:thread,state:'claimed',owner:'operator'}], taskError = false;
  let taskDelay = false, messageDelay = false;
  await page.route('http://task-link.test/**',async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === '/chat') return route.fulfill({contentType:'text/html',body:html});
    let body = {};
    if (endpoint === '/sessions') body = {sessions:[session]};
    if (endpoint === '/status') body = {health:'ok',require_agent_signature:false};
    if (endpoint.endsWith('/messages')) {
      if (messageDelay) await new Promise(resolve => setTimeout(resolve,150));
      body = {session,messages:[message,other],findings:[]};
    }
    if (endpoint.endsWith('/tasks')) {
      if (taskDelay) await new Promise(resolve => setTimeout(resolve,150));
      if (taskError) return route.fulfill({status:503,json:{error:'task list fixture unavailable'}});
      body = {tasks};
    }
    await route.fulfill({json:body});
  });
  await page.goto('http://task-link.test/chat');
  await page.waitForFunction(() => !state.busy);
  await page.locator('[data-session="'+session.id+'"]').click();
  await page.waitForFunction(() => !state.busy);
  const bubble = page.locator('.msg.task');
  const link = bubble.getByRole('button',{name:'Open in Tasks',exact:true});
  assert.equal(await link.count(),1,'task-bearing bubble must expose an Open in Tasks button');
  assert.match(await bubble.innerText(),/task state: not exposed/);
  assert.match(await bubble.innerText(),/Lifecycle lives in the Tasks panel/);
  assert.equal(await link.getAttribute('data-thread'),thread);
  assert.equal(await link.isEnabled(),true);
  assert.equal(await page.locator('.msg.plain .task-navigation').count(),0,'ordinary messages must not acquire task semantics');
  await page.evaluate(() => { state.sending = true; updateComposer(); });
  assert.equal(await link.isDisabled(),true,'in-flight send must not leave a no-op button enabled');
  await page.evaluate(() => { state.sending = false; updateComposer(); });
  assert.equal(await link.isEnabled(),true);
  // Native keyboard focus/activation, not a synthetic DOM click.
  await page.locator('#thread-filter').focus();
  let reached = false;
  for (let i=0;i<15;i++) {
    await page.keyboard.press('Tab');
    if (await link.evaluate(el => el === document.activeElement)) { reached = true; break; }
  }
  assert.equal(reached,true,'task affordance must be keyboard reachable');
  await page.evaluate(() => refresh());
  assert.equal(await link.evaluate(el => el === document.activeElement),true,'unchanged refresh must preserve keyboard focus on the affordance');
  await page.keyboard.press('Enter');
  assert.equal(await page.locator('#thread-filter').inputValue(),thread);
  assert.equal(await page.locator('.msg').count(),1,'thread filter isolates the task');
  assert.equal(await page.locator('#tasks [data-task-row="'+taskID+'"] .task-link').evaluate(el => el === document.activeElement),true,'activation must focus the matching Tasks row');
  await page.screenshot({path:path.join(evidence,'task-open-in-tasks.png'),fullPage:true});
  await bubble.screenshot({path:path.join(evidence,'task-bubble-detail.png')});
  // Existing reverse direction must still use the shared data-thread mechanism.
  await page.locator('#thread-filter').selectOption('');
  await page.locator('#tasks .task-link').click();
  assert.equal(await page.locator('#thread-filter').inputValue(),thread);
  const refresh = async () => { await page.locator('#refresh').click(); await page.waitForFunction(() => !state.busy); };
  tasks = []; taskDelay = true;
  await refresh();
  assert.match(await bubble.innerText(),/not in the loaded task list/);
  assert.equal(await link.count(),0,'absent task must not look operable');
  await page.screenshot({path:path.join(evidence,'task-absent.png'),fullPage:true});
  await bubble.screenshot({path:path.join(evidence,'task-absent-detail.png')});
  tasks = [{task_id:'different-task',thread_id:thread,state:'open'}];
  await refresh();
  assert.equal(await link.count(),0,'same thread with a different task ID must not link');
  tasks = [{task_id:taskID,thread_id:thread,state:'done'}];
  await refresh();
  assert.equal(await link.count(),1,'task arriving after transcript must restore the affordance');
  assert.match(await bubble.innerText(),/task state: not exposed/);
  taskError = true;
  await refresh();
  assert.match(await bubble.innerText(),/not in the loaded task list/);
  assert.equal(await link.count(),0,'failed task read must not leave a stale operable link');
  taskError = false; taskDelay = false; messageDelay = true;
  await refresh();
  assert.equal(await link.count(),1,'transcript arriving after tasks must expose the affordance');
  assert.deepEqual(errors,[]);
  const result = {method:'Real Chromium; production HTML; explicitly intercepted API fixtures',control:'button',affordance:'Open in Tasks',absent:'not in the loaded task list; no operable control',keyboard:'Tab + Enter selects thread and focuses matching task row',checks:['present task','absent task','wrong ID same thread','tasks after messages','messages after tasks','task read failure','existing reverse link','snapshot remains not exposed'],pageErrors:errors};
  fs.writeFileSync(path.join(evidence,'results.json'),JSON.stringify(result,null,2));
  console.log('PASS DF-CRIER-325: '+JSON.stringify(result));
})().catch(error => { console.error(error); process.exitCode = 1; }).finally(async () => { if (browser) await browser.close(); });
