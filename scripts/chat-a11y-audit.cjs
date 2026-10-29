#!/usr/bin/env node
// DF-CRIER-450: real Chromium layout, shipped /chat HTML/CSS/JS, no npm dependencies.
// node scripts/chat-a11y-audit.cjs [--html /absolute/pre-change-chat.html]
// Requires Chrome/Chromium (CHROME_BIN, default /usr/bin/google-chrome).
// Node built-ins stub fetch with explicit empty HTTP responses; no live API is contacted.
// This is a bounded regression audit, NOT a complete WCAG conformance assessment.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {pathToFileURL} = require('node:url');
const os = require('node:os');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {setTimeout: delay} = require('node:timers/promises');

const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'),
  'usage: node scripts/chat-a11y-audit.cjs [--html <page>]');
const source = path.resolve(args[1] || path.join(__dirname, '../cmd/server/chat.html'));
const html = fs.readFileSync(source, 'utf8');
// Fixed baseline, not derived from the page under test: deleting a region or
// replacing it with an unrelated announcement must fail, even if the total stays 23.
// Include initially hidden/empty result slots: they become live when results arrive.
const requiredLiveIDs = [
  'posture', 'create-result', 'identity-gate', 'remember-status', 'session-state',
  'session-lifecycle-result', 'refresh-state', 'sender-authority', 'composer-why',
  'audience-warning', 'send-progress', 'send-result', 'participant-result', 'task-result',
  'management-failures', 'management-principals', 'management-bindings', 'management-grants',
  'management-federation-posture', 'management-peers', 'management-anchors',
  'management-chain', 'management-class',
];
const fixtures = {
  '/agents': {agents: []}, '/sessions': {sessions: []},
  // /groups is a route the page reads on every refresh (loadGroups). Without a fixture
  // here it is recorded as an UNEXPECTED request, which made this audit red on a page
  // that was merely reading a real route — a stale allowlist, not an accessibility
  // defect. The empty roster is the honest fixture for "a server that has no groups".
  '/groups': {groups: []},
  '/principals': {items: []}, '/bindings': {items: []}, '/grants': {items: []},
  '/fed/peers': {peers: []}, '/trust/anchors': {anchors: []},
  '/status': {health: 'ok', agents_online: 0, agents_total: 0, auth_enabled: false,
    registry_backend: 'memory', registry_durable: false, session_backend: 'memory',
    session_store_mount: 'memory', require_agent_signature: false,
    management_surface: 'disabled', a2a_enabled: false,
    build: {version: 'audit-fixture', commit: 'not-a-live-server'}},
};

// Executed inside the actual page, never in a simulated DOM. Rendered means
// layout-present (including below the fold and the offscreen keyboard skip link),
// not display:none/hidden/collapsed-details content. Disabled controls count too.
function measure(required) {
  const all = selector => [...document.querySelectorAll(selector)];
  const rendered = el => {
    const style = getComputedStyle(el), rect = el.getBoundingClientRect();
    return el.checkVisibility({checkOpacity: true, checkVisibilityCSS: true}) &&
      style.display !== 'none' && rect.width > 0 && rect.height > 0;
  };
  const text = el => (el?.innerText || '').replace(/\s+/g, ' ').trim();
  const name = el => {
    if (el.hasAttribute('aria-labelledby')) return el.getAttribute('aria-labelledby').trim()
      .split(/\s+/).map(id => document.getElementById(id)?.textContent || '').join(' ').trim();
    if (el.hasAttribute('aria-label')) return el.getAttribute('aria-label').trim();
    if (el.labels?.length) return [...el.labels].map(label => {
      const copy = label.cloneNode(true);
      // A select's current option/value is not label text (and cannot rescue
      // an otherwise empty label). Remove embedded controls before naming it.
      copy.querySelectorAll('input,select,textarea,button,[aria-hidden=true]').forEach(node => node.remove());
      return copy.textContent.replace(/\s+/g, ' ').trim();
    }).join(' ').trim();
    if (el.matches('input[type=button],input[type=submit],input[type=reset]')) return el.value.trim();
    if (el.matches('input[type=image]')) return (el.alt || '').trim();
    // Text entry/selection content or a placeholder is NOT its accessible name.
    if (el.matches('input,select,textarea')) return (el.title || '').trim();
    return text(el) || [...el.querySelectorAll('img[alt]')].map(img => img.alt).join(' ').trim() || (el.title || '').trim();
  };
  const identify = el => el.id ? '#' + el.id : el.tagName.toLowerCase() +
    (el.getAttribute('href') ? '[href="' + el.getAttribute('href') + '"]' : '') + ' ' + JSON.stringify(name(el));
  // summary is interactive too, beyond the task's minimum control selector.
  const controls = all('button,input,select,textarea,a[href],summary').filter(rendered);
  const targets = controls.map(el => {
    const r = el.getBoundingClientRect();
    return {target: identify(el), width: Number(r.width.toFixed(2)), height: Number(r.height.toFixed(2)),
      small: r.width < 24 || r.height < 24};
  });
  const headings = all('h1,h2,h3,h4,h5,h6').filter(rendered)
    .map(el => ({level: Number(el.tagName[1]), text: text(el)}));
  const counts = new Map();
  for (const el of all('[id]').filter(rendered)) counts.set(el.id, (counts.get(el.id) || 0) + 1);
  const live = all('[aria-live], [role=status], [role=alert]').filter(el =>
    ['polite', 'assertive'].includes(el.getAttribute('aria-live')) ||
    (['status', 'alert'].includes(el.getAttribute('role')) && el.getAttribute('aria-live') !== 'off'));
  return {
    controls: controls.length, unnamed: controls.filter(el => !name(el)).map(identify),
    headings, h1Count: all('h1').length, h1Index: headings.findIndex(h => h.level === 1),
    skips: headings.flatMap((h, i) => i && h.level > headings[i - 1].level + 1 ? [headings[i - 1].level + '->' + h.level + ' ' + h.text] : []),
    duplicateIDs: [...counts].filter(([, count]) => count > 1), targets,
    liveIDs: live.map(el => el.id || '(no id)'),
    missingLiveIDs: required.filter(id => !live.some(el => el.id === id)),
  };
}

