// DF-CRIER-360 browser regressions. Test-only registry responses; no product fixtures.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
module.exports = async function capabilityMatrix(page, evidence) {
  const legend = 'Declared capabilities only — crier records declarations and has no approval state, so "ask" is not shown.';
  const base = await page.evaluate(async () => (await fetch('/agents')).json());
  let payload = base;
  const route = r => r.fulfill({contentType:'application/json',body:JSON.stringify(payload)});
  const refresh = async () => {
    await page.locator('#refresh').click();
    await page.waitForFunction(() => !state.busy && !state.sending);
  };
  const rows = () => page.locator('#capability-matrix-table tbody th').allTextContents();
  const cells = () => page.locator('#capability-matrix-table tbody td').allTextContents();
  await page.route('**/agents',route);
  try {
    assert.equal(await page.locator('#capability-matrix-legend').textContent(),legend);
    assert.deepEqual((await rows()).sort(),[...new Set(base.agents.flatMap(a => a.capabilities || []))].sort());
    const before = await page.locator('#capability-matrix-table tbody tr').count();
    const added = {id:'ui-test-new-agent',capabilities:['ui-test-new-capability']};
    payload = {agents:[...base.agents,added]};
    await refresh();
    assert.equal(await page.locator('#capability-matrix-table tbody tr').count(),before+1);
    assert.ok((await rows()).includes('ui-test-new-capability'));
    const row = page.locator('#capability-matrix-table tbody tr').filter({hasText:'ui-test-new-capability'});
    assert.deepEqual(await row.locator('td').allTextContents(),[...base.agents.map(() => ''),'✓']);
    // Every cell is declaration-only, never an approval or denial state.
    assert.ok((await cells()).every(text => text === '' || text === '✓'));
    assert.deepEqual(await row.locator('td').evaluateAll(els => els.map(el => el.getAttribute('aria-label'))),[...base.agents.map(() => 'Not declared'),'Declared']);
    assert.equal(await page.locator('#identity-gate').isVisible(),true);
    assert.equal(await page.locator('#send-button').isDisabled(),true);
    for (const width of [390,768,1280]) {
      await page.setViewportSize({width,height:1000});
      await page.screenshot({path:path.join(evidence,'chat-matrix-populated-'+width+'.png'),fullPage:true});
    }
    await page.locator('#capability-matrix-panel').screenshot({path:path.join(evidence,'chat-matrix-populated-detail.png')});
    // Every registry column remains present. Long registry values and HTML-like
    // declarations must be text, not DOM markup, even off the scroll viewport.
    const hostile = '<img src=x onerror="window.__matrixInjected=true">';
    const long = 'ui-test-capability-'+'x'.repeat(120);
    payload = {agents:Array.from({length:10},(_,i) => ({id:'ui-test-agent-'+i+'-'+'z'.repeat(40),capabilities:i === 9 ? ['ui-test-hidden-column-capability',hostile,long] : ['ui-test-common']})).concat({id:'foreign-agent',namespace:'other-realm',capabilities:['ui-test-foreign-capability']})};
    await refresh();
    assert.equal(await page.locator('#capability-matrix-table thead th[scope="col"]').count(),11); // row label + all ten agents
    assert.match(await page.locator('#capability-matrix-more').textContent(),/Showing \d+ of 10 agent columns fully — scroll the matrix/);
    assert.ok((await rows()).includes('ui-test-hidden-column-capability'));
    assert.ok(!(await rows()).includes('ui-test-foreign-capability'));
    assert.ok((await rows()).includes(hostile));
    assert.equal(await page.locator('#capability-matrix img, #capability-matrix [onerror]').count(),0);
    assert.equal(await page.evaluate(() => window.__matrixInjected),undefined);
    const hiddenRow = page.locator('#capability-matrix-table tbody tr').filter({hasText:'ui-test-hidden-column-capability'});
    assert.deepEqual(await hiddenRow.locator('td').allTextContents(),[...Array(9).fill(''),'✓']);
    assert.ok((await cells()).every(text => text === '' || text === '✓'));
    const measurements = [];
    for (const width of [390,768,1280]) {
      await page.setViewportSize({width,height:1000});
      const result = await page.evaluate(() => {
        const gap = el => Math.max(0,el.scrollWidth-el.clientWidth);
        return {width:innerWidth,pageOverflow:gap(document.documentElement),panelOverflow:gap(document.getElementById('capability-matrix-panel')),matrixOverflow:gap(document.getElementById('capability-matrix')),tableOverflow:gap(document.getElementById('capability-matrix-table'))};
      });
      assert.equal(result.pageOverflow,0); assert.equal(result.panelOverflow,0); assert.equal(result.tableOverflow,0);
      assert.ok(result.matrixOverflow > 0,'scrolling belongs inside the matrix, not the page');
      measurements.push(result);
      await page.screenshot({path:path.join(evidence,'chat-matrix-'+width+'.png'),fullPage:true});
    }
    await page.locator('#capability-matrix-panel').screenshot({path:path.join(evidence,'chat-matrix-detail.png')});
    // Empty results and registry errors must clear the previous matrix, not
    // imply denials or leave declarations from the previous realm visible.
    payload = {agents:[]}; await refresh();
    assert.match(await page.locator('#capability-matrix').textContent(),/No capabilities declared/);
    assert.equal(await page.locator('#capability-matrix-table').count(),0);
    assert.equal(await page.locator('#capability-matrix-more').textContent(),'');
    await page.unroute('**/agents',route);
    await page.route('**/agents',r => r.fulfill({status:403,contentType:'application/json',body:JSON.stringify({error:{code:'TEST_REGISTRY_REFUSAL',message:'Registry read refused'}})}));
    await refresh();
    assert.match(await page.locator('#capability-matrix').textContent(),/Unavailable — HTTP 403: TEST_REGISTRY_REFUSAL: Registry read refused/);
    assert.equal(await page.locator('#capability-matrix-table').count(),0);
    assert.equal(await page.locator('#capability-matrix-legend').textContent(),legend);
    const report = {legend,agentColumns:10,addedCapability:'ui-test-new-capability',measurements};
    fs.writeFileSync(path.join(evidence,'capability-matrix-results.json'),JSON.stringify(report,null,2));
    console.log('PASS: declared-capability matrix '+JSON.stringify(report));
  } finally {
    await page.unroute('**/agents');
    await refresh();
    await page.setViewportSize({width:1280,height:1000});
  }
};
