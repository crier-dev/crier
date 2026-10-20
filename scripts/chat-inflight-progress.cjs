#!/usr/bin/env node
// DF-CRIER-414: the composer's in-flight line, driven live by the shipped page.
//
// Measured defect: a send to a dead webhook sat at "Recording message…" for the
// whole 30s delivery budget; the first thing the operator learned was the
// terminal outcome. This script reproduces that shape with a fixture server and
// reads what the page actually renders while the delivery is in flight.
//
// Two fixture targets, because the two interesting terminal outcomes need
// different endpoint behaviour:
//   - dead-bot      — a port nothing listens on: the runtime's dial is REFUSED
//                     within milliseconds, which is the early signal this row
//                     adds (terminal outcome `refused`: a transport refusal, not
//                     a cut — budget exhaustion alone is not a cut, §3.2).
//   - blackhole-bot — a listener that accepts and never answers: no attempt
//                     result exists until the budget expires, so the terminal
//                     outcome is `budget_cut` — the measured case.
//
// Run: CRIER_TEST_BINARY=/tmp/df-crier-414-server \
//      NODE_PATH=/tmp/crier-browser/node_modules \
//      node scripts/chat-inflight-progress.cjs
// Screenshots land in docs/screenshots/df-crier-414/ (1280 and 390 widths).
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const http = require('node:http');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');
const {once} = require('node:events');

const evidence = process.env.CHAT_EVIDENCE || path.resolve('docs/screenshots/df-crier-414');
const BUDGET_SECONDS = 30; // the default, deliberate per-target delivery budget
let child, browser, hangServer;