async function run() {
  let browser, profile, exit, stderr = '', sequence = 0, buffer = '', closeBrowser;
  const pending = new Map(), unexpected = [], pageErrors = [];
  const failPending = error => { for (const p of pending.values()) { clearTimeout(p.timer); p.reject(error); } pending.clear(); };
  try {
    profile = fs.mkdtempSync(path.join(os.tmpdir(), 'crier-a11y-'));
    const pagePath = path.join(profile, 'chat.html');
    fs.writeFileSync(pagePath, html);
    browser = spawn(process.env.CHROME_BIN || '/usr/bin/google-chrome', [
      '--headless', '--no-sandbox', '--disable-dev-shm-usage', '--no-first-run', '--no-proxy-server',
      '--disable-background-networking', '--remote-debugging-pipe', '--user-data-dir=' + profile,
    ], {stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe']});
    exit = once(browser, 'exit').catch(() => {});
    browser.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-4000); });
    browser.on('error', failPending);
    browser.on('exit', () => failPending(new Error('Chrome exited: ' + stderr)));
    browser.stdio[3].on('error', failPending);
    browser.stdio[4].setEncoding('utf8');
    browser.stdio[4].on('data', chunk => {
      buffer += chunk;
      let end;
      while ((end = buffer.indexOf('\0')) !== -1) {
        const message = JSON.parse(buffer.slice(0, end)); buffer = buffer.slice(end + 1);
        if (process.env.A11Y_DEBUG) console.error('CDP ' + JSON.stringify(message).slice(0, 800));
        if (message.method === 'Runtime.exceptionThrown') pageErrors.push(message.params.exceptionDetails.text + ': ' + (message.params.exceptionDetails.exception?.description || ''));
        const request = pending.get(message.id);
        if (!request) continue;
        clearTimeout(request.timer); pending.delete(message.id);
        if (message.error) request.reject(new Error(JSON.stringify(message.error)));
        else request.resolve(message.result);
      }
    });
    const cdp = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
      const id = ++sequence;
      const timer = setTimeout(() => { pending.delete(id); reject(new Error('CDP timeout: ' + method + '\n' + stderr)); }, 15000);
      pending.set(id, {resolve, reject, timer});
      browser.stdio[3].write(JSON.stringify({id, method, params, ...(sessionId ? {sessionId} : {})}) + '\0');
    });
    closeBrowser = () => cdp('Browser.close');
    const {targetId} = await cdp('Target.createTarget', {url: 'about:blank'});
    const {sessionId} = await cdp('Target.attachToTarget', {targetId, flatten: true});
    const command = (method, params) => cdp(method, params, sessionId);
    const evaluate = async expression => {
      const result = await command('Runtime.evaluate', {expression, returnByValue: true, awaitPromise: true});
      assert.ok(!result.exceptionDetails, JSON.stringify(result.exceptionDetails));
      return result.result.value;
    };
    await command('Runtime.enable');
    await command('Page.enable');
    await command('Page.addScriptToEvaluateOnNewDocument', {source: `
      window.__a11yUnexpectedRequests = [];
      window.fetch = async (route, options = {}) => {
        const fixtures = ${JSON.stringify(fixtures)};
        const body = fixtures[route];
        if (!body || (options.method && options.method !== 'GET')) {
          window.__a11yUnexpectedRequests.push((options.method || 'GET') + ' ' + route);
          return new Response('{"error":"audit fixture route absent"}', {status: 404});
        }
        return new Response(JSON.stringify(body), {status: 200, headers: {'Content-Type': 'application/json'}});
      };`});
    console.log('SOURCE ' + source);
    console.log('ENGINE ' + (await cdp('Browser.getVersion')).product + '; shipped file + empty fetch/HTTP stubs; real CSS layout');
    console.log('LIVE BASELINE ' + requiredLiveIDs.length + ' IDs: ' + requiredLiveIDs.join(','));
    let failures = 0, checks = 0;
    const check = (label, ok, detail) => {
      checks++; if (!ok) failures++;
      console.log((ok ? 'PASS ' : 'FAIL ') + label + ': ' + detail);
    };
    for (const width of [1280, 390]) {
      await command('Emulation.setDeviceMetricsOverride', {width, height: 1000, deviceScaleFactor: 1, mobile: false});
      await command('Page.navigate', {url: pathToFileURL(pagePath).href});
      let ready = false;
      for (let i = 0; i < 100; i++) {
        ready = await evaluate('document.readyState === "complete" && typeof state !== "undefined" && !state.busy && document.getElementById("refresh-state").textContent.startsWith("Last HTTP refresh") && !document.getElementById("management-refresh").disabled');
        if (ready) break;
        await delay(100);
      }
      assert.ok(ready, 'shipped page did not finish initial reads');
      await evaluate('document.fonts.ready');
      for (const expanded of [false, true]) {
        if (expanded) await evaluate('document.getElementById("new-session").click()');
        const m = await evaluate('(' + measure.toString() + ')(' + JSON.stringify(requiredLiveIDs) + ')');
        console.log('STATE ' + width + 'px / ' + (expanded ? 'new-channel expanded' : 'initial'));
        check('accessible names', m.controls > 0 && !m.unnamed.length,
          m.controls + ' rendered controls; unnamed=' + JSON.stringify(m.unnamed));
        check('one h1', m.h1Count === 1, 'h1Count=' + m.h1Count);
        check('heading order', m.h1Index === 0 && !m.skips.length,
          'h1Index=' + m.h1Index + '; first=' + m.headings.slice(0, 3).map(h => 'H' + h.level + ' ' + JSON.stringify(h.text)).join(' > ') + '; levels=' + m.headings.map(h => h.level).join(',') + '; skipped=' + JSON.stringify(m.skips));
        check('visible unique IDs', !m.duplicateIDs.length, 'duplicates=' + JSON.stringify(m.duplicateIDs));
        const offenders = m.targets.filter(t => t.small);
        check('target size >=24x24 CSS px', !offenders.length,
          offenders.length ? offenders.map(t => t.target + ' ' + t.width + 'x' + t.height).join('; ') : m.targets.length + ' targets; offenders=[]');
        console.log('WATCHED TARGETS ' + m.targets.filter(t => t.target === '#remember-agent' || t.target.startsWith('a[')).map(t => t.target + ' ' + t.width + 'x' + t.height).join('; '));
        check('live regions retained', m.liveIDs.length >= requiredLiveIDs.length && !m.missingLiveIDs.length,
          'count=' + m.liveIDs.length + '; minimum=' + requiredLiveIDs.length + '; missing=' + JSON.stringify(m.missingLiveIDs));
      }
      unexpected.push(...await evaluate('window.__a11yUnexpectedRequests'));
    }
    check('shipped JS and fixture requests', !pageErrors.length && !unexpected.length,
      'pageErrors=' + JSON.stringify(pageErrors) + '; unexpectedRequests=' + JSON.stringify(unexpected));
    console.log(`RESULT ${checks - failures}/${checks} passed; ${failures} failed`);
    console.log('LIMITS no screen reader, contrast, populated sessions, result announcements or focus-visibility test; target-size rule is strict (no WCAG exceptions).');
    if (failures) process.exitCode = 1;
  } finally {
    if (closeBrowser && browser.exitCode === null && browser.signalCode === null) {
      await closeBrowser().catch(() => {});
      await Promise.race([exit, delay(3000)]);
    }
    if (browser && browser.exitCode === null && browser.signalCode === null) browser.kill('SIGKILL');
    if (exit) await exit;
    failPending(new Error('audit finished'));
    if (profile) {
      try { await fs.promises.rm(profile, {recursive: true, force: true, maxRetries: 10, retryDelay: 200}); }
      catch (error) { console.error('WARN profile cleanup: ' + error.message); }
    }
  }
}
run().catch(error => { console.error('FAIL audit infrastructure: ' + error.stack); process.exitCode = 1; });
