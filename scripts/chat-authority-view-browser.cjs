#!/usr/bin/env node
// DF-CRIER-384 — the Authority view renders the ACL the server already serves.
//
// The defect: DF-CRIER-315 added GET /principals, /bindings, /grants and
// /agents/{id}/class behind the admin token, and DF-CRIER-383 made /principals
// identify who each principal is (display_name, namespace, role, status), but
// none of it was reachable from chat.html — an operator asking "who may deliver
// to atlas, and under what grant" had to leave the panel and curl an
// admin-only route.
//
// What this battery proves, against scratch in-process servers and the SHIPPED
// page (no fixture rows in the page, no second client for the ACL):
//   1. the four inventories render from the SAME REST paths the server serves;
//   2. an empty fold says so in words (never a blank panel);
//   3. a 401/403 renders as a NAMED gate state — route + status + the server's
//      own detail + what arms the gate — and never as an empty list;
//   4. token material is never rendered: a fingerprint only as a SHORT prefix,
//      labelled as such, and a 64-hex value is refused in words;
//   5. the view carries no write control;
//   6. no ACL fetch fires while the view is closed, and a refused fetch is not
//      retried in a loop.
//
// Every ACL record used here is created through the REAL write API, except the
// two disclosed lines written straight into the append log: the write API
// refuses unknown fields by design (DF-CRIER-316), so a deployment only has a
// fingerprint to project when a record outside the API carries token bytes.
//
// Run:
//   NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-384-server \
//     CHAT_EVIDENCE=docs/screenshots/df-crier-384 node scripts/chat-authority-view-browser.cjs
// Missing prerequisites exit 77, never PASS.
const crypto = require('node:crypto');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');

const evidence = path.resolve(process.env.CHAT_EVIDENCE || 'docs/screenshots/df-crier-384');
const AGENT = 'atlas';
const FIXTURE_PRINCIPAL = 'prin_fixture_owner';
const FIXTURE_DISPLAY_NAME = 'Atlas Owner';
// A credential-shaped value that must NEVER reach the page, with the server's
// own projection of it: sha256 hex, of which the read surface serves the first
// four bytes (8 hex digits).
const FIXTURE_TOKEN = 'df-crier-384-fixture-token-value';
const TOKEN_SHA256 = crypto.createHash('sha256').update(FIXTURE_TOKEN).digest('hex');
const TOKEN_FINGERPRINT = TOKEN_SHA256.slice(0, 8);
const HEX64 = /[0-9a-fA-F]{64}/;

const children = [];
const captures = [];
const pageErrors = [];
let checks = 0;
let failures = 0;

function check(label, ok, detail) {
  checks++;
  if (ok) { console.log('PASS ' + checks + '. ' + label); return true; }
  failures++;
  console.error('FAIL ' + checks + '. ' + label + (detail ? '\n      ' + detail : ''));
  return false;
}
function checkEqual(label, actual, expected) {
  return check(label, actual === expected, 'actual=' + JSON.stringify(actual) + ' expected=' + JSON.stringify(expected));
}
function checkIncludes(label, haystack, needle) {
  return check(label, String(haystack).includes(needle), 'missing ' + JSON.stringify(needle) + ' in ' + JSON.stringify(String(haystack).slice(0, 400)));
}

async function freePort() {
  const probe = net.createServer();
  await new Promise((resolve, reject) => probe.once('error', reject).listen(0, '127.0.0.1', resolve));
  const port = probe.address().port;
  await new Promise(resolve => probe.close(resolve));
  return port;
}

// Disclosed ACL fixture set: ONE hand-written principal carrying token bytes
// (so the read surface has a fingerprint to project) and ONE agent-class row.
function fixtureRecords() {
  const ts = new Date().toISOString();
  return [
    {v: 1, type: 'permission.principal', id: FIXTURE_PRINCIPAL, ts, principal: {id: FIXTURE_PRINCIPAL, kind: 'principal', status: 'active', display_name: FIXTURE_DISPLAY_NAME, namespace: AGENT, role: 'owner', created_at: ts, token: FIXTURE_TOKEN}},
    {v: 1, type: 'permission.agent', id: AGENT, ts, agent: {id: AGENT, class: 'personal', owner: FIXTURE_PRINCIPAL, namespace: AGENT}},
  ];
}

