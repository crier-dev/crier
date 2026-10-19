// DF-CRIER-367: actual DOM + real open/locked routers, no synthetic authority.
// Called by TestChatClientComposerStateBrowser. Only a send response is delayed.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {generateKeyPairSync} = require('node:crypto');
let chromium;
try { ({chromium} = require('playwright')); }
catch (e) { if (e.code !== 'MODULE_NOT_FOUND') throw e; console.error('Playwright unavailable; set NODE_PATH'); process.exit(77); }
const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
if (!fs.existsSync(executablePath)) { console.error('Chromium unavailable at '+executablePath); process.exit(77); }
const [open,locked] = process.argv.slice(2);
const evidence = process.env.CHAT_COMPOSER_EVIDENCE;
const failures = [], observations = [];
const check = (label,fn) => { try { fn(); } catch (e) { failures.push(label+': '+e.message); } };
async function api(base,method,endpoint,body) {
  const r = await fetch(base+endpoint,{method,headers:{'Content-Type':'application/json'},body:body && JSON.stringify(body)});
  const b = await r.json();
  assert.ok(r.ok,method+' '+endpoint+' '+r.status+' '+JSON.stringify(b));
  return b;
}
async function run() {
  const browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  let releaseSend;
  try {
    const page = await browser.newPage({viewport:{width:1280,height:1000}});
    // Negative controls may serve a pinned historical/mutated HTML artifact;
    // APIs remain the real scratch routers and normal runs use embedded /chat.
    if (process.env.CHAT_COMPOSER_HTML) {
      const html = fs.readFileSync(process.env.CHAT_COMPOSER_HTML,'utf8');
      console.log('TEST-ONLY /chat HTML override: '+process.env.CHAT_COMPOSER_HTML);
      await page.route('**/chat',r => r.fulfill({contentType:'text/html',body:html}));
    }
    const errors = []; page.on('pageerror',e => errors.push(e.message));
    const idle = () => page.waitForFunction(() => !state.sending && !state.busy && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));
    async function observe(label,disabled,pattern) {
      const actual = await page.evaluate(() => {
        const text = document.getElementById('text'), row = document.getElementById('composer-why');
        const visible = !!row && row.getClientRects().length > 0 && getComputedStyle(row).visibility !== 'hidden';
        const reason = visible ? (document.getElementById('composer-reason') || row).textContent.trim() : '';
        return {disabled:text.disabled,sendDisabled:document.getElementById('send-button').disabled,reason,describedBy:text.getAttribute('aria-describedby') || '',draft:text.value};
      });
      observations.push({label,...actual}); console.log('COMPOSER '+label+' '+JSON.stringify(actual));
      check(label,() => {
        assert.equal(actual.disabled,Boolean(actual.reason),'disabled iff a non-empty reason is visibly rendered');
        assert.equal(actual.disabled,disabled,'expected disabled state');
        assert.equal(actual.sendDisabled,disabled,'send button must agree with textarea');
        assert.match(actual.reason,pattern);
        assert.ok(actual.describedBy.split(/\s+/).includes('composer-why'),'textarea must describe its visible reason');
      });
      return actual;
    }
    await page.goto(open+'/chat'); await idle();
    await observe('no identity',true,/Set an acting agent ID \(Connection & identity, left\) to enable sending\./);
    if (await page.locator('#composer-identity').count()) {
      await page.locator('#composer-identity').click();
      assert.equal(await page.locator('#agent').evaluate(el => el === document.activeElement),true,'inline identity action focuses the acting ID');
    } else failures.push('no identity: missing inline Set acting ID recovery');
    const operator = 'composer-test-operator', recipient = 'composer-test-recipient';
    for (const id of [operator,recipient]) {
      const {publicKey} = generateKeyPairSync('ed25519');
      const registration = {id,public_key:publicKey.export({type:'spki',format:'der'}).subarray(-32).toString('hex')};
      await api(open,'POST','/agents',registration);
      await api(locked,'POST','/agents',registration);
    }
    await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await idle();
    await observe('identity but no channel',true,/no channel open — pick or create one/i);
    if (await page.locator('#composer-pick').count()) {
      await page.locator('#composer-pick').click();
      assert.equal(await page.locator('#session-search').evaluate(el => el === document.activeElement),true,'inline pick action focuses conversation search');
    } else failures.push('no channel: missing inline Pick a channel recovery');
    for (const width of [390,768,1280]) {
      await page.setViewportSize({width,height:1000});
      const overflow = await page.evaluate(() => Math.max(document.documentElement.scrollWidth-document.documentElement.clientWidth,document.body.scrollWidth-document.body.clientWidth));
      assert.equal(overflow,0,'composer recovery must not overflow the page at '+width);
    }
    const recovery = page.locator('#composer-why button').filter({hasText:'Create channel'});
    check('inline recovery',() => assert.ok(observations.at(-1).reason));
    if (await recovery.count()) await recovery.click();
    else { failures.push('no channel: missing inline Create channel recovery'); await page.locator('#new-session').click(); }
    assert.equal(await page.locator('#create-form').isVisible(),true);
    await page.locator('#session-title').fill('Composer state regression');
    await page.locator('#initial-members').selectOption(recipient);
    await page.locator('#create-button').click();
    await page.waitForFunction(() => state.session?.state === 'open' && !state.sending && !state.busy);
    const id = await page.evaluate(() => state.current);
    await observe('ready',false,/^$/);
    if (evidence) {
      fs.mkdirSync(evidence,{recursive:true});
      await page.reload(); await idle();
      await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await idle();
      await page.screenshot({path:path.join(evidence,'chat-no-channel.png'),fullPage:true});
      await page.locator('.composer-wrap').screenshot({path:path.join(evidence,'chat-no-channel-detail.png')});
      await page.locator(`[data-session="${id}"]`).click(); await idle();
    }
    await page.locator('#recipient').selectOption(recipient);
    await page.locator('#text').fill('Keep this draft while recording');
    const held = new Promise(resolve => { releaseSend=resolve; });
    let observedSend;
    const sent = new Promise(resolve => { observedSend=resolve; });
    await page.route('**/sessions/*/messages',async route => {
      if (route.request().method() !== 'POST') return route.continue();
      const response = await route.fetch(); // real server accepted the real write
      observedSend(response.status());
      await held;
      await route.fulfill({response});
    });
    await page.locator('#send-button').click();
    assert.equal(await sent,201,'pending state must come from an accepted server request');
    const pending = await observe('send in progress',true,/in progress.*draft kept/i);
    assert.equal(pending.draft,'Keep this draft while recording');
    if (evidence) await page.locator('.composer-wrap').screenshot({path:path.join(evidence,'chat-sending-detail.png')});
    releaseSend(); await idle();
    await observe('send completed',false,/^$/);
    assert.equal(await page.locator('#text').inputValue(),'');
    assert.equal((await api(open,'GET',`/sessions/${id}/messages`)).messages.length,1,'only one message recorded');
    await page.goto(locked+'/chat'); await idle();
    const channel = await api(locked,'POST','/sessions',{title:'Locked composer',kind:'channel',created_by:{agent:operator},members:[{member_type:'agent',member_id:operator}]});
    await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await idle();
    await page.locator(`[data-session="${channel.id}"]`).click(); await idle();
    await observe('send authority missing',true,/agent signatures REQUIRED.*SESSION_SENDER_SIGNATURE_REQUIRED/);
    if (evidence) await page.locator('.composer-wrap').screenshot({path:path.join(evidence,'chat-authority-detail.png')});
    const refused = await fetch(locked+`/sessions/${channel.id}/messages`,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({sender:operator,payload:{text:'must refuse'}})});
    assert.equal(refused.status,401);
    assert.match(await refused.text(),/SESSION_SENDER_SIGNATURE_REQUIRED/);
    assert.equal((await api(locked,'GET',`/sessions/${channel.id}/messages`)).messages.length,0);
    // Unknown/stale policy is also unusable. Prove a failed refresh drops an old
    // permissive value, rather than accidentally keeping sends enabled.
    await page.goto(open+'/chat'); await idle();
    await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await idle();
    await page.locator(`[data-session="${id}"]`).click(); await idle();
    await page.route('**/status',r => r.fulfill({status:503,contentType:'application/json',body:'{"error":"test-only posture outage"}'}));
    await page.locator('#refresh').click(); await idle();
    await observe('posture unavailable',true,/authority.*unknown.*[Rr]efresh/);
    await page.unroute('**/status');
    if (await page.locator('#composer-refresh').count()) await page.locator('#composer-refresh').click();
    else { failures.push('unknown authority: missing inline Refresh recovery'); await page.locator('#refresh').click(); }
    await idle();
    await observe('posture recovered',false,/^$/);
    // Closed sessions have an explicit reason too; this supplemental state
    // injection is not presented as a server transition or a real close action.
    await page.evaluate(() => { state.session.state='closed'; updateComposer(); });
    await observe('closed channel (injected)',true,/closed.*[Pp]ick or create/);
    assert.deepEqual(errors,[]);
    if (evidence) fs.writeFileSync(path.join(evidence,'browser-results.json'),JSON.stringify({observations,failures,errors},null,2)+'\n');
    assert.deepEqual(failures,[],'composer state contract failures');
    console.log('PASS: composer disabled iff visible reason; four states, inline recovery, draft retention, signature refusal, stale policy and accessible description');
  } finally { if (releaseSend) releaseSend(); await browser.close(); }
}
run().catch(e => { console.error(e); process.exitCode=1; });
