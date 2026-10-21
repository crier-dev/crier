#!/usr/bin/env node
// DF-CRIER-446: stock server + real Chromium, scratch JSONL and SQLite only.
// Build: go build -o /tmp/df446-server ./cmd/server
// Run: NODE_PATH=/tmp/crier-browser/node_modules node scripts/session-lifecycle-browser.cjs
// CRIER_TEST_BINARY and CHAT_EVIDENCE optionally override the binary/artifact paths.
const assert = require('node:assert/strict');
const {spawn, spawnSync} = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const {chromium} = require('playwright');
const evidence = process.env.CHAT_EVIDENCE || fs.mkdtempSync(path.join(os.tmpdir(),'df446-evidence-'));
fs.mkdirSync(evidence,{recursive:true});
const binary = process.env.CRIER_TEST_BINARY || '/tmp/df446-server';
let browser;
const children = new Set();
async function port() {
  const server=net.createServer();
  await new Promise((resolve,reject)=>server.once('error',reject).listen(0,'127.0.0.1',resolve));
  const value=server.address().port;await new Promise(r=>server.close(r));return value;
}
async function start(backend,dir,p) {
  const env=Object.fromEntries(Object.entries(process.env).filter(([k])=>!k.startsWith('CR_')&&!k.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(p),CR_REGISTRY_BACKEND:'memory',CR_SESSION_BACKEND:backend,CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),CR_SESSION_SQLITE_PATH:path.join(dir,'sessions.sqlite'),CR_GROUP_ROOT:path.join(dir,'groups'),CR_AUTH_TOKEN:'',CR_REQUIRE_AGENT_SIG:'false',CR_GUARD_ENABLED:'false',CR_RATE_LIMIT_PER_MINUTE:'0'});
  const log=fs.openSync(path.join(dir,'server.log'),'a');
  const proc=spawn(binary,[],{cwd:dir,env,stdio:['ignore',log,log]});fs.closeSync(log);children.add(proc);
  const base='http://127.0.0.1:'+p;
  for(let i=0;i<150;i++){
    if(proc.exitCode!==null)throw new Error('server exited: '+fs.readFileSync(path.join(dir,'server.log'),'utf8'));
    try {if((await fetch(base+'/health')).ok){assert.equal(proc.exitCode,null);return {proc,base};}} catch {}
    await new Promise(r=>setTimeout(r,100));
  }
  throw new Error('readiness failed: '+dir);
}
async function stop(proc) {
  if(proc.exitCode!==null)return;
  const ended=new Promise(resolve=>proc.once('exit',resolve));proc.kill('SIGTERM');
  const timer=setTimeout(()=>proc.kill('SIGKILL'),5000);await ended;clearTimeout(timer);children.delete(proc);
}
function curl(base,method,endpoint,body,status){
  const args=['-sS','--max-time','15','-X',method,base+endpoint,'-H','Content-Type: application/json'];
  if(body!==undefined)args.push('--data-binary',JSON.stringify(body));args.push('-w','\n%{http_code}');
  const result=spawnSync('curl',args,{encoding:'utf8'});assert.equal(result.status,0,result.stderr);
  const cut=result.stdout.lastIndexOf('\n');const code=Number(result.stdout.slice(cut+1)),raw=result.stdout.slice(0,cut);
  assert.equal(code,status,raw);console.log('CURL '+method+' '+endpoint+' -> '+code+'\n'+raw.trim());return JSON.parse(raw);
}
function stored(backend,dir,id) {
  let rows;
  if(backend==='jsonl')rows=fs.readFileSync(path.join(dir,'sessions',id+'.jsonl'),'utf8').trim().split('\n').map(JSON.parse).filter(r=>['session.close','session.reopen'].includes(r.type));
  else {
    const result=spawnSync('sqlite3',[path.join(dir,'sessions.sqlite'),`SELECT record_json FROM chat_session_lifecycle WHERE session_id = '${id}' ORDER BY seq;`],{encoding:'utf8'});
    assert.equal(result.status,0,result.stderr);rows=result.stdout.trim().split('\n').filter(Boolean).map(JSON.parse);
  }
  console.log('STORE READ '+backend+' (direct disk read, not HTTP response):\n'+rows.map(r=>JSON.stringify(r)).join('\n'));return rows;
}
async function idle(page) {await page.waitForFunction(()=>!state.sending&&!state.busy&&document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));}
async function select(page,id){await page.locator('[data-session="'+id+'"]').click();await idle(page);}
async function badge(page,value){await page.waitForFunction(v=>document.getElementById('session-state').textContent===v,value);}
async function run(){
  browser=await chromium.launch({executablePath:process.env.CHROME_BIN||'/usr/bin/google-chrome',headless:true,args:['--no-sandbox']});
  console.log('Real Chromium; scratch storage; signature-disabled deployment records claimed identity, not verified login.');
  for(const backend of ['jsonl','sqlite']){
    const dir=fs.mkdtempSync(path.join(os.tmpdir(),'df446-'+backend+'-'));const p=await port();let server=await start(backend,dir,p);const base=server.base;
    const id='df446-'+backend;
    curl(base,'POST','/sessions',{id,title:'DF-CRIER-446 '+backend,created_by:{agent:'operator'}},201);
    const page=await browser.newPage({viewport:{width:1440,height:1000}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(base+'/chat');await idle(page);await select(page,id);await badge(page,'open');
    assert.equal(await page.locator('#close-session').isVisible(),true);assert.equal(await page.locator('#close-session').isDisabled(),true);assert.equal(await page.locator('#reopen-session').isVisible(),false);
    await page.locator('#agent').fill('operator');await page.locator('#apply-identity').click();await idle(page);await select(page,id);
    assert.equal(await page.locator('#close-session').isEnabled(),true);
    // Required live proof: close with curl, read the real store, then refuse send.
    const closed=curl(base,'POST','/sessions/'+id+'/state',{state:'closed',sender:'operator'},200);
    const events=stored(backend,dir,id);assert.equal(events.length,1);assert.equal(events[0].actor.agent,'operator');assert.equal(events[0].sender_identity_basis,'sender-identity-claimed');assert.equal(events[0].ts,closed.closed_at);
    const refusal=curl(base,'POST','/sessions/'+id+'/messages',{sender:'operator',payload:{text:'must not be admitted'}},409);
    assert.deepEqual(refusal,{error:'SESSION_CLOSED',detail:'session "'+id+'" is closed and admits no new message (§1.3)'});
    // Stale UI still offers close; server refusal must refresh to reported closed.
    await page.locator('#close-session').click();await idle(page);await badge(page,'closed');
    assert.match(await page.locator('#session-lifecycle-result').textContent(),/SESSION_ALREADY_CLOSED/);
    assert.equal(await page.locator('#close-session').isVisible(),false);assert.equal(await page.locator('#reopen-session').isVisible(),true);
    assert.match(await page.locator('.system-event').textContent(),/marked complete.*operator.*sender-identity-claimed.*state closed/s);
    // Actually restart the server and reload the page, not an in-page state reset.
    await stop(server.proc);server=await start(backend,dir,p);
    await page.reload();await idle(page);await select(page,id);await badge(page,'closed');
    assert.equal(await page.locator('.system-event').count(),1);
    await page.screenshot({path:path.join(evidence,backend+'-closed.png'),fullPage:true});
    await page.locator('.thread-header').screenshot({path:path.join(evidence,backend+'-closed-detail.png')});
    await page.locator('#agent').fill('operator');await page.locator('#apply-identity').click();await idle(page);await select(page,id);
    await page.locator('#reopen-session').click();await idle(page);await badge(page,'open');
    assert.equal(await page.locator('#reopen-session').isVisible(),false);assert.equal(await page.locator('#close-session').isVisible(),true);
    const reopened=stored(backend,dir,id);assert.equal(reopened.length,2);assert.equal(reopened[1].type,'session.reopen');assert.equal(reopened[1].actor.agent,'operator');
    await page.reload();await idle(page);await select(page,id);await badge(page,'open');assert.equal(await page.locator('.system-event').count(),2);
    await page.screenshot({path:path.join(evidence,backend+'-reopened.png'),fullPage:true});
    // Exercise the legal Close button too, not only curl and a stale refusal.
    await page.locator('#agent').fill('operator');await page.locator('#apply-identity').click();await idle(page);await select(page,id);
    await page.locator('#close-session').click();await idle(page);await badge(page,'closed');
    const uiClosed=stored(backend,dir,id);assert.equal(uiClosed.length,3);assert.equal(uiClosed[2].type,'session.close');
    await page.locator('#reopen-session').click();await idle(page);await badge(page,'open');
    assert.equal(stored(backend,dir,id).length,4);
    // Labeled test-only missing-state and read-failure injections prove no guessed badge.
    await page.route('**/sessions/'+id+'/messages',async route=>{const response=await route.fetch();const data=await response.json();delete data.session.state;await route.fulfill({json:data});});
    await page.locator('#refresh').click();await idle(page);await badge(page,'Unavailable');assert.equal(await page.locator('#close-session').isVisible(),false);assert.equal(await page.locator('#reopen-session').isVisible(),false);
    await page.unroute('**/sessions/'+id+'/messages');
    await page.route('**/sessions/'+id+'/messages',route=>route.fulfill({status:503,json:{error:'TEST_ONLY_READ_UNAVAILABLE',detail:'injected read failure'}}));
    await page.locator('#refresh').click();await idle(page);await badge(page,'Unavailable');assert.equal(await page.locator('#close-session').isVisible(),false);
    await page.unroute('**/sessions/'+id+'/messages');await page.locator('#refresh').click();await idle(page);await badge(page,'open');
    assert.deepEqual(errors,[]);console.log('BROWSER PASS '+backend+': reload+process restart badge, illegal control visibility, stale-transition refusal, actual legal Close/Reopen clicks, retained events, unknown/failed read (test injection), no page errors.');
    await page.close();await stop(server.proc);
  }
  console.log('EVIDENCE '+evidence);
}
run().catch(error=>{console.error(error);process.exitCode=1;}).finally(async()=>{if(browser)await browser.close();for(const proc of children)await stop(proc);});