async function freePort() {
  const listener = net.createServer();
  await new Promise((resolve, reject) => listener.once('error', reject).listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  return port;
}

// A blackhole endpoint: it accepts the delivery and never answers, so the
// runtime's attempt has no result until its own budget expires.
async function startHangServer() {
  const server = http.createServer(() => {}); // deliberately never responds
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {server, port: server.address().port};
}

async function run() {
  let chromium;
  try { ({chromium} = require('playwright')); }
  catch (error) { if (error.code !== 'MODULE_NOT_FOUND') throw error; console.error('SKIP: Playwright missing'); process.exitCode = 77; return; }
  const binary = process.env.CRIER_TEST_BINARY;
  assert.ok(binary && fs.existsSync(binary), 'CRIER_TEST_BINARY must select the rebuilt server');
  fs.mkdirSync(evidence, {recursive: true});

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'df-crier-414-'));
  const port = await freePort();
  const base = 'http://127.0.0.1:' + port;
  const deadPort = await freePort();
  const hang = await startHangServer();
  hangServer = hang.server;

  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('CR_') && !key.startsWith('CRIER_')));
  Object.assign(env, {
    CRIER_PORT: String(port), CR_REQUIRE_AGENT_SIG: 'false', CR_REGISTRY_BACKEND: 'memory',
    CR_SESSION_BACKEND: 'jsonl', CR_SESSION_LOG_ROOT: path.join(dir, 'sessions'), CR_GROUP_ROOT: path.join(dir, 'groups'),
    CR_RATE_LIMIT_PER_MINUTE: '0', CR_GUARD_ENABLED: 'false',
  });
  const fd = fs.openSync(path.join(dir, 'server.log'), 'w');
  child = spawn(binary, [], {cwd: dir, env, stdio: ['ignore', fd, fd]});
  fs.closeSync(fd);
  let ready = false;
  for (let i = 0; i < 100; i++) {
    assert.equal(child.exitCode, null, 'scratch server exited: ' + fs.readFileSync(path.join(dir, 'server.log'), 'utf8'));
    try { if ((await fetch(base + '/health')).ok) { ready = true; break; } } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  assert.ok(ready, 'scratch server not ready: ' + dir);

  async function api(route, method = 'GET', body) {
    const response = await fetch(base + route, {method, headers: body ? {'content-type': 'application/json'} : {}, body: body ? JSON.stringify(body) : undefined});
    const text = await response.text();
    assert.ok(response.ok, method + ' ' + route + ' ' + response.status + ' ' + text);
    return text ? JSON.parse(text) : null;
  }

  const operator = 'df414-operator';
  for (const id of [operator, 'dead-bot', 'blackhole-bot']) {
    const {publicKey} = crypto.generateKeyPairSync('ed25519');
    const registration = {id, public_key: publicKey.export({type: 'spki', format: 'der'}).subarray(-32).toString('hex')};
    if (id === 'dead-bot') registration.webhook = {url: `http://127.0.0.1:${deadPort}/hook`, delivery_mode: 'blocking'};
    if (id === 'blackhole-bot') registration.webhook = {url: `http://127.0.0.1:${hang.port}/hook`, delivery_mode: 'blocking'};
    await api('/agents', 'POST', registration);
  }
  const room = await api('/sessions', 'POST', {
    title: 'DF-CRIER-414 in-flight', kind: 'channel', created_by: {agent: operator},
    members: [operator, 'dead-bot', 'blackhole-bot'].map(id => ({member_type: 'agent', member_id: id})),
  });

  browser = await chromium.launch({executablePath: process.env.CHROME_BIN || '/usr/bin/google-chrome', headless: true, args: ['--no-sandbox']});
  const page = await browser.newPage({viewport: {width: 1280, height: 1000}});
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(base + '/chat');
  await page.waitForFunction(() => !document.getElementById('management-refresh').disabled);
  await page.locator('#agent').fill(operator);
  await page.locator('#apply-identity').click();
  await page.waitForFunction(() => !state.sending && !state.busy && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));
  await page.locator(`[data-session="${room.id}"]`).click();
  await page.waitForFunction(id => state.current === id && !state.busy, room.id);

  const panel = () => page.locator('#send-progress').innerText();
  const visible = () => page.evaluate(() => {
    const el = document.getElementById('send-progress');
    return {hidden: el.hidden, className: el.className, spinnerHidden: document.getElementById('send-progress-spinner').hidden, text: document.getElementById('send-progress-text').textContent};
  });
  const shot = async (name, selector) => {
    const target = path.join(evidence, name + '.png');
    if (selector) await page.locator(selector).screenshot({path: target});
    else await page.screenshot({path: target, fullPage: true});
    return path.basename(target);
  };

  const samples = [];
  const started = Date.now();
  await page.locator('#text').fill('are you there?');
  await page.locator('#send-button').click();

  // ~2s: the in-flight line, elapsed vs the budget, per target.
  await page.waitForTimeout(2000);
  const early = await visible();
  const earlyReading = await api(`/sessions/${room.id}/messages`);
  const earlyLive = (earlyReading.messages[0] || {}).dispatch_live || null;
  samples.push({at: '2s', panel: early.text, server: earlyLive && {elapsed_ms: earlyLive.elapsed_ms, budget_ms: earlyLive.budget_ms, targets: earlyLive.targets}});
  console.log('IN-FLIGHT 2s ' + JSON.stringify({panel: early.text, elapsed_ms: earlyLive && earlyLive.elapsed_ms, budget_ms: earlyLive && earlyLive.budget_ms}));
  assert.equal(early.hidden, false, 'the in-flight panel must be visible while delivery is in flight');
  assert.equal(early.spinnerHidden, false, 'the in-flight panel must show it is working');
  assert.match(early.text, /^Delivery in flight — \d+(\.\d+)?s of the \d+(\.\d+)?s delivery budget/, 'the elapsed and the budget come first');
  assert.match(early.text, new RegExp(`dead-bot: unreachable — .*127\\.0\\.0\\.1:${deadPort}.*connection refused`), 'the early line names the target and the transport reason');
  assert.match(early.text, /blackhole-bot: no attempt has returned yet/, 'an unanswered target is reported as such, never as a failure');
  assert.notEqual(earlyLive, null, 'the server must publish the live pass the panel renders');
  assert.equal(earlyLive.budget_ms, BUDGET_SECONDS * 1000, 'the budget is the deliberate one');

  const inflightShots = [await shot('inflight-1280', '.composer-wrap'), await shot('inflight-1280-full')];
  await page.setViewportSize({width: 390, height: 900});
  const overflowNarrow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  inflightShots.push(await shot('inflight-390', '.composer-wrap'));
  assert.equal(overflowNarrow, 0, 'the in-flight panel must not overflow a 390px viewport');
  await page.setViewportSize({width: 1280, height: 1000});

  // ~5s: the same line, later in the same budget — the elapsed is the server's.
  await page.waitForTimeout(3000);
  const later = await visible();
  const laterReading = await api(`/sessions/${room.id}/messages`);
  const laterLive = (laterReading.messages[0] || {}).dispatch_live || null;
  const panelElapsed = Number((later.text.match(/^Delivery in flight — (\d+(?:\.\d+)?)s/) || [])[1]);
  samples.push({at: '5s', panel: later.text, server: laterLive && {elapsed_ms: laterLive.elapsed_ms, budget_ms: laterLive.budget_ms}});
  console.log('IN-FLIGHT 5s ' + JSON.stringify({panel: later.text, elapsed_ms: laterLive && laterLive.elapsed_ms}));
  assert.match(later.text, new RegExp(`dead-bot: unreachable — .*127\\.0\\.0\\.1:${deadPort}`), 'the reason is still named at 5s, inside the budget');
  assert.ok(laterLive, 'the pass is still live at 5s');
  assert.ok(Math.abs(panelElapsed * 1000 - laterLive.elapsed_ms) < 1500, `the rendered elapsed (${panelElapsed}s) must be the server reading (${laterLive.elapsed_ms}ms), not a client clock`);
  assert.ok(laterLive.elapsed_ms > earlyLive.elapsed_ms, 'the server reading advances');

  // ~31s: the terminal outcome replaces the line, and nothing spins.
  await page.waitForFunction(() => /Delivery finished|Delivery still in flight|not readable yet/.test(document.getElementById('send-progress-text').textContent), null, {timeout: 45000});
  const terminal = await visible();
  const finalReading = await api(`/sessions/${room.id}/messages`);
  const outcomes = (finalReading.messages[0] || {}).outcomes || [];
  samples.push({at: 'terminal', wall_ms: Date.now() - started, panel: terminal.text, outcomes});
  console.log('TERMINAL ' + JSON.stringify({wall_ms: Date.now() - started, panel: terminal.text, outcomes: outcomes.map(o => ({target: o.target, outcome: o.outcome}))}));
  assert.equal(terminal.spinnerHidden, true, 'the indicator must not be left spinning after the budget');
  assert.match(terminal.text, /^Delivery finished — /, 'the terminal state is the recorded outcome, not a progress line');
  assert.match(terminal.text, /blackhole-bot: budget_cut — /, 'the blackholed target ends in the honest budget_cut');
  assert.match(terminal.text, /dead-bot: (delivery refused|refused) — /, 'the refused dial keeps the refusal vocabulary');
  assert.equal(terminal.hidden, false, 'the terminal outcome is shown where the operator is looking');
  assert.ok(outcomes.some(o => o.outcome === 'budget_cut' && o.target === 'blackhole-bot'), 'budget_cut is the recorded outcome, not a panel invention');
  assert.ok(outcomes.some(o => o.outcome === 'refused' && o.target === 'dead-bot'), 'the refused dial is recorded as refused');
  assert.equal(Date.now() - started >= BUDGET_SECONDS * 1000, true, 'the budget was not shortened to make the panel lively');
  const terminalShots = [await shot('terminal-1280', '.composer-wrap'), await shot('terminal-1280-full')];
  await page.setViewportSize({width: 390, height: 900});
  const terminalOverflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  terminalShots.push(await shot('terminal-390', '.composer-wrap'));
  assert.equal(terminalOverflow, 0, 'the terminal line must not overflow a 390px viewport');
  await page.setViewportSize({width: 1280, height: 1000});

  // The recorded outcome is on the message row too (the durable record).
  await page.waitForFunction(() => /budget_cut/.test(document.getElementById('transcript').innerText), null, {timeout: 15000});
  const transcriptText = await page.locator('#transcript').innerText();
  assert.deepEqual(errors, [], 'browser errors');

  fs.writeFileSync(path.join(evidence, 'rendered-inflight.json'),
    JSON.stringify({room: room.id, budget_seconds: BUDGET_SECONDS, dead_port: deadPort, hang_port: hang.port, samples, transcript: transcriptText, screenshots: [...inflightShots, ...terminalShots], errors}, null, 2) + '\n');
  console.log('SCREENSHOTS ' + JSON.stringify([...inflightShots, ...terminalShots]));
  console.log('PASS: in-flight elapsed/budget, early unreachable naming the target, and the terminal outcome replacing the line');
}

run().catch(error => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (hangServer) await new Promise(resolve => hangServer.close(resolve)).catch(() => {});
  if (browser) await browser.close();
  if (child && child.exitCode === null && child.signalCode === null) { child.kill('SIGTERM'); await once(child, 'exit'); }
});