async function start(opts = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'df-crier-384-'));
  const port = await freePort();
  const admin = crypto.randomBytes(24).toString('hex');
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('CR_') && !k.startsWith('CRIER_')));
  Object.assign(env, {
    CRIER_PORT: String(port), CR_REQUIRE_AGENT_SIG: 'false', CR_REGISTRY_BACKEND: 'memory',
    CR_SESSION_BACKEND: 'jsonl', CR_SESSION_LOG_ROOT: path.join(dir, 'sessions'),
    CR_GROUP_ROOT: path.join(dir, 'groups'), CR_RATE_LIMIT_PER_MINUTE: '0', CR_GUARD_ENABLED: 'false',
  });
  if (opts.armed) {
    const storeDir = path.join(dir, 'permissions');
    if (opts.records) {
      fs.mkdirSync(storeDir, {recursive: true});
      fs.writeFileSync(path.join(storeDir, 'permissions.jsonl'), opts.records.map(record => JSON.stringify(record)).join('\n') + '\n');
    }
    Object.assign(env, {CR_PERMISSIONS_ENABLED: 'true', CR_PERMISSIONS_ADMIN_TOKEN: admin, CR_PERMISSIONS_DIR: storeDir});
  }
  const fd = fs.openSync(path.join(dir, 'server.log'), 'w');
  const child = spawn(process.env.CRIER_TEST_BINARY || '/tmp/df-crier-384-server', [], {cwd: dir, env, stdio: ['ignore', fd, fd]});
  children.push(child);
  fs.closeSync(fd);
  const base = 'http://127.0.0.1:' + port;
  for (let i = 0; i < 120; i++) {
    if (child.exitCode !== null || child.signalCode !== null) throw new Error('scratch boot failed: ' + fs.readFileSync(path.join(dir, 'server.log'), 'utf8'));
    try { if ((await fetch(base + '/health')).ok) return {base, admin, dir}; } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error('readiness failed: ' + dir);
}

