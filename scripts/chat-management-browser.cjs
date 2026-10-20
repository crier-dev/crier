#!/usr/bin/env node
// DF-CRIER-400: actual stock-server reads, never product fixture rows.
// NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-400-server node scripts/chat-management-browser.cjs
// Optional CHAT_EVIDENCE directory; missing prerequisites exit 77, not PASS.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');
const evidence = process.env.CHAT_EVIDENCE || fs.mkdtempSync(path.join(os.tmpdir(),'crier-management-evidence-'));
fs.mkdirSync(evidence,{recursive:true});
const children = [], captures = [], errors = [];
let browser;
const routes = {'management-principals':'/principals','management-bindings':'/bindings','management-grants':'/grants','management-anchors':'/trust/anchors','management-peers':'/fed/peers'};
const adminPaths = ['/principals','/bindings','/grants','/trust/anchors'];
const keypair = () => crypto.generateKeyPairSync('ed25519');
const publicHex = pair => pair.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('hex');
async function start(armed, tokenConfigured = armed) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'crier-management-server-'));
  const probe = net.createServer();
  await new Promise((resolve,reject) => probe.once('error',reject).listen(0,'127.0.0.1',resolve));
  const port = probe.address().port;
  await new Promise(resolve => probe.close(resolve));
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env,{CRIER_PORT:String(port),CR_REQUIRE_AGENT_SIG:'false',CR_REGISTRY_BACKEND:'memory',CR_SESSION_BACKEND:'jsonl',CR_SESSION_LOG_ROOT:path.join(dir,'sessions'),CR_GROUP_ROOT:path.join(dir,'groups'),CR_RATE_LIMIT_PER_MINUTE:'0',CR_GUARD_ENABLED:'false',CR_FED_REQUIRE_PEER:'true'});
  const token = crypto.randomBytes(24).toString('hex');
  // With admin UNSET, lifecycle peer reads use ordinary reach access. This
  // yields the real 200/[] response rather than discovery's local-self row.
  Object.assign(env,{CR_FED_RECORDS_FILE:path.join(dir,'peers.json'),CR_FED_SELF_KEY_FILE:path.join(dir,'instance.key')});
  fs.writeFileSync(env.CR_FED_SELF_KEY_FILE,keypair().privateKey.export({format:'pem',type:'pkcs8'}),{mode:0o600});
  if (armed) Object.assign(env,{CR_PERMISSIONS_ENABLED:'true',CR_PERMISSIONS_ADMIN_TOKEN:tokenConfigured ? token : '',CR_PERMISSIONS_DIR:path.join(dir,'permissions')});
  else env.CR_AUTH_TOKEN = token;
  const fd = fs.openSync(path.join(dir,'server.log'),'w');
  const child = spawn(process.env.CRIER_TEST_BINARY || '/tmp/df-crier-400-server',[],{cwd:dir,env,stdio:['ignore',fd,fd]});
  children.push(child); fs.closeSync(fd);
  const base = 'http://127.0.0.1:'+port;
  for (let i=0;i<100;i++) {
    if (child.exitCode !== null || child.signalCode !== null) throw new Error('Scratch boot failed: '+fs.readFileSync(path.join(dir,'server.log'),'utf8'));
    try { if ((await fetch(base+'/health')).ok) return {base,token,dir}; } catch {}
    await new Promise(resolve => setTimeout(resolve,100));
  }
  throw new Error('Readiness failed: '+dir);
}
async function request(server,method,route,body,extra={}) {
  const response = await fetch(server.base+route,{method,headers:{'Content-Type':'application/json',...(server.token ? {Authorization:'Bearer '+server.token} : {}),...extra},body:body === undefined ? undefined : JSON.stringify(body)});
  const raw = await response.text();
  const result = raw ? JSON.parse(raw) : null;
  assert.ok(response.ok,method+' '+route+' '+response.status+' '+JSON.stringify(result));
  return result;
}
async function idle(page) {
  await page.waitForFunction(() => !document.querySelector('#management-refresh').disabled && !state.busy);
}
async function refresh(page) {
  await page.locator('#management-refresh').click(); await idle(page);
}
async function capture(page,label) {
  const panels = await page.evaluate(ids => Object.fromEntries(ids.map(id => [id,{state:document.getElementById(id).dataset.readState,text:document.getElementById(id).innerText} ])),[...Object.keys(routes),'management-class','management-chain','management-federation-posture']);
  captures.push({label,posture:await page.locator('#posture').innerText(),panels});
  fs.writeFileSync(path.join(evidence,'rendered-strings.json'),JSON.stringify(captures,null,2));
  console.log('RENDERED '+label+': '+JSON.stringify(panels));
}
async function submitClass(page,id) {
  await page.locator('#class-agent').fill(id); await page.locator('#class-query-submit').click();
  await page.waitForFunction(() => !document.getElementById('class-query-submit').disabled);
}
async function submitChain(page,query) {
  await page.locator('#trust-query').fill(JSON.stringify(query)); await page.locator('#trust-query-submit').click();
  await page.waitForFunction(() => !document.getElementById('trust-query-submit').disabled);
}
async function stateIs(page,id,expected) { assert.equal(await page.locator('#'+id).getAttribute('data-read-state'),expected,id); }
async function run() {
  let chromium;
  try { ({chromium} = require('playwright')); }
  catch (error) { if (error.code !== 'MODULE_NOT_FOUND') throw error; console.error('SKIP: Playwright unavailable; exit 77'); process.exitCode=77; return; }
  const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
  if (!fs.existsSync(executablePath)) { console.error('SKIP: Chromium unavailable; exit 77'); process.exitCode=77; return; }
  browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  const off = await start(false);
  const page = await browser.newPage({viewport:{width:1280,height:1000},extraHTTPHeaders:{Authorization:'Bearer '+off.token}});
  page.on('pageerror',error => errors.push(error.message));
  const counts = {};
  page.on('request',req => { const url = new URL(req.url()); counts[url.pathname] = (counts[url.pathname] || 0)+1; });
  await page.goto(off.base+'/chat'); await idle(page);
  for (const [id,route] of Object.entries(routes)) {
    const wire = await fetch(off.base+route,{headers:{Authorization:'Bearer '+off.token}}), body = await wire.json();
    if (route === '/fed/peers') {
      assert.equal(wire.status,200); assert.deepEqual(body.peers,[]);
      await stateIs(page,id,'empty');
      assert.equal(await page.locator('#'+id).innerText(),'Empty — GET /fed/peers succeeded. No federation peers returned (0 peers).');
    } else {
      assert.equal(wire.status,403); await stateIs(page,id,'refused');
      const text = await page.locator('#'+id).innerText();
      assert.ok(text.includes('\n'+body.detail+'\n'),text+' must preserve exact server detail');
      assert.ok(text.includes('GET '+route)); assert.ok(text.includes('CR_PERMISSIONS_ADMIN_TOKEN'));
      assert.equal(await page.locator('#'+id+' table').count(),0);
    }
  }
  await submitClass(page,'test-management-agent'); await stateIs(page,'management-class','refused');
  const classWire = await fetch(off.base+'/agents/test-management-agent/class',{headers:{Authorization:'Bearer '+off.token}});
  assert.ok((await page.locator('#management-class').innerText()).includes((await classWire.json()).detail));
  await submitChain(page,{signer:publicHex(keypair()),scope:[],chain:[]});
  assert.match(await page.locator('#management-chain').innerText(),/TRUST_UNKNOWN_ANCHOR/);
  assert.match(await page.locator('#management-chain').innerText(),/Basis: not reported by server/);
  assert.match(await page.locator('#management-federation-posture').innerText(),/Configured require-peer: true.*Effective require-peer: false.*Lifecycle peer records: 0/);
  await capture(page,'real-admin-unset-refusals-and-zero-peers');
  const disabledPosture = await page.locator('#posture').innerText();
  assert.ok(disabledPosture.includes('Management: disabled — delivery ACL off (CR_PERMISSIONS_ENABLED=false).'));
  assert.equal((await request(off,'GET','/status')).management_surface,'disabled');
  await page.locator('#posture').screenshot({path:path.join(evidence,'posture-disabled.png')});
  const before = {...counts};
  // Exercise the transcript refresh: it must not retry refused management reads.
  await page.locator('#refresh').click(); await idle(page);
  for (const route of Object.values(routes)) assert.equal(counts[route],before[route],'no automatic management retry for '+route);
  for (const width of [390,768,1280]) {
    await page.setViewportSize({width,height:1000});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth-window.innerWidth),0,'unarmed overflow at '+width);
    await page.screenshot({path:path.join(evidence,'management-refused-'+width+'.png'),fullPage:true});
  }
  await page.locator('.management').screenshot({path:path.join(evidence,'management-refused-detail.png')});
  // Real positive/empty controls with a scratch-only gateway adding an ephemeral
  // admin header to management URLs. Product code never receives a token value.
  // DF-CRIER-410: both unreadable ACL-live configurations, using actual API
  // responses. A configured token that this viewer did not supply is NOT off.
  for (const configured of [false,true]) {
    const unarmed = await start(true,configured);
    await page.goto(unarmed.base+'/chat'); await idle(page);
    const wire = await fetch(unarmed.base+'/status');
    assert.equal(wire.status,200);
    assert.equal((await wire.json()).management_surface,'enabled-unarmed');
    const posture = await page.locator('#posture').innerText();
    assert.ok(posture.includes('Management: enabled but not armed for this viewer — ACL live; management reads refused.'));
    assert.ok(posture.includes('CR_PERMISSIONS_ADMIN_TOKEN'));
    for (const id of ['management-principals','management-bindings','management-grants']) await stateIs(page,id,'refused');
    for (const route of ['/principals','/bindings','/grants']) {
      const refused = await fetch(unarmed.base+route);
      assert.equal(refused.status,403);
      assert.match((await refused.json()).detail,/CR_PERMISSIONS_ADMIN_TOKEN/);
    }
    for (const width of [390,768,1280]) {
      await page.setViewportSize({width,height:1000});
      const overflow = await page.evaluate(() => ({page:document.documentElement.scrollWidth-innerWidth,posture:document.getElementById('posture').scrollWidth-document.getElementById('posture').clientWidth}));
      assert.deepEqual(overflow,{page:0,posture:0},'ACL-live unreadable posture overflow at '+width);
    }
    const label = configured ? 'configured-token-not-supplied' : 'enabled-no-admin-configured';
    await capture(page,label);
    await page.screenshot({path:path.join(evidence,'posture-'+label+'-page.png'),fullPage:true});
    await page.locator('#posture').screenshot({path:path.join(evidence,'posture-'+label+'.png')});
  }
  const on = await start(true);
  await page.route(on.base+'/**',async route => {
    const url = new URL(route.request().url());
    const management = url.pathname === '/status' || adminPaths.includes(url.pathname) || /^\/agents\/[^/]+\/class$/.test(url.pathname) || url.pathname === '/fed/peers';
    await route.continue({headers:{...route.request().headers(),...(management ? {authorization:'Bearer '+on.token} : {})}});
  });
  await page.goto(on.base+'/chat'); await idle(page);
  for (const id of Object.keys(routes)) { await stateIs(page,id,'empty'); assert.equal(await page.locator('#'+id+' table').count(),0); }
  assert.equal((await request(on,'GET','/status')).management_surface,'admin-token-gated');
  assert.ok((await page.locator('#posture').innerText()).includes('Management: admin-token gated — enabled and gated; ACL live; management reads authorized for this viewer.'));
  await page.locator('#posture').screenshot({path:path.join(evidence,'posture-admin-gated.png')});
  await capture(page,'real-armed-empty-lists');
  const principal = await request(on,'POST','/principals',{display_name:'Browser test holder',namespace:'test-realm',role:'owner'});
  const agent = 'test-management-agent';
  await request(on,'POST','/bindings',{principal:principal.id,agent});
  const grant = await request(on,'POST','/grants',{principal:principal.id,subject:{type:'agent',ref:agent},actions:['send']});
  await request(on,'POST','/agents/'+agent+'/class',{class:'personal',owner:principal.id});
  const leaf = keypair(), moved = keypair();
  await request(on,'POST','/agents',{id:agent,public_key:publicHex(leaf)});
  const anchor = await request(on,'POST','/trust/anchors',{id:'browser-anchor',label:'Browser test anchor',public_key:publicHex(leaf)});
  const peer = await request(on,'POST','/fed/peers',{id:'browser-peer',public_key:publicHex(keypair()),url:'http://127.0.0.1:1',direction:'both',label:'Browser test peer'});
  await refresh(page); await submitClass(page,agent);
  for (const id of Object.keys(routes)) await stateIs(page,id,'populated');
  await stateIs(page,'management-class','populated');
  for (const [id,value] of [['management-principals',principal.id],['management-bindings',principal.id],['management-grants',grant.id],['management-anchors',anchor.id],['management-peers',peer.state],['management-class',principal.id]]) assert.ok((await page.locator('#'+id).innerText()).includes(value));
  assert.match(await page.locator('#management-federation-posture').innerText(),/Configured require-peer: true.*Effective require-peer: true.*Lifecycle peer records: 1/);
  const query = {signer:publicHex(leaf),scope:[],chain:[]};
  await submitChain(page,query);
  assert.match(await page.locator('#management-chain').innerText(),/TRUST_CHAIN_REACHABLE/);
  assert.match(await page.locator('#management-chain').innerText(),/Basis: chain-only/);
  await capture(page,'real-populated-and-chain-only');
  // Test-only signing gateway on the real read-only evaluator: no canned verdicts.
  // The UI still submits its textarea; the gateway attaches fresh exact-body proof.
  let proof = 'missing';
  await page.route(on.base+'/trust/verify',async route => {
    const raw = route.request().postData(), ts = String(Math.floor(Date.now()/1000));
    const digest = crypto.createHash('sha256').update(raw).digest('hex');
    const signature = crypto.sign(null,Buffer.from('POST\n/trust/verify\n'+ts+'\nsha256:'+digest),leaf.privateKey).toString('hex');
    await route.continue({headers:{...route.request().headers(),...(proof === 'missing' ? {} : {'x-agent-id':agent,'x-agent-ts':ts,'x-agent-sig':signature,'x-agent-body-sha256':digest})}});
  });
  query.challenge = 'browser-public-challenge';
  // An absent proof is proof-missing, not a cryptographic signature failure.
  // The separate trust-bases browser battery exercises an actual bad signature.
  for (const basis of ['proof-missing','possession-verified','proof-registry-key-mismatch']) {
    if (basis === 'possession-verified') proof = 'leaf';
    if (basis === 'proof-registry-key-mismatch') {
      await request(on,'DELETE','/agents/'+agent);
      await request(on,'POST','/agents',{id:agent,public_key:publicHex(moved)});
    }
    await submitChain(page,query);
    const text = await page.locator('#management-chain').innerText();
    assert.ok(text.includes('Basis: '+basis),text);
    assert.ok(text.includes(basis === 'possession-verified' ? 'TRUST_VERIFIED' : 'TRUST_CHAIN_REACHABLE'));
    assert.ok(text.includes(anchor.id)); await capture(page,'real-chain-'+basis);
  }
  for (const width of [390,768,1280]) {
    await page.setViewportSize({width,height:1000});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth-window.innerWidth),0,'populated overflow at '+width);
    for (const region of await page.locator('.management-table-wrap').all()) {
      const box = await region.boundingBox(); assert.ok(box.x >= 0 && box.x+box.width <= width);
      if (await region.evaluate(el => el.scrollWidth > el.clientWidth)) {
        await region.evaluate(el => el.scrollLeft = 0);
        await region.focus(); await page.keyboard.press('ArrowRight');
        await page.waitForFunction(el => el.scrollLeft > 0,await region.elementHandle());
        const reachable = await region.evaluate(el => {
          el.scrollLeft = el.scrollWidth;
          const last = el.querySelector('tbody tr:last-child td:last-child').getBoundingClientRect();
          const bounds = el.getBoundingClientRect();
          return last.right <= bounds.right+1 && last.left >= bounds.left-1;
        });
        assert.ok(reachable,'last management column reachable at '+width);
        await region.evaluate(el => el.scrollLeft = 0);
      }
    }
    await page.screenshot({path:path.join(evidence,'management-populated-'+width+'.png'),fullPage:true});
  }
  // Disclosed fault injection tests only error rendering, not server behavior.
  for (const status of [401,403,503,'network','malformed']) {
    const fault = async route => status === 'network' ? route.abort('failed') : route.fulfill({status:status === 'malformed' ? 200 : status,contentType:'application/json',body:JSON.stringify(status === 'malformed' ? {} : {error:'TEST_FAULT',detail:'Exact browser fault detail <img src=x onerror=alert(1)>.'})});
    for (const route of [...Object.values(routes),'/agents/'+agent+'/class','/trust/verify']) await page.route(on.base+route,fault);
    await refresh(page); await submitClass(page,agent); await submitChain(page,query);
    for (const id of [...Object.keys(routes),'management-class','management-chain']) {
      await stateIs(page,id,status === 401 || status === 403 ? 'refused' : 'error');
      assert.equal(await page.locator('#'+id+' table, #'+id+' img').count(),0);
      if (Number.isInteger(status)) assert.ok((await page.locator('#'+id).innerText()).includes('Exact browser fault detail <img src=x onerror=alert(1)>.'));
    }
    await capture(page,'injected-'+status);
    for (const route of [...Object.values(routes),'/agents/'+agent+'/class','/trust/verify']) await page.unroute(on.base+route,fault);
  }
  assert.deepEqual(errors,[]);
  fs.writeFileSync(path.join(evidence,'summary.json'),JSON.stringify({pageErrors:errors,viewports:[390,768,1280],overflow:0,realChecks:['three management postures: disabled, ACL-live/no-admin and unsupplied-admin refusals, authorized admin reads','admin unset: exact server refusals, zero peers, no automatic management retries','admin armed: empty then actual API-created records','real trust verdicts: all four bases, test-only signing gateway','configured true/effective false then true'],injections:['scratch admin gateway','fresh body-signature gateway for possession controls','401/403/503/network/malformed responses only in fault cells'],captures:captures.length},null,2));
  console.log('PASS: management browser; real refusals/empty/populated/trust bases; disclosed error cells; 390/768/1280 overflow 0. Evidence: '+evidence);
}
run().catch(error => {console.error(error); process.exitCode=1;}).finally(async () => {
  if (browser) await browser.close();
  for (const child of children) {
    child.kill('SIGTERM');
    await Promise.race([new Promise(resolve => child.once('exit',resolve)),new Promise(resolve => setTimeout(resolve,2000))]);
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
  }
});
