#!/usr/bin/env node
// DF-CRIER-423: real trust HTTP responses rendered by the shipped chat page.
// The scratch gateway adds test proof headers; this is NOT browser signing.
// NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-423-server node scripts/chat-trust-bases-browser.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const evidence = process.env.CHAT_EVIDENCE || path.resolve('docs/screenshots/df-crier-423');
let child, browser;
async function run() {
  let chromium;
  try { ({chromium} = require('playwright')); }
  catch (error) { if (error.code !== 'MODULE_NOT_FOUND') throw error; console.error('SKIP: Playwright missing'); process.exitCode=77; return; }
  const binary = process.env.CRIER_TEST_BINARY;
  assert.ok(binary && fs.existsSync(binary), 'CRIER_TEST_BINARY must select the rebuilt server');
  fs.mkdirSync(evidence,{recursive:true});
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'df-crier-423-'));
  const listener = net.createServer();
  await new Promise((resolve,reject) => listener.once('error',reject).listen(0,'127.0.0.1',resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const base = 'http://127.0.0.1:'+port;
  const admin = crypto.randomBytes(24).toString('hex');
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('CR_') && !key.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(port),CR_REQUIRE_AGENT_SIG:'false',CR_REGISTRY_BACKEND:'memory',CR_SESSION_BACKEND:'jsonl',CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),CR_GROUP_ROOT:path.join(dir,'groups'),CR_PERMISSIONS_DIR:path.join(dir,'permissions'),CR_PERMISSIONS_ADMIN_TOKEN:admin,CR_RATE_LIMIT_PER_MINUTE:'0',CR_GUARD_ENABLED:'false'});
  const fd = fs.openSync(path.join(dir,'server.log'),'w');
  child = spawn(binary,[],{cwd:dir,env,stdio:['ignore',fd,fd]});
  fs.closeSync(fd);
  let ready = false;
  for (let i=0;i<100;i++) {
    assert.equal(child.exitCode,null,'scratch server exited: '+fs.readFileSync(path.join(dir,'server.log'),'utf8'));
    try { if ((await fetch(base+'/health')).ok) { ready=true; break; } } catch {}
    await new Promise(resolve => setTimeout(resolve,100));
  }
  assert.ok(ready,'scratch server not ready: '+dir);
  const key = crypto.generateKeyPairSync('ed25519');
  const publicKey = key.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('hex');
  async function post(route,body,headers={}) {
    const response = await fetch(base+route,{method:'POST',headers:{'content-type':'application/json',...headers},body:JSON.stringify(body)});
    const result = await response.json();
    assert.ok(response.ok,route+' '+response.status+' '+JSON.stringify(result));
    return result;
  }
  await post('/agents',{id:'proof-agent',public_key:publicKey});
  await post('/trust/anchors',{id:'browser-anchor',label:'Scratch browser evidence',public_key:publicKey,created_at:new Date().toISOString()},{authorization:'Bearer '+admin});
  browser = await chromium.launch({executablePath:process.env.CHROME_BIN || '/usr/bin/google-chrome',headless:true,args:['--no-sandbox']});
  const cases = [
    {basis:'proof-stale-timestamp',agent:'proof-agent',offset:-120,invalid:false},
    {basis:'proof-no-registry-key',agent:'unknown-proof-agent',offset:0,invalid:false},
    {basis:'proof-invalid-signature',agent:'proof-agent',offset:0,invalid:true}
  ];
  const results = [], errors = [];
  for (const width of [390,1280]) {
    const page = await browser.newPage({viewport:{width,height:1000}});
    page.on('pageerror',error => errors.push(error.message));
    let current, observed;
    await page.route(base+'/trust/verify',async route => {
      const request = route.request();
      const digest = crypto.createHash('sha256').update(request.postDataBuffer()).digest('hex');
      const ts = String(Math.floor(Date.now()/1000)+current.offset);
      const payload = Buffer.from('POST\n/trust/verify\n'+ts+'\nsha256:'+digest);
      const sig = current.invalid ? Buffer.alloc(64).toString('hex') : crypto.sign(null,payload,key.privateKey).toString('hex');
      // Real server evaluation with test-only signed gateway headers.
      const response = await route.fetch({headers:{...request.headers(),'x-agent-id':current.agent,'x-agent-ts':ts,'x-agent-sig':sig,'x-agent-body-sha256':digest}});
      observed = await response.json();
      assert.equal(response.status(),200);
      assert.equal(observed.basis,current.basis);
      await route.fulfill({response});
    });
    await page.goto(base+'/chat');
    await page.waitForFunction(() => !document.getElementById('management-refresh').disabled);
    const html = await (await fetch(base+'/chat')).text();
    const words = JSON.parse(html.match(/const basisWords = (\{[^\n]*\});/)[1]);
    for (current of cases) {
      await page.locator('#trust-query').fill(JSON.stringify({signer:publicKey,scope:[],chain:[],challenge:'scratch-browser-proof'}));
      await page.locator('#trust-query-submit').click();
      await page.waitForFunction(() => !document.getElementById('trust-query-submit').disabled);
      const sentence = await page.locator('#management-chain > p').innerText();
      assert.equal(sentence,'Basis: '+current.basis+' — '+words[current.basis]);
      const overflow = await page.evaluate(() => {
        const p = document.querySelector('#management-chain > p');
        return {page:document.documentElement.scrollWidth-innerWidth,sentence:p.scrollWidth-p.clientWidth};
      });
      assert.deepEqual(overflow,{page:0,sentence:0},'overflow at '+width);
      const label = current.basis+'-'+width;
      const screenshot = path.join(evidence,label+'.png');
      const detail = path.join(evidence,label+'-detail.png');
      await page.screenshot({path:screenshot,fullPage:true});
      await page.locator('section[aria-labelledby="anchors-title"]').screenshot({path:detail});
      results.push({width,basis:current.basis,source:'real server response; test-only proof gateway',response:observed,sentence,overflow,screenshot:path.basename(screenshot),detail:path.basename(detail)});
      console.log(JSON.stringify(results.at(-1)));
    }
    await page.close();
  }
  assert.deepEqual(errors,[],'browser errors');
  fs.writeFileSync(path.join(evidence,'rendered-bases.json'),JSON.stringify(results,null,2)+'\n');
}
run().catch(error => { console.error(error); process.exitCode=1; }).finally(async () => {
  if (browser) await browser.close();
  if (child && child.exitCode === null && child.signalCode === null) { child.kill('SIGTERM'); await once(child,'exit'); }
});