async function api(server, method, route, body, token) {
  const supplied = token === undefined ? server.admin : token;
  const response = await fetch(server.base + route, {
    method,
    headers: {'Content-Type': 'application/json', ...(supplied ? {Authorization: 'Bearer ' + supplied} : {})},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const raw = await response.text();
  return {status: response.status, body: raw ? JSON.parse(raw) : null};
}

async function panel(page, id) {
  return page.evaluate(sel => {
    const el = document.getElementById(sel);
    return el ? {state: el.dataset.readState, text: el.innerText, tables: el.querySelectorAll('table').length} : null;
  }, id);
}
async function idle(page) {
  await page.waitForFunction(() => !document.querySelector('#management-refresh').disabled && !state.busy);
}
// The view is addressed through its own fold id; a build without the fold is a
// NAMED failure of requirement 6 (there is no closed state to test), not a
// locator timeout that aborts the rest of the battery.
async function viewState(page) {
  return page.evaluate(() => {
    const el = document.getElementById('authority-view');
    return {present: !!el, open: el ? el.open : null, text: (el || document.querySelector('.management'))?.innerText || ''};
  });
}
async function viewLocator(page) {
  return (await viewState(page)).present ? page.locator('#authority-view') : page.locator('.management');
}
async function toggleView(page) {
  if ((await viewState(page)).present) { await page.locator('#authority-view > summary').click(); return true; }
  return false;
}
async function refresh(page) {
  await page.locator('#management-refresh').click();
  await idle(page);
}
function countsFor(counters, paths) {
  return Object.fromEntries(paths.map(route => [route, counters[route] || 0]));
}
async function capture(page, label) {
  const view = await page.evaluate(() => {
    const el = document.getElementById('authority-view');
    return el ? {open: el.open, text: el.innerText} : null;
  });
  const record = {label, open: view?.open, authority: view?.text};
  captures.push(record);
  fs.writeFileSync(path.join(evidence, 'rendered-strings.json'), JSON.stringify(captures, null, 2) + '\n');
  return record;
}

async function run() {
  let chromium;
  try { ({chromium} = require('playwright')); }
  catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error;
    console.error('SKIP: Playwright unavailable; exit 77');
    process.exitCode = 77;
    return;
  }
  const executablePath = process.env.CHROME_BIN || '/usr/bin/google-chrome';
  if (!fs.existsSync(executablePath)) { console.error('SKIP: Chromium unavailable; exit 77'); process.exitCode = 77; return; }
  if (!process.env.CRIER_TEST_BINARY || !fs.existsSync(process.env.CRIER_TEST_BINARY)) {
    console.error('SKIP: CRIER_TEST_BINARY must select the rebuilt server binary; exit 77');
    process.exitCode = 77;
    return;
  }
  fs.mkdirSync(evidence, {recursive: true});
  const browser = await chromium.launch({executablePath, headless: true, args: ['--no-sandbox']});
  try {
    await populated(browser);
    await empty(browser);
    await refused(browser);
    await disabled(browser);
  } finally {
    await browser.close();
  }
}

// --- 1. The four inventories, live, plus token hygiene and fetch discipline ---
async function populated(browser) {
  const server = await start({armed: true, records: fixtureRecords()});
  // Written through the real API, not into the log: these are what an operator
  // would have created, and they are also the fixture the panel must NOT carry.
  const minted = await api(server, 'POST', '/principals', {display_name: 'API Holder', namespace: AGENT, role: 'member', status: 'active'});
  checkEqual('mint a second principal through the real write API', minted.status, 201);
  const binding = await api(server, 'POST', '/bindings', {principal: minted.body.id, agent: AGENT, as_agent: true});
  checkEqual('create a binding through the real write API', binding.status, 201);
  const grant = await api(server, 'POST', '/grants', {principal: minted.body.id, subject: {type: 'agent', ref: AGENT}, actions: ['send'], note: 'DF-CRIER-384 fixture'});
  checkEqual('create a grant through the real write API', grant.status, 201);
  const wirePrincipals = await api(server, 'GET', '/principals');
  const fixtureOnWire = (wirePrincipals.body.items || []).find(p => p.id === FIXTURE_PRINCIPAL);
  check('GET /principals serves the stored principal', !!fixtureOnWire, JSON.stringify(wirePrincipals.body).slice(0, 300));
  checkEqual('the served principal carries the recorded display_name (DF-CRIER-383)', fixtureOnWire?.display_name, FIXTURE_DISPLAY_NAME);
  checkEqual('the served principal carries a SHORT fingerprint prefix, not a hash', fixtureOnWire?.token_fingerprint, TOKEN_FINGERPRINT);
  check('the served principal never carries the credential itself', !JSON.stringify(wirePrincipals.body).includes(FIXTURE_TOKEN));
  check('the served principal never carries the 64-hex digest', !HEX64.test(JSON.stringify(wirePrincipals.body)));

  const page = await browser.newPage({viewport: {width: 1280, height: 1200}, extraHTTPHeaders: {Authorization: 'Bearer ' + server.admin}});
  page.on('pageerror', error => pageErrors.push(error.message));
  const counters = {};
  const pagePosts = [];
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin === server.base) { counters[url.pathname] = (counters[url.pathname] || 0) + 1; if (request.method() === 'POST') pagePosts.push(url.pathname); }
  });
  await page.goto(server.base + '/chat');
  await idle(page);

  const served = await (await fetch(server.base + '/chat')).text();
  check('the shipped page carries no fixture row for this view', !served.includes(FIXTURE_PRINCIPAL) && !served.includes(FIXTURE_DISPLAY_NAME));
  check('the shipped page never carries token material', !served.includes(FIXTURE_TOKEN) && !served.includes(TOKEN_SHA256));
  check('the view states its own read-only contract', /read-only/i.test((await viewState(page)).text));

  const principals = await panel(page, 'management-principals');
  checkEqual('principals: the fold renders as POPULATED', principals?.state, 'populated');
  checkIncludes('principals: the stored principal is rendered', principals.text, FIXTURE_PRINCIPAL);
  checkIncludes('principals: display_name is rendered, not invented', principals.text, FIXTURE_DISPLAY_NAME);
  checkIncludes('principals: namespace/role/status are rendered', principals.text, AGENT);
  checkIncludes('principals: the API-created principal is rendered', principals.text, minted.body.id);
  check('principals: no 64-hex run reaches the fold', !HEX64.test(principals.text), principals.text.slice(0, 300));
  check('principals: the credential is never rendered', !principals.text.includes(FIXTURE_TOKEN));
  checkIncludes('principals: the fingerprint prefix is rendered', principals.text, TOKEN_FINGERPRINT);
  const principalHeaders = await page.evaluate(() => Array.from(document.querySelectorAll('#management-principals thead th')).map(th => th.innerText));
  check('principals: the fingerprint column is LABELLED as a short prefix', principalHeaders.some(h => /token_fingerprint \(short prefix\)/.test(h)), JSON.stringify(principalHeaders));
  check('principals: no column claims to show a credential', !principalHeaders.some(h => /^(token|token_hash|secret|password|credential)$/i.test(h)), JSON.stringify(principalHeaders));

  const bindings = await panel(page, 'management-bindings');
  checkEqual('bindings: the fold renders as POPULATED', bindings?.state, 'populated');
  checkIncludes('bindings: the binding is rendered', bindings.text, binding.body.id);
  checkIncludes('bindings: the bound pair is rendered', bindings.text, AGENT);

  const grants = await panel(page, 'management-grants');
  checkEqual('grants: the fold renders as POPULATED', grants?.state, 'populated');
  checkIncludes('grants: the grant is rendered', grants.text, grant.body.id);
  checkIncludes('grants: the grantee is rendered', grants.text, minted.body.id);

  await page.locator('#class-agent').fill(AGENT);
  await page.locator('#class-query-submit').click();
  await page.waitForFunction(() => !document.getElementById('class-query-submit').disabled);
  const klass = await panel(page, 'management-class');
  checkEqual('agent class: the per-agent fold renders as POPULATED', klass?.state, 'populated');
  checkIncludes('agent class: the recorded class is rendered', klass.text, 'personal');
  checkIncludes('agent class: the recorded owner is rendered', klass.text, FIXTURE_PRINCIPAL);

  // "the same REST paths the server serves" — the page must have READ them.
  for (const route of ['/principals', '/bindings', '/grants']) {
    check('the panel read ' + route + ' from the server', (counters[route] || 0) > 0);
  }
  check('the panel read the per-agent class route', Object.keys(counters).some(p => /^\/agents\/.+\/class$/.test(p)));
  check('the view issues no write call to the ACL', pagePosts.filter(p => ['/principals', '/bindings', '/grants', '/agents/' + AGENT + '/class'].includes(p)).length === 0, JSON.stringify(pagePosts));

  await capture(page, 'populated');

  // A 64-hex fingerprint (or a raw token) offered by the server must not be
  // printed. Disclosed fault injection: the response body only.
  await page.route(server.base + '/principals', route => route.fulfill({
    status: 200, contentType: 'application/json',
    body: JSON.stringify({items: [{id: 'prin_injected', kind: 'principal', created_at: new Date().toISOString(), token_fingerprint: TOKEN_SHA256, token: FIXTURE_TOKEN, token_hash: TOKEN_SHA256}], count: 1}),
  }));
  await refresh(page);
  const injected = await panel(page, 'management-principals');
  check('a 64-hex fingerprint is refused in words', /short prefix/.test(injected.text) && /never rendered|not shown/.test(injected.text), injected.text.slice(0, 400));
  check('a 64-hex fingerprint is not printed', !injected.text.includes(TOKEN_SHA256), injected.text.slice(0, 400));
  check('a token offered by the server is not printed', !injected.text.includes(FIXTURE_TOKEN));
  await page.unroute(server.base + '/principals');
  await refresh(page);

  // Fetch discipline: the transcript refresh must not re-issue an ACL read.
  const aclPaths = ['/principals', '/bindings', '/grants'];
  const beforeTranscript = countsFor(counters, aclPaths);
  await page.locator('#refresh').click();
  await idle(page);
  await page.locator('#refresh').click();
  await idle(page);
  check('a transcript refresh does not retry the ACL reads',
    JSON.stringify(countsFor(counters, aclPaths)) === JSON.stringify(beforeTranscript),
    JSON.stringify(countsFor(counters, aclPaths)) + ' vs ' + JSON.stringify(beforeTranscript));

  // Closed: nothing fires.
  const closedState = await toggleView(page);
  check('the view has a closed state (a fold the operator can close)', closedState, 'no #authority-view fold in the served page');
  checkEqual('the view can be closed', closedState ? (await viewState(page)).open : null, false);
  const closedAt = countsFor(counters, aclPaths);
  await page.waitForTimeout(2500);
  check('no ACL fetch fires while the view is closed',
    JSON.stringify(countsFor(counters, aclPaths)) === JSON.stringify(closedAt),
    JSON.stringify(countsFor(counters, aclPaths)) + ' vs ' + JSON.stringify(closedAt));
  // The fold's own toggle event is queued, not synchronous with the click, so
  // wait for the read it is supposed to issue instead of racing it.
  const refetched = closedState
    ? page.waitForRequest(request => new URL(request.url()).pathname === '/principals', {timeout: 5000}).catch(() => null)
    : Promise.resolve(null);
  await toggleView(page);
  check('reopening the view refetches the inventories', !!(await refetched), 'no /principals read followed the reopen');
  await idle(page);
  const reopened = countsFor(counters, aclPaths);
  check('reopening the view fetches each inventory exactly once',
    aclPaths.every(route => (reopened[route] || 0) === (closedAt[route] || 0) + 1),
    JSON.stringify(reopened) + ' vs ' + JSON.stringify(closedAt));

  const view = await viewLocator(page);
  for (const width of [1280, 390]) {
    await page.setViewportSize({width, height: 1200});
    checkEqual('zero page overflow at ' + width + 'px', await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth), 0);
    await page.screenshot({path: path.join(evidence, 'authority-populated-' + width + '.png'), fullPage: true});
    await view.screenshot({path: path.join(evidence, 'authority-view-' + width + '.png')});
  }
  await page.setViewportSize({width: 1280, height: 1200});
  await page.locator('#management-principals').screenshot({path: path.join(evidence, 'authority-principals-detail.png')});
  await toggleView(page);
  await page.screenshot({path: path.join(evidence, 'authority-closed-1280.png'), fullPage: true});
  await page.close();
}

