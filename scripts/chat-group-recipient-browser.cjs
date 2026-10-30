#!/usr/bin/env node
// Real-browser regression: named group recipients bypass note-to-self confirmation.
const assert = require('node:assert/strict');
const {spawn, spawnSync} = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const {chromium} = require('playwright');
const binary = process.env.CRIER_TEST_BINARY;
if (!binary) throw new Error('set CRIER_TEST_BINARY to a freshly built Crier server binary');
const evidence = fs.mkdtempSync(path.join(os.tmpdir(), 'crier-group-recipient-'));
let server, browser;
async function freePort() {
  const s = net.createServer();
  await new Promise((resolve, reject) => s.once('error', reject).listen(0, '127.0.0.1', resolve));
  const p = s.address().port; await new Promise(r => s.close(r)); return p;
}
async function api(base, method, route, body) {
  const r = await fetch(base + route, {method, headers:{'Content-Type':'application/json'}, body:body === undefined ? undefined : JSON.stringify(body)});
  const text = await r.text(); let json = null; try { json = text ? JSON.parse(text) : null; } catch {}
  if (!r.ok) throw new Error(`${method} ${route} -> ${r.status}: ${text}`);
  return json;
}
async function main() {
  const store = fs.mkdtempSync(path.join(os.tmpdir(), 'crier-group-recipient-store-'));
  const port = await freePort(), base = `http://127.0.0.1:${port}`;
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env, {CRIER_PORT:String(port), CR_REGISTRY_BACKEND:'memory', CR_SESSION_BACKEND:'jsonl', CR_SESSION_LOG_ROOT:path.join(store,'sessions'), CR_GROUP_ROOT:path.join(store,'groups'), CR_AUTH_TOKEN:'', CR_REQUIRE_AGENT_SIG:'false', CR_GUARD_ENABLED:'false', CR_RATE_LIMIT_PER_MINUTE:'0'});
  const log = fs.openSync(path.join(evidence,'server.log'),'w');
  server = spawn(binary, [], {cwd:store, env, stdio:['ignore',log,log]}); fs.closeSync(log);
  for (let i=0; i<100; i++) { if (server.exitCode !== null) throw new Error('test server exited'); try { if ((await fetch(base+'/health')).ok) break; } catch {} await new Promise(r=>setTimeout(r,100)); if(i===99) throw new Error('server readiness timed out'); }
  const ids = ['group-solo-operator','group-solo-a','group-solo-b','group-solo-unrelated'];
  const keyDir = fs.mkdtempSync(path.join(os.tmpdir(),'crier-group-recipient-keys-'));
  for (const id of ids) {
    const key = path.join(keyDir,id+'.pem');
    const k = spawnSync(binary,['keygen','-out',key,'-id',id,'-server',base,'-json'],{encoding:'utf8'});
    if (k.status !== 0) throw new Error(`keygen ${id}: ${k.stderr||k.stdout}`);
    await api(base,'POST','/agents',{...JSON.parse(JSON.parse(k.stdout).registration),capabilities:['group-e2e']});
  }
  const [operator,a,b,unrelated] = ids, groupMembers = [a,b];
  // Only the actor is a session member; group members and unrelated are external.
  const session = await api(base,'POST','/sessions',{title:'Group solo-session regression',kind:'channel',created_by:{agent:operator},members:[{member_type:'agent',member_id:operator}]});
  await api(base,'POST','/groups',{name:'uat-team',created_by:operator,members:groupMembers});
  browser = await chromium.launch({executablePath:process.env.CHROME_BIN||'/usr/bin/google-chrome',headless:true,args:['--no-sandbox']});
  const page = await browser.newPage({viewport:{width:1440,height:1000}}), pageErrors = [], posted = [];
  page.on('pageerror',e=>pageErrors.push(e.message));
  const messageRoute = `/sessions/${session.id}/messages`;
  page.on('request',r=>{if(r.method()==='POST'&&new URL(r.url()).pathname===messageRoute) posted.push(r.postDataJSON());});
  await page.goto(base+'/chat');
  const waitIdle = () => page.waitForFunction(()=>!state.sending&&!state.busy&&document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'),null,{timeout:10000});
  await waitIdle();
  await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await waitIdle();
  await page.locator(`[data-session="${session.id}"]`).click(); await waitIdle();
  await page.locator('#text').fill('first-click group delivery regression');
  await page.locator('[data-group-address="uat-team"]').click();
  assert.equal(await page.locator('#recipient').inputValue(),'');
  assert.match(await page.locator('#group-recipient').innerText(),/@team:uat-team/);
  const responsePromise = page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===messageRoute,{timeout:10000});
  const warningPromise = page.locator('#audience-warning').waitFor({state:'visible',timeout:10000}).then(()=>({warning:true})).catch(()=>({warning:false}));
  await page.locator('#send-button').click();
  const firstClick = await Promise.race([responsePromise.then(response=>({response})),warningPromise]);
  assert.ok(!firstClick.warning,'first click displayed the false note-to-self warning');
  const response = firstClick.response;
  const result = await response.json();
  assert.equal(response.status(),201,JSON.stringify(result));
  await page.waitForFunction(t=>document.querySelector('#transcript').innerText.includes(t),'first-click group delivery regression',{timeout:10000});
  await waitIdle();
  assert.equal(await page.locator('#audience-warning').isVisible(),false,'first click displayed the false note-to-self warning');
  assert.equal(posted.length,1,`expected exactly one first-click POST, got ${posted.length}`);
  assert.equal(posted[0].targets,undefined,'group address must remain body-routed, not acquire an individual target');
  assert.match(posted[0].payload.text,/@team:uat-team/);
  const readback = await api(base,'GET',messageRoute);
  const msg = readback.messages.find(m=>m.payload?.text?.includes('first-click group delivery regression'));
  assert.ok(msg,'sent message missing in independent session readback');
  const outcomes = msg.outcomes || result.outcomes || [];
  const targets = outcomes.map(o=>o.target).sort();
  assert.deepEqual(targets,[...groupMembers].sort(),`server outcome targets: ${JSON.stringify(outcomes)}`);
  const inboxes = await Promise.all(ids.slice(1).map(id=>api(base,'GET',`/agents/${encodeURIComponent(id)}/inbox?limit=20`)));
  const entries = x=>x.messages||x.entries||[];
  const messageEntries = x=>entries(x).filter(m=>{try{return JSON.parse(Buffer.from(m.payload,'base64').toString('utf8')).text?.includes('first-click group delivery regression');}catch{return false;}});
  assert.equal(messageEntries(inboxes[0]).length,1,'group member A must receive exactly once');
  assert.equal(messageEntries(inboxes[1]).length,1,'group member B must receive exactly once');
  assert.equal(messageEntries(inboxes[2]).length,0,'unrelated session-external individual must receive nothing');
  assert.deepEqual(pageErrors,[],'browser JavaScript errors');
  const trace = {tested_binary:binary,session_id:session.id,group:'uat-team',session_participants:[operator],group_members:groupMembers,unrelated,first_click_posts:posted.length,warning_visible:await page.locator('#audience-warning').isVisible(),response_status:response.status(),outcome_targets:targets,inbox_match_counts:{[a]:messageEntries(inboxes[0]).length,[b]:messageEntries(inboxes[1]).length,[unrelated]:messageEntries(inboxes[2]).length},page_errors:pageErrors};
  fs.writeFileSync(path.join(evidence,'result.json'),JSON.stringify(trace,null,2));
  console.log('LIVE_GROUP_SOLO_SESSION_TRACE '+JSON.stringify(trace));
  console.log('LIVE UI GROUP SOLO-SESSION PASS');
}
main().catch(e=>{console.error(e);process.exitCode=1;}).finally(async()=>{if(browser)await browser.close();if(server&&server.exitCode===null){server.kill('SIGTERM');await new Promise(r=>server.once('exit',r));}});
