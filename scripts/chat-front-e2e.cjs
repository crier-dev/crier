// E2E: is the Crier UI USABLE through the shipped authenticating front?
// Proves: direct 401, fronted 200, a real send over the front, and the shared
// token absent from page source, storage and cookies.
// DF-CRIER-333: does the shipped authenticating front make /chat usable?
// rc 0 pass, 1 regression, 77 named skip when the browser stack is absent.
let chromium;
try {
  ({ chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright'));
} catch {
  console.log('SKIP 77: playwright not installed (set PLAYWRIGHT_PATH to a playwright module)');
  process.exit(77);
}
const fs = require('fs');

const BACK = process.env.CRIER_BACK || 'http://127.0.0.1:18995';   // relay with CR_AUTH_TOKEN set
const FRONT = process.env.CRIER_FRONT || 'http://127.0.0.1:18996';  // shipped crier-front
const TOKEN = process.env.CRIER_E2E_TOKEN;
const SHOTS = process.env.SHOTS || 'e2e/shots';
fs.mkdirSync(SHOTS, { recursive: true });

const out = { checks: [], errors: [] };
const rec = (name, data) => { out.checks.push({ name, ...data }); console.log('CHECK', name, JSON.stringify(data).slice(0, 600)); };

let browserRef = null;
(async () => {
  if (!TOKEN) throw new Error('CRIER_E2E_TOKEN unset');
  const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH || '/usr/bin/google-chrome', args: ['--no-sandbox'] });
  browserRef = browser;
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  page.on('pageerror', e => out.errors.push(e.message));

  // 1. Direct navigation, no credential: must be refused, and the refusal must be legible.
  const direct = await page.goto(BACK + '/chat', { waitUntil: 'domcontentloaded' });
  const directBody = (await page.evaluate(() => document.body.innerText)).trim();
  rec('direct-no-credential', {
    status: direct.status(),
    legible: /front|401|authoriz|token/i.test(directBody),
    excerpt: directBody.slice(0, 220),
  });
  await page.screenshot({ path: `${SHOTS}/01-direct-401.png` });

  // 2. Same browser through the front: the panel must load.
  const fronted = await page.goto(FRONT + '/chat', { waitUntil: 'networkidle' });
  await page.waitForTimeout(1200);
  const panel = await page.evaluate(() => ({
    status: null,
    title: document.title,
    hasComposer: !!document.querySelector('#text'),
    hasSidebar: !!document.querySelector('#conversation-list') || !!document.querySelector('[data-session]') || !!document.querySelector('.sidebar'),
    bodyLen: document.body.innerText.length,
  }));
  rec('fronted-panel', { httpStatus: fronted.status(), ...panel });
  await page.screenshot({ path: `${SHOTS}/02-fronted-panel.png` });

  // 3. Register agents THROUGH the front (proves the front carries the credential for API calls too).
  const stamp = Date.now().toString(36).slice(-5);
  const agents = [`e2e-alpha-${stamp}`, `e2e-bravo-${stamp}`];
  for (const id of agents) {
    const r = await page.evaluate(async ({ base, id }) => {
      const res = await fetch(base + '/agents', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, capabilities: [] }),
      });
      return { status: res.status, text: (await res.text()).slice(0, 200) };
    }, { base: FRONT, id });
    rec(`register-${id}`, r);
  }

  // 4. Create a channel and send a message from the shipped UI.
  await page.fill('#agent', agents[0]);
  await page.click('#apply-identity');
  await page.waitForTimeout(900);
  await page.click('#new-session');
  await page.waitForTimeout(250);
  await page.fill('#session-title', 'fronted e2e channel');
  await page.evaluate((member) => {
    const sel = document.querySelector('#initial-members');
    if (!sel) return;
    for (const o of sel.options) if (o.value === member) o.selected = true;
  }, agents[1]);
  await page.click('#create-button');
  await page.waitForTimeout(1800);
  await page.fill('#text', 'sent through the shipped authenticating front');
  await page.click('#send-button');
  await page.waitForTimeout(2500);
  const sendNotice = await page.evaluate(() => (document.querySelector('#send-result')?.innerText || '').trim());
  const transcript = await page.evaluate(() => (document.querySelector('#transcript')?.innerText || '').trim());
  rec('send-through-front', {
    notice: sendNotice.slice(0, 300),
    accepted: /accepted:\s*[1-9]/.test(sendNotice),
    transcriptHasMessage: transcript.includes('sent through the shipped authenticating front'),
  });
  await page.screenshot({ path: `${SHOTS}/03-fronted-send.png` });

  // 5. The credential must be invisible to the page.
  const storage = await page.evaluate((token) => {
    const ls = { ...localStorage }, ss = { ...sessionStorage };
    const dump = JSON.stringify({ ls, ss }) + document.documentElement.outerHTML + document.cookie;
    return {
      inLocalStorage: JSON.stringify(ls).includes(token),
      inSessionStorage: JSON.stringify(ss).includes(token),
      inCookie: document.cookie.includes(token),
      inPageSource: document.documentElement.outerHTML.includes(token),
      storageKeys: Object.keys(ls).concat(Object.keys(ss)).slice(0, 8),
      anyLeak: dump.includes(token),
    };
  }, TOKEN);
  rec('token-not-in-page', storage);

  // 6. Confirm the relay is genuinely auth-protected (not accidentally open).
  // Done from Node, not the page: a cross-origin fetch from the fronted page to
  // the relay is refused by CORS, which would measure the browser, not the relay.
  const relayNoToken = await fetch(BACK + '/sessions');
  const relayWrongToken = await fetch(BACK + '/sessions', { headers: { Authorization: 'Bearer wrong-token' } });
  const relayRightToken = await fetch(BACK + '/sessions', { headers: { Authorization: 'Bearer ' + TOKEN } });
  rec('relay-auth-boundary', {
    noToken: relayNoToken.status,
    wrongToken: relayWrongToken.status,
    rightToken: relayRightToken.status,
  });

  rec('browser-errors', { errors: out.errors });

  const byName = Object.fromEntries(out.checks.map(c => [c.name, c]));
  const verdict = {
    directRefused: byName['direct-no-credential']?.status === 401,
    refusalLegible: byName['direct-no-credential']?.legible === true,
    frontedPanel: byName['fronted-panel']?.httpStatus === 200 && byName['fronted-panel']?.hasComposer === true,
    registeredThroughFront: ['register-' + agents[0], 'register-' + agents[1]]
      .every(n => [201, 409].includes(byName[n]?.status)),
    sendThroughFront: byName['send-through-front']?.accepted === true && byName['send-through-front']?.transcriptHasMessage === true,
    tokenHidden: byName['token-not-in-page']?.anyLeak === false,
    relayClosed: byName['relay-auth-boundary']?.noToken === 401 && byName['relay-auth-boundary']?.wrongToken === 401,
    relayOpensWithToken: byName['relay-auth-boundary']?.rightToken === 200,
    noBrowserErrors: out.errors.length === 0,
  };
  out.verdict = verdict;
  const failed = Object.entries(verdict).filter(([, v]) => !v).map(([k]) => k);
  console.log('VERDICT', JSON.stringify(verdict));
  if (failed.length) {
    console.error('FAIL: ' + failed.join(','));
    process.exitCode = 1;
  } else {
    console.log('PASS: fronted /chat is usable — direct 401, fronted panel, real send, token absent from page source/storage');
  }
  rec('browser-errors', { errors: out.errors });
  fs.writeFileSync(process.env.CRIER_E2E_OUT || 'e2e/front-e2e-results.json', JSON.stringify(out, null, 2));
  await browser.close();
})().catch(async (e) => {
  out.errors.push(String(e.stack || e));
  try { fs.writeFileSync(process.env.CRIER_E2E_OUT || 'e2e/front-e2e-results.json', JSON.stringify(out, null, 2)); } catch (_) {}
  console.error(e.stack || e);
  try { if (browserRef) await browserRef.close(); } catch (_) {}
  process.exit(1);
});