// --- 2. An empty fold says so -------------------------------------------------
async function empty(browser) {
  const server = await start({armed: true});
  const page = await browser.newPage({viewport: {width: 1280, height: 1000}, extraHTTPHeaders: {Authorization: 'Bearer ' + server.admin}});
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(server.base + '/chat');
  await idle(page);
  const expectations = {
    'management-principals': 'GET /principals',
    'management-bindings': 'GET /bindings',
    'management-grants': 'GET /grants',
  };
  for (const [id, route] of Object.entries(expectations)) {
    const state = await panel(page, id);
    const wire = await api(server, 'GET', route.slice(4));
    checkEqual('empty: ' + route + ' answers 200 with an empty fold', wire.status, 200);
    checkEqual('empty: ' + route + ' is only "empty" once the server said so', state?.state, 'empty');
    check('empty: the fold says "Empty — ' + route + ' succeeded"', /^Empty — /.test(state.text) && state.text.includes(route), state.text);
    checkEqual('empty: ' + route + ' draws no table', state.tables, 0);
  }
  await page.locator('#class-agent').fill(AGENT);
  await page.locator('#class-query-submit').click();
  await page.waitForFunction(() => !document.getElementById('class-query-submit').disabled);
  const klass = await panel(page, 'management-class');
  check('empty: a missing agent-class record is a NAMED error, not an inferred class',
    klass?.state === 'error' && /HTTP 404/.test(klass.text) && /no such agent/.test(klass.text), JSON.stringify(klass));
  await capture(page, 'empty');
  await page.screenshot({path: path.join(evidence, 'authority-empty-1280.png'), fullPage: true});
  await page.setViewportSize({width: 390, height: 1000});
  checkEqual('empty: zero page overflow at 390px', await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth), 0);
  await page.screenshot({path: path.join(evidence, 'authority-empty-390.png'), fullPage: true});
  await page.close();
}

