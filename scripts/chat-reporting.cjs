// DF-CRIER-377/378: real posture from scratch memory/sqlite servers; outcome
// reporting uses disclosed browser-intercepted responses, never runtime claims.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
module.exports = async function reporting(page,base,evidence,start,session,operator,agent) {
  const idle = () => page.waitForFunction(() => !state.sending && !state.busy);
  const refresh = async () => { await page.locator('#refresh').click(); await idle(); };
  const memory = await (await fetch(base+'/status')).json();
  assert.equal(memory.registry_backend,'memory'); assert.equal(memory.registry_durable,false);
  assert.equal(memory.session_backend,'jsonl'); assert.equal(memory.auth_enabled,false); assert.equal(memory.a2a_enabled,false);
  await page.locator('.identity').evaluate(el => el.open=false);
  assert.equal(await page.locator('#posture').isVisible(),true);
  assert.match(await page.locator('#health').innerText(),/Health: ok.*agents are lost on restart/);
  const memoryLine = await page.locator('#posture').innerText();
  assert.match(memoryLine,/Registry: memory · non-durable — agents are lost on restart · Session store: jsonl · Auth: disabled · A2A: disabled · Build: /);
  assert.ok(memoryLine.includes(memory.build.version+' / '+memory.build.commit));
  await page.screenshot({path:path.join(evidence,'posture-memory.png'),fullPage:true});
  await page.locator('.runtime-posture').screenshot({path:path.join(evidence,'posture-memory-detail.png')});
  // A second actual server, same binary, independently selected sqlite registry.
  const sqliteBase = await start('', 'sqlite');
  const sqlite = await (await fetch(sqliteBase+'/status')).json();
  assert.equal(sqlite.registry_backend,'sqlite'); assert.equal(sqlite.registry_durable,true);
  const sqlPage = await page.context().newPage();
  await sqlPage.goto(sqliteBase+'/chat');
  await sqlPage.waitForFunction(() => !state.busy);
  const sqliteLine = await sqlPage.locator('#posture').innerText();
  assert.match(sqliteLine,/Registry: sqlite · durable · Session store: jsonl · Auth: disabled · A2A: disabled/);
  assert.doesNotMatch(await sqlPage.locator('#health').innerText(),/lost on restart/);
  await sqlPage.screenshot({path:path.join(evidence,'posture-sqlite.png'),fullPage:true});
  await sqlPage.close();
  const measurements = [];
  for (const width of [390,768,1280]) {
    await page.setViewportSize({width,height:1000});
    await page.route('**/status',route => route.fulfill({contentType:'application/json',body:JSON.stringify({...memory,registry_backend:'future-server-reported-backend',registry_durable:true,auth_enabled:true,a2a_enabled:true,build:{version:'long-build-version-'+ 'x'.repeat(80),commit:'a'.repeat(40)}})}));
    await refresh();
    assert.match(await page.locator('#posture').innerText(),/future-server-reported-backend · durable.*Auth: enabled · A2A: enabled/);
    const measure = await page.evaluate(() => ({viewport:innerWidth,overflow:Math.max(document.documentElement.scrollWidth,document.body.scrollWidth)-innerWidth,postureOverflow:document.getElementById('posture').scrollWidth-document.getElementById('posture').clientWidth}));
    assert.equal(measure.overflow,0,JSON.stringify(measure)); assert.equal(measure.postureOverflow,0,JSON.stringify(measure));
    measurements.push(measure);
    await page.screenshot({path:path.join(evidence,`posture-${width}.png`),fullPage:true});
    await page.unroute('**/status');
  }
  await page.route('**/status',route => route.fulfill({contentType:'application/json',body:'{}'}));
  await refresh();
  assert.equal(await page.locator('#posture').innerText(),'Registry: unknown · durability: unknown · Session store: unknown · Auth: unknown · A2A: unknown · Build: unknown / unknown');
  assert.match(await page.locator('#health').innerText(),/^Health: unknown/);
  await page.unroute('**/status');
  await page.route('**/status',route => route.fulfill({status:503,contentType:'application/json',body:'{"error":"posture unavailable"}'}));
  await refresh(); assert.match(await page.locator('#posture').innerText(),/Runtime posture: unknown.*posture unavailable/);
  await page.unroute('**/status'); await refresh();
  await page.setViewportSize({width:1280,height:1000});
  await page.locator('.identity').evaluate(el => el.open=true);
  await page.locator('#agent').fill(operator); await page.locator('#apply-identity').click(); await idle();
  await page.locator(`[data-session="${session.id}"]`).click(); await idle();
  await page.locator('#recipient').selectOption(agent);
  const outcomes = [];
  for (const outcome of ['delivered','leased','acked','expired','refused','failed','canned','budget_cut','unreadable_reply']) {
    const result = {outcomes:[{target:agent,outcome}]};
    await page.route(`**/sessions/${session.id}/messages`,async route => {
      if (route.request().method()==='POST') return route.fulfill({contentType:'application/json',body:JSON.stringify(result)});
      const response = await route.fetch(), body = await response.json();
      body.messages[0].outcomes=result.outcomes;
      await route.fulfill({response,json:body});
    });
    const draft = 'Reporting fixture '+outcome;
    await page.locator('#text').fill(draft); await page.locator('#send-button').click(); await idle();
    const line = await page.locator('#send-result').innerText();
    assert.match(line,new RegExp('Message recorded; '+outcome+': 1\\.'));
    if (outcome!=='delivered') assert.doesNotMatch(line,/delivered to|; delivered:/);
    const negative = ['refused','failed','canned','budget_cut','unreadable_reply'].includes(outcome);
    assert.equal(await page.locator('#send-result').evaluate(el=>el.classList.contains('error')),negative);
    assert.equal(await page.locator('#text').inputValue(),negative?draft:'');
    assert.equal(await page.locator('#transcript .outcomes span').first().evaluate(el=>el.classList.contains('error')),negative);
    assert.equal(await page.locator('#transcript .outcomes span').first().innerText(),agent+': '+(outcome==='refused'?'delivery refused':outcome));
    outcomes.push({outcome,line,negative,draftKept:negative});
    if (outcome==='unreadable_reply') {
      await page.screenshot({path:path.join(evidence,'outcome-unreadable-reply.png'),fullPage:true});
      await page.locator('.composer-wrap').screenshot({path:path.join(evidence,'outcome-draft-detail.png')});
    }
    await page.unroute(`**/sessions/${session.id}/messages`);
  }
  await page.locator('#text').fill(''); await page.locator('#recipient').selectOption('');
  // Restore clean attribution state for the original full browser suite.
  await page.locator('#agent').fill(''); await page.locator('#apply-identity').click(); await idle();
  const report = {memoryLine,sqliteLine,measurements,outcomes,method:'Real memory/sqlite status; future/missing/error status and outcome responses explicitly intercepted for reporting tests'};
  fs.writeFileSync(path.join(evidence,'reporting-results.json'),JSON.stringify(report,null,2));
  console.log('BROWSER VERIFIED: runtime posture memory/sqlite, unknown/error fields, 390/768/1280 overflow=0; exact outcome vocabulary, negative styling, draft retention (intercepted outcomes)');
};
