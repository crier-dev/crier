#!/usr/bin/env node
// DF-CRIER-486: exercise the shipped renderer against registry-backed and
// missing/unavailable /agents responses. Synthetic IDs are test-only fixtures.
const assert = require('node:assert/strict');
const fs = require('node:fs');
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
const html = fs.readFileSync(path.join(__dirname,'../cmd/server/chat.html'),'utf8');
assert.doesNotMatch(html,/Math\.random\(\).*suggest|SUGGESTED_ID\s*=\s*["']browser-/,
  'negative control: the shipped page must not mint a random identity suggestion');
const registered = [{id:'identity-fixture-registered',namespace:'default'},{id:'another-registered',namespace:'default'}];
let registryMode = 'registered';
let agentPosts = 0;
let browser;
(async () => {
  browser = await chromium.launch({executablePath,headless:true,args:['--no-sandbox']});
  const page = await browser.newPage();
  await page.route('http://identity.test/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/chat') return route.fulfill({contentType:'text/html',body:html});
    if (url.pathname === '/agents' && route.request().method() === 'GET') {
      if (registryMode === 'unavailable') return route.fulfill({status:503,json:{error:'registry fixture unavailable'}});
      return route.fulfill({json:{agents:registryMode === 'registered' ? registered : []}});
    }
    if (url.pathname === '/agents' && route.request().method() === 'POST') agentPosts++;
    const bodies = {
      '/sessions':{sessions:[]},
      '/status':{health:'ok',require_agent_signature:false,management_surface:'disabled'},
      '/groups':{groups:[]},
    };
    return route.fulfill({json:bodies[url.pathname] || {}});
  });
  const open = async () => {
    await page.goto('http://identity.test/chat');
    await page.waitForFunction(() => !state.busy && !document.getElementById('identity-suggestion-status').textContent.startsWith('Loading'));
  };
  await open();
  const button = page.locator('#use-suggested');
  assert.equal(await button.isVisible(),true,'suggestion must be offered for a returned registry row');
  assert.equal(await page.locator('#suggested-id').innerText(),registered[0].id,'suggestion must exactly match /agents row');
  assert.match(await button.getAttribute('aria-label'),new RegExp('Use registered agent '+registered[0].id));
  assert.match(await page.locator('#identity-gate').innerText(),/registered agent/i);
  await button.click();
  await page.waitForFunction(id => state.agent === id && !state.busy,registered[0].id);
  assert.equal(await page.locator('#agent').inputValue(),registered[0].id,'Use action applies the registry-backed ID');
  assert.equal(agentPosts,0,'the suggestion path does not claim to register an identity');
  console.log('PASS registered registry row: exact ID provenance, labelled control, identity apply, no registration claim');

  registryMode = 'empty';
  await open();
  assert.equal(await button.isVisible(),false,'empty registry must not expose a fabricated suggestion');
  assert.equal(await page.locator('#suggested-id').innerText(),'');
  assert.match(await page.locator('#identity-suggestion-status').innerText(),/No registered agent is available/);
  assert.match(await page.locator('#identity-suggestion-status').innerText(),/this page does not register IDs/);
  assert.equal(await page.locator('#agent').inputValue(),'','no fresh ID is silently minted');
  assert.equal(agentPosts,0);
  console.log('PASS empty registry: no fabricated ID, explicit no-agent status, no register claim');

  registryMode = 'unavailable';
  await open();
  assert.equal(await button.isVisible(),false,'unavailable registry must not expose an unverified suggestion');
  assert.equal(await page.locator('#suggested-id').innerText(),'');
  assert.match(await page.locator('#identity-suggestion-status').innerText(),/could not be loaded/);
  assert.match(await page.locator('#identity-suggestion-status').innerText(),/Enter an existing agent ID only/);
  assert.equal(await page.locator('#agent').inputValue(),'');
  assert.equal(agentPosts,0);
  console.log('PASS unavailable registry: read failure is explicit and no unregistered value is suggested');
  console.log('PASS: DF-CRIER-486 shipped-renderer identity suggestion provenance checks');
})().catch(error => { console.error(error); process.exitCode=1; }).finally(async () => { if (browser) await browser.close(); });