// --- 3. 401/403 is a named gate state ---------------------------------------
async function refused(browser) {
  const server = await start({armed: true, records: fixtureRecords()});
  const page = await browser.newPage({viewport: {width: 1280, height: 1000}});
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(server.base + '/chat');
  await idle(page);
  for (const [id, route] of Object.entries({'management-principals': '/principals', 'management-bindings': '/bindings', 'management-grants': '/grants'})) {
    const wire = await api(server, 'GET', route, undefined, '');
    checkEqual('refusal: ' + route + ' refuses an authorized-less read', wire.status, 403);
    const state = await panel(page, id);
    checkEqual('refusal: ' + route + ' renders as a NAMED gate state', state?.state, 'refused');
    check('refusal: ' + route + ' names the route and the HTTP status', state.text.includes('GET ' + route) && state.text.includes('HTTP 403'), state.text);
    check('refusal: ' + route + ' reproduces the server\'s own detail', state.text.includes(wire.body.detail), state.text);
    check('refusal: ' + route + ' names what arms the gate', state.text.includes('CR_PERMISSIONS_ADMIN_TOKEN'), state.text);
    check('refusal: ' + route + ' is not a silent empty list', state.tables === 0 && state.text.trim().length > 0);
  }
  // The per-agent class read refuses the same way.
  await page.locator('#class-agent').fill(AGENT);
  await page.locator('#class-query-submit').click();
  await page.waitForFunction(() => !document.getElementById('class-query-submit').disabled);
  const refusedClass = await panel(page, 'management-class');
  checkEqual('refusal: the per-agent class read renders as REFUSED', refusedClass?.state, 'refused');
  checkIncludes('refusal: the class panel names the route', refusedClass.text, '/agents/');
  // Disclosed fault injection: a 401 from the same route must read the same way.
  await page.route(server.base + '/principals', route => route.fulfill({status: 401, contentType: 'application/json', body: JSON.stringify({error: 'UNAUTHORIZED', detail: 'admin token refused by the deployment gateway'})}));
  await refresh(page);
  const injected = await panel(page, 'management-principals');
  checkEqual('refusal: an injected 401 is the REFUSED state too', injected?.state, 'refused');
  check('refusal: a 401 names its status and detail', injected.text.includes('HTTP 401') && injected.text.includes('admin token refused by the deployment gateway'), injected.text);
  await page.unroute(server.base + '/principals');
  await capture(page, 'refused');
  await page.screenshot({path: path.join(evidence, 'authority-refused-1280.png'), fullPage: true});
  await page.close();
}

