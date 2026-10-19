#!/usr/bin/env node
// DF-CRIER-391: two real scratch deployments, real /status and shipped /chat.
// NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/crier-391 \
//   node scripts/federation-posture-browser.cjs
// All records/credentials are test-only. No intercepted or canned API responses.
const assert = require('node:assert/strict');
const {spawn} = require('node:child_process');
const {generateKeyPairSync, createHash} = require('node:crypto');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const {chromium} = require('playwright');
const token = 'federation-posture-browser-test';
const evidence = process.env.CHAT_EVIDENCE || path.resolve('docs/screenshots/df-crier-391');
const processes = [];
const observations = [];
let browser;
async function freePort() {
  const socket = net.createServer();
  await new Promise((resolve,reject) => socket.once('error',reject).listen(0,'127.0.0.1',resolve));
  const port = socket.address().port;
  await new Promise(resolve => socket.close(resolve));
  return port;
}
async function boot(linked) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'crier-391-browser-'));
  const port = await freePort();
  const {publicKey, privateKey} = generateKeyPairSync('ed25519');
  const pub = publicKey.export({type:'spki',format:'der'}).subarray(-32);
  const keyfile = path.join(dir,'self.pem'), records = path.join(dir,'peers.json');
  fs.writeFileSync(keyfile,privateKey.export({type:'pkcs8',format:'pem'}),{mode:0o600});
  fs.writeFileSync(records,JSON.stringify({version:1,peers:linked ? [{
    id:'unrelated-test-peer',public_key:pub.toString('hex'),
    key_id:createHash('sha256').update(pub).digest('hex').slice(0,16),
    url:'https://unrelated.example',direction:'both',state:'linked',label:'Test fixture'
  }] : []}),{mode:0o600});
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(port),CR_AUTH_TOKEN:token,CR_REQUIRE_AGENT_SIG:'false',CR_GUARD_ENABLED:'false',
    CR_REGISTRY_BACKEND:'memory',CR_SESSION_BACKEND:'jsonl',CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),
    CR_GROUP_ROOT:path.join(dir,'groups'),CR_PIDFILE:path.join(dir,'server.pid'),
    CR_FED_REQUIRE_PEER:'true',CR_FED_SELF_KEY_FILE:keyfile,CR_FED_RECORDS_FILE:records});
  const logPath = path.join(dir,'boot.log');
  const log = fs.openSync(logPath,'w');
  const proc = spawn(process.env.CRIER_TEST_BINARY || '/tmp/crier-391',[],{cwd:dir,env,stdio:['ignore',log,log]});
  const done = new Promise(resolve => proc.once('exit',resolve));
  processes.push({proc,done}); fs.closeSync(log);
  const base = 'http://127.0.0.1:'+port;
  for(let i=0;i<200;i++) {
    assert.equal(proc.exitCode,null,'scratch process exited: '+fs.readFileSync(logPath,'utf8'));
    try { if((await fetch(base+'/health')).ok) return {base,logPath}; } catch {}
    await new Promise(resolve => setTimeout(resolve,50));
  }
  throw new Error('readiness failed: '+logPath);
}
async function api(base,method,route,body) {
  const response = await fetch(base+route,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},body:body === undefined ? undefined : JSON.stringify(body)});
  const raw = await response.text();
  return {status:response.status,headers:response.headers,body:JSON.parse(raw)};
}
async function main() {
  fs.mkdirSync(evidence,{recursive:true});
  browser = await chromium.launch({executablePath:process.env.CHROME_BIN || '/usr/bin/google-chrome',headless:true,args:['--no-sandbox']});
  for(const linked of [false,true]) {
    const {base,logPath} = await boot(linked);
    assert.equal((await api(base,'POST','/agents',{id:'posture-test-operator'})).status,201);
    assert.equal((await api(base,'POST','/sessions',{title:'Federation posture verification',kind:'channel',created_by:{agent:'posture-test-operator'},members:[{member_type:'agent',member_id:'posture-test-operator'}]})).status,201);
    const inbox = await api(base,'POST','/agents/posture-test-operator/inbox',{payload:{text:'test-only posture probe'}});
    assert.equal(inbox.status,linked ? 403 : 201);
    assert.equal(inbox.headers.get('X-Crier-Fed-Inbound-Verdict'),linked ? 'FED_NO_LINK' : 'FED_TOKEN_ONLY');
    const status = (await api(base,'GET','/status')).body;
    assert.equal(status.federation_require_peer_configured,true);
    assert.equal(status.federation_require_peer_effective,linked);
    assert.equal(status.federation_peer_count,linked ? 1 : 0);
    const words = linked ? 'Federation: strict (1 peer)' : 'Federation: require-peer configured but inert (0 peers) — token-only inbound accepted';
    const context = await browser.newContext({extraHTTPHeaders:{Authorization:'Bearer '+token}});
    const page = await context.newPage(), errors = [];
    page.on('pageerror',error => errors.push(error.message));
    for(const width of [1280,390]) {
      await page.setViewportSize({width,height:1000});
      await page.goto(base+'/chat');
      await page.waitForFunction(words => document.getElementById('posture').textContent.includes(words),words);
      const text = await page.locator('#posture').innerText();
      assert.ok(text.includes(words),text);
      const overflow = await page.locator('#posture').evaluate(el => el.scrollWidth-el.clientWidth);
      assert.equal(overflow,0);
      const prefix = (linked ? 'strict' : 'inert')+'-'+width;
      await page.screenshot({path:path.join(evidence,prefix+'.png'),fullPage:true});
      await page.locator('.runtime-posture').screenshot({path:path.join(evidence,prefix+'-detail.png')});
      observations.push({deployment:linked ? 'linked' : 'empty',width,words,overflow,inboxStatus:inbox.status,
        configured:status.federation_require_peer_configured,effective:status.federation_require_peer_effective,
        peers:status.federation_peer_count,bootLine:fs.readFileSync(logPath,'utf8').split('\n').find(line => line.includes('federation inbound posture'))});
      console.log(JSON.stringify(observations.at(-1)));
    }
    assert.deepEqual(errors,[]);
    await context.close();
  }
  fs.writeFileSync(path.join(evidence,'evidence.json'),JSON.stringify(observations,null,2)+'\n');
  console.log('PASS: both deployments, real status/inbox/DOM, desktop/mobile, zero posture overflow and page errors');
}
main().catch(error => {console.error(error);process.exitCode=1;}).finally(async () => {
  if(browser) await browser.close();
  for(const {proc,done} of processes) {
    proc.kill('SIGTERM');
    let timer;
    await Promise.race([done,new Promise(resolve => {timer=setTimeout(() => {proc.kill('SIGKILL');resolve();},5000);})]);
    clearTimeout(timer);
    await done;
  }
});
