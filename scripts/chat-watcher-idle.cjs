#!/usr/bin/env node
// DF-CRIER-455: shipped watcher + fake clock/visibility, no dependencies.
// node scripts/chat-watcher-idle.cjs [--html /absolute/pre-change-chat.html]
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-watcher-idle.cjs [--html FILE]');
const html = fs.readFileSync(args[1] || path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
const shipped = extract('function outcomeLabel(', 'async function send(event)') +
  extract('const PROGRESS_READ_GRACE_MS', '$("close-session").addEventListener');
const live = key => ({idempotency_key: key, dispatch_live: {
  started_at: '2026-10-21T12:00:00Z', elapsed_ms: 45000, budget_ms: 120000,
  targets: [{target: 'slow', delivery_class: 'blocking_webhook', attempts: 0}],
}});
function client(hidden = false) {
  let now = Date.parse('2026-10-21T12:00:45Z'), nextID = 1, reads = 0, hiddenScheduled = 0;
  let hold = false, pending = [], record, refreshes = 0;
  const timers = new Map(), listeners = new Map(), elements = new Map();
  const state = {messages: [], sendWatch: null};
  const document = {hidden, addEventListener(name, fn) {
    const list = listeners.get(name) || []; list.push(fn); listeners.set(name, list);
  }};
  const element = id => {
    if (!elements.has(id)) elements.set(id, {hidden: true, textContent: '', className: ''});
    return elements.get(id);
  };
  // At most ONE watcher timer, including inside timer callbacks and awaits.
  const schedule = (fn, delay, interval) => {
    const id = nextID++;
    timers.set(id, {fn, at: now + delay, interval});
    if (document.hidden) hiddenScheduled++;
    assert.ok(timers.size <= 1, `duplicate watcher timers: ${timers.size}`);
    return id;
  };
  const context = vm.createContext({state, document, $: element, stamp: String,
    Date: class extends Date { static now() { return now; } },
    setTimeout: (fn, ms) => schedule(fn, ms, 0), clearTimeout: id => timers.delete(id),
    setInterval: (fn, ms) => schedule(fn, ms, ms), clearInterval: id => timers.delete(id),
    sessionPath: (id, suffix) => `/sessions/${id}/${suffix}`,
    call: async () => { reads++; if (hold) await new Promise(resolve => pending.push(resolve)); return {messages: [record]}; },
    refresh: async () => { refreshes++; },
  });
  vm.runInContext(shipped, context);
  const run = source => vm.runInContext(source, context);
  const drain = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
  const check = () => assert.ok(timers.size <= 1, `duplicate watcher timers: ${timers.size}`);
  return {state, element, text: () => element('send-progress-text').textContent,
    reads: () => reads, timers: () => timers.size, hiddenScheduled: () => hiddenScheduled, refreshes: () => refreshes,
    cadence: () => run('typeof PROGRESS_POLL_INTERVAL_MS === "number" ? PROGRESS_POLL_INTERVAL_MS : 1000'),
    async start(key = 'k') {
      if (!state.sendWatch || state.sendWatch.key !== key || state.sendWatch.done) {
        state.sendWatch = {session: 'room', key, startedAt: now, settled: false, done: false, pendingPolls: 0};
        record = live(key);
      }
      run('if (typeof startSendProgressWatcher === "function") startSendProgressWatcher();');
      await drain(); check();
    },
    async visibility(value) {
      document.hidden = value;
      for (const fn of listeners.get('visibilitychange') || []) fn();
      await drain(); check();
    },
    async advance(ms) {
      const target = now + ms;
      for (;;) {
        const entry = [...timers].sort((a, b) => a[1].at - b[1].at)[0];
        if (!entry || entry[1].at > target) break;
        const [id, timer] = entry; now = timer.at;
        if (timer.interval) timer.at += timer.interval; else timers.delete(id);
        timer.fn(); await drain(); check();
      }
      now = target; await drain(); check();
    },
    hold: () => { hold = true; },
    async release() { hold = false; for (const resolve of pending) resolve(); pending = []; await drain(); check(); },
    terminal: () => { record = {idempotency_key: state.sendWatch.key, outcomes: [{target: 'slow', outcome: 'delivered', retryable: false}]}; },
    async finish() { state.sendWatch.done = true; run('clearSendProgress()'); await drain(); check(); },
    poll: async () => { await run('watchSendProgress()'); await drain(); check(); },
  };
}
const cases = [
  ['hidden for five cadences: zero reads, zero timers, zero scheduling', async c => {
    await c.start(); await c.advance(c.cadence());
    const before = c.reads();
    await c.visibility(true); await c.advance(5 * c.cadence());
    console.log(`  hidden reads over five cadences: ${c.reads() - before}; live timers: ${c.timers()}`);
    assert.equal(c.reads() - before, 0, 'hidden watcher must not poll (pre-change: one poll every second)');
    assert.equal(c.timers(), 0, 'hidden watcher owns no timer');
    assert.equal(c.hiddenScheduled(), 0, 'no timer scheduled while hidden');
  }],
  ['hidden start and repeated starts stay idle; stale line is paused, not red', async () => {
    const c = client(true); await c.start(); await c.start(); await c.advance(3 * c.cadence());
    assert.equal(c.reads(), 0); assert.equal(c.timers(), 0); assert.equal(c.hiddenScheduled(), 0);
    assert.match(c.text(), /paused — tab hidden/);
    assert.doesNotMatch(c.text(), /refused|failed|\d+(?:\.\d+)?s/);
    assert.equal(c.element('send-progress').className, 'gate');
    assert.equal(c.element('send-progress-spinner').hidden, true);
  }],
  ['show reads immediately, then resumes exactly one normal cadence', async c => {
    await c.start(); await c.advance(c.cadence()); await c.visibility(true);
    await c.advance(3 * c.cadence());
    assert.match(c.text(), /paused — tab hidden/);
    assert.match(c.text(), /Last reading/);
    const before = c.reads(); await c.visibility(false);
    assert.equal(c.reads(), before + 1, 'show must poll without clock advancement');
    assert.doesNotMatch(c.text(), /paused — tab hidden/);
    assert.equal(c.timers(), 1);
    await c.advance(c.cadence() - 1); assert.equal(c.reads(), before + 1);
    await c.advance(1); assert.equal(c.reads(), before + 2); assert.equal(c.timers(), 1);
  }],
  ['duplicate starts and repeated hide/show keep a single owner; finish stays stopped', async c => {
    await c.start(); await c.start(); assert.equal(c.timers(), 1);
    await c.advance(c.cadence()); assert.equal(c.reads(), 1);
    for (let i = 0; i < 4; i++) {
      await c.visibility(true); await c.start(); assert.equal(c.timers(), 0);
      await c.visibility(false); await c.start(); assert.equal(c.timers(), 1);
    }
    await c.finish(); assert.equal(c.timers(), 0);
    const before = c.reads(); await c.visibility(true); await c.visibility(false); await c.advance(4 * c.cadence());
    assert.equal(c.reads(), before); assert.equal(c.timers(), 0);
  }],
  ['hide/show during an outstanding read never overlaps; resume is immediate after settlement', async c => {
    await c.start(); c.hold(); await c.advance(c.cadence()); assert.equal(c.reads(), 1);
    await c.visibility(true); await c.visibility(false); await c.start(); await c.visibility(true);
    await c.advance(4 * c.cadence()); assert.equal(c.reads(), 1); assert.equal(c.timers(), 0);
    await c.release(); assert.match(c.text(), /paused — tab hidden/); assert.equal(c.timers(), 0);
    await c.visibility(false); assert.equal(c.reads(), 2); assert.equal(c.timers(), 1);
    c.hold(); await c.advance(c.cadence()); const before = c.reads();
    await c.visibility(true); await c.visibility(false); assert.equal(c.reads(), before);
    await c.release(); assert.equal(c.reads(), before + 1); assert.equal(c.timers(), 1);
  }],
  ['replacing or finishing a ticket during a read cannot resurrect a timer', async c => {
    await c.start(); c.hold(); await c.advance(c.cadence());
    await c.start('replacement'); await c.start('replacement');
    await c.release(); assert.equal(c.reads(), 2); assert.equal(c.timers(), 1);
    assert.match(c.text(), /Delivery in flight/);
    c.hold(); await c.advance(c.cadence()); await c.visibility(true); await c.finish();
    await c.visibility(false); await c.release();
    assert.equal(c.timers(), 0); assert.equal(c.element('send-progress').hidden, true);
  }],
  ['terminal read ends cadence and cannot be relabelled paused', async c => {
    await c.start(); await c.advance(c.cadence()); c.terminal();
    await c.advance(c.cadence());
    assert.match(c.text(), /Delivery finished/); assert.equal(c.timers(), 0); assert.equal(c.refreshes(), 1);
    const before = c.reads(); await c.visibility(true); await c.visibility(false); await c.advance(3 * c.cadence());
    assert.equal(c.reads(), before); assert.doesNotMatch(c.text(), /paused/);
  }],
  ['direct watcher invocation cannot bypass hidden protection', async c => {
    await c.start(); await c.visibility(true); const before = c.reads();
    await c.poll(); assert.equal(c.reads(), before);
  }],
];
(async () => {
  let failures = 0;
  for (const [name, test] of cases) {
    try { await test(client()); console.log('PASS ' + name); }
    catch (error) { failures++; console.error('FAIL ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failures}/${cases.length} passed; ${failures} failed (fake clock/visibility; shipped watcher)`);
  if (failures) process.exitCode = 1;
})();