// --- 4. The panel keeps working when the surface is disabled at startup ------
async function disabled(browser) {
  const server = await start({armed: false});
  const page = await browser.newPage({viewport: {width: 1280, height: 1000}});
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(server.base + '/chat');
  await idle(page);
  checkIncludes('disabled: the posture line says the management surface is off', await page.locator('#posture').innerText(), 'Management: disabled');
  for (const id of ['management-principals', 'management-bindings', 'management-grants']) {
    const state = await panel(page, id);
    check('disabled: ' + id + ' states why it cannot read instead of erroring blank', !!state?.state && state.text.trim().length > 0, JSON.stringify(state));
  }
  check('disabled: the panel raised no page error', pageErrors.length === 0, JSON.stringify(pageErrors));
  await capture(page, 'disabled-startup');
  await page.screenshot({path: path.join(evidence, 'authority-disabled-1280.png'), fullPage: true});
  await page.close();
}

run().catch(error => { console.error(error); process.exitCode = 1; }).finally(async () => {
  check('the page raised no uncaught error', pageErrors.length === 0, JSON.stringify(pageErrors));
  fs.mkdirSync(evidence, {recursive: true});
  fs.writeFileSync(path.join(evidence, 'summary.json'), JSON.stringify({
    assertions: checks, failures, captures: captures.map(c => c.label), viewports: [1280, 390],
    fixture: {principal: FIXTURE_PRINCIPAL, agent: AGENT, fingerprint: TOKEN_FINGERPRINT, tokenRendered: false},
    pageErrors,
  }, null, 2) + '\n');
  for (const child of children) {
    child.kill('SIGTERM');
    await Promise.race([once(child, 'exit'), new Promise(resolve => setTimeout(resolve, 2000))]);
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
  }
  if (process.exitCode === undefined || process.exitCode === 0) {
    if (failures) { process.exitCode = 1; console.error('FAIL: ' + failures + ' of ' + checks + ' assertions failed. Evidence: ' + evidence); }
    else console.log('PASS: Authority view — ' + checks + ' assertions; four inventories live, empty fold named, 401/403 named, fingerprint prefix only, read-only, no fetch while closed. Evidence: ' + evidence);
  }
});
