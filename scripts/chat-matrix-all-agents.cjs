// DF-CRIER-374: real /agents + /sessions data and the served embedded client.
// Invoked by TestChatClientCapabilityMatrixAllAgentsBrowser; needs Playwright
// on NODE_PATH and CHROME_BIN (default /usr/bin/google-chrome).
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {generateKeyPairSync} = require('node:crypto');
let chromium;
try { ({chromium} = require('playwright')); }
catch (e) { if (e.code !== 'MODULE_NOT_FOUND') throw e; console.error('Playwright unavailable; set NODE_PATH to its node_modules directory'); process.exit(77); }
const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
if (!fs.existsSync(executablePath)) { console.error('Chromium unavailable at '+executablePath); process.exit(77); }
const base = process.argv[2];
const evidence = process.env.CHAT_MATRIX_EVIDENCE;
async function api(method, endpoint, body) {
  const response = await fetch(base+endpoint,{method,headers:{'Content-Type':'application/json'},body:body && JSON.stringify(body)});
  const json = await response.json();
  assert.ok(response.ok,method+' '+endpoint+': '+response.status+' '+JSON.stringify(json));
  return json;
}
async function run() {
  const browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  try {
    const ids = Array.from({length:6},(_,i) => 'matrix-test-agent-'+(i+1));
    for (const [i,id] of ids.entries()) {
      const {publicKey} = generateKeyPairSync('ed25519');
      const public_key = publicKey.export({type:'spki',format:'der'}).subarray(-32).toString('hex');
      await api('POST','/agents',{id,public_key,capabilities:i === 0 ? [] : ['matrix-cap-'+(i+1)]});
    }
    const session = await api('POST','/sessions',{title:'Six-agent matrix regression',kind:'channel',created_by:{agent:ids[0]},members:ids.map(member_id => ({member_type:'agent',member_id}))});
    const registry = await api('GET','/agents');
    assert.deepEqual(registry.agents.map(a => a.id).sort(),ids);
    const roster = await api('GET',`/sessions/${session.id}/participants`);
    assert.equal(roster.participants.length,6,'channel must really contain six participants');
    const page = await browser.newPage({viewport:{width:390,height:1000}});
    const errors = []; page.on('pageerror',e => errors.push(e.message));
    const idle = () => page.waitForFunction(() => !state.busy && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));
    await page.goto(base+'/chat'); await idle();
    await page.locator(`[data-session="${session.id}"]`).click(); await idle();
    const headers = () => page.locator('#capability-matrix-table thead th').allTextContents();
    const actual = (await headers()).slice(1).sort();
    console.log('MATRIX DOM agent ids: '+JSON.stringify(actual));
    assert.deepEqual(actual,ids,'matrix must render every agent, including the fifth and sixth');
    const legend = 'Declared capabilities only — crier records declarations and has no approval state, so "ask" is not shown.';
    assert.equal(await page.locator('#capability-matrix-legend').textContent(),legend);
    const row = page.locator('#capability-matrix-table tbody tr').filter({hasText:'matrix-cap-6'});
    assert.equal(await row.locator('td').count(),6);
    const columnOrder = (await headers()).slice(1);
    assert.deepEqual(await row.locator('td').allTextContents(),columnOrder.map(id => id === ids[5] ? '✓' : ''));
    assert.deepEqual(await row.locator('td').evaluateAll(els => els.map(el => el.getAttribute('aria-label'))),columnOrder.map(id => id === ids[5] ? 'Declared' : 'Not declared'));
    const measure = () => page.evaluate(() => {
      const box = document.getElementById('capability-matrix'), rect = box.getBoundingClientRect();
      const cols = [...box.querySelectorAll('thead th')].slice(1);
      const fullyVisible = cols.filter(col => { const r = col.getBoundingClientRect(); return r.left >= rect.left-1 && r.right <= rect.left+box.clientWidth+1; }).length;
      return {width:innerWidth,pageOverflow:document.documentElement.scrollWidth-document.documentElement.clientWidth,bodyOverflow:Math.max(0,document.body.scrollWidth-document.body.clientWidth),matrixOverflow:box.scrollWidth-box.clientWidth,fullyVisible,notice:document.getElementById('capability-matrix-more').textContent};
    });
    const checkNotice = async () => {
      await page.waitForFunction(() => {
        const box = document.getElementById('capability-matrix'), rect = box.getBoundingClientRect();
        const cols = [...box.querySelectorAll('thead th')].slice(1);
        const visible = cols.filter(col => { const r = col.getBoundingClientRect(); return r.left >= rect.left-1 && r.right <= rect.left+box.clientWidth+1; }).length;
        const text = document.getElementById('capability-matrix-more').textContent;
        return box.scrollWidth > box.clientWidth ? text.includes(`Showing ${visible} of ${cols.length}`) && text.includes('scroll the matrix') : text.includes(`All ${cols.length} agent`);
      });
    };
    const measurements = [];
    for (const width of [390,768,1280]) {
      await page.setViewportSize({width,height:1000}); await checkNotice();
      const result = await measure(); measurements.push(result);
      console.log('MATRIX MEASUREMENT '+JSON.stringify(result));
      assert.equal(result.pageOverflow,0,'page must not scroll horizontally at '+width);
      assert.equal(result.bodyOverflow,0,'body must not scroll horizontally at '+width);
      assert.ok(result.matrixOverflow > 0,'six readable columns must scroll inside the matrix at '+width);
      if (evidence) { fs.mkdirSync(evidence,{recursive:true}); await page.screenshot({path:path.join(evidence,'chat-matrix-six-'+width+'.png'),fullPage:true}); }
    }
    if (evidence) await page.locator('#capability-matrix-panel').screenshot({path:path.join(evidence,'chat-matrix-six-detail.png')});
    // Keyboard users can reach the scroll region; a scroll must update the count.
    assert.equal(await page.locator('#capability-matrix').getAttribute('tabindex'),'0');
    await page.locator('#capability-matrix').evaluate(el => { el.scrollLeft = el.scrollWidth; });
    await checkNotice();
    assert.equal(await page.locator('#capability-matrix-table thead th').last().evaluate(el => { const a = el.getBoundingClientRect(), b = el.closest('#capability-matrix').getBoundingClientRect(); return a.left >= b.left && a.right <= b.right+1; }),true,'last agent must be reachable by scrolling');
    console.log('MATRIX SCROLLED '+JSON.stringify(await measure()));
    const scrolled = await page.locator('#capability-matrix').evaluate(el => el.scrollLeft);
    await page.locator('#capability-matrix').focus();
    await page.locator('#capability-matrix').press('ArrowLeft');
    await page.waitForFunction(before => document.getElementById('capability-matrix').scrollLeft < before,scrolled);
    await checkNotice();
    // Resize the same element to exercise the all-visible branch without
    // replacing the registry or the production renderer.
    await page.setViewportSize({width:768,height:1000});
    await page.locator('#capability-matrix-panel').evaluate(el => { el.style.gridColumn='1 / -1'; });
    await checkNotice();
    const allVisible = await measure();
    assert.equal(allVisible.matrixOverflow,0); assert.equal(allVisible.fullyVisible,6);
    console.log('MATRIX ALL VISIBLE '+JSON.stringify(allVisible));
    // An all-undeclared registry must retain all headers, not erase the agents.
    const noDeclarations = {agents:registry.agents.map(a => ({...a,capabilities:[]}))};
    await page.route('**/agents',r => r.fulfill({contentType:'application/json',body:JSON.stringify(noDeclarations)}));
    await page.locator('#refresh').click(); await idle();
    assert.deepEqual((await headers()).slice(1).sort(),ids);
    assert.equal(await page.locator('#capability-matrix-table tbody tr').count(),0);
    assert.match(await page.locator('#capability-matrix').textContent(),/No capabilities declared/);
    await page.unroute('**/agents');
    // Registry refreshes must also discard old counts after data disappears.
    await page.route('**/agents',r => r.fulfill({contentType:'application/json',body:'{"agents":[]}'}));
    await page.locator('#refresh').click(); await idle();
    assert.equal(await page.locator('#capability-matrix-table').count(),0);
    assert.equal(await page.locator('#capability-matrix-more').textContent(),'');
    assert.match(await page.locator('#capability-matrix').textContent(),/No capabilities declared/);
    assert.deepEqual(errors,[]);
    if (evidence) fs.writeFileSync(path.join(evidence,'six-agent-measurements.json'),JSON.stringify({ids,measurements,allVisible},null,2));
    console.log('PASS: all six live agents, declaration cells, resize/scroll notice and page overflow');
  } finally { await browser.close(); }
}
run().catch(e => { console.error(e); process.exitCode=1; });
