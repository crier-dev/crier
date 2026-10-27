#!/usr/bin/env node
// DF-CRIER-456: execute the shipped client against stub HTTP responses.
// node scripts/chat-inflight-truth.cjs [--html /absolute/pre-change-chat.html]
// The historical file is selectable so RED evidence stays reproducible after commit.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(process.argv[2] === '--html' ? process.argv[3] : path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
const shipped = extract('async function call(', 'function notice(') +
  extract('function outcomeLabel(', 'async function send(event)') +
  extract('const PROGRESS_READ_GRACE_MS', 'setInterval(watchSendProgress, 1000);');
function client() {
  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, {hidden: true, textContent: '', className: ''});
    return elements.get(id);
  };
  let response = {messages: []}, refreshes = 0, reads = 0;
  const state = {current: 'room', agent: 'operator', messages: [], sendWatch: null};
  const context = vm.createContext({state, $: element, stamp: String, Response, AbortSignal,
    Date, setTimeout, clearTimeout, DOMException,
    sessionPath: (id, suffix) => `/sessions/${id}/${suffix}`,
    refresh: async () => { refreshes++; },
    fetch: async () => { reads++; return new Response(typeof response === 'string' ? response : JSON.stringify(response), {status: 200}); },
  });
  vm.runInContext(shipped, context);
  state.sendWatch = {session: 'room', key: 'k', startedAt: Date.now(), settled: false, done: false, budgetMs: null, pendingPolls: 0};
  return {state, element, text: () => element('send-progress-text').textContent,
    response: value => { response = value; },
    poll: () => vm.runInContext('watchSendProgress()', context),
    line: value => { context.target = value; return vm.runInContext('progressTargetLine(target)', context); },
    reads: () => reads, refreshes: () => refreshes};
}
const live = targets => ({idempotency_key: 'k', dispatch_live: {started_at: '2026-10-21T12:00:00Z', elapsed_ms: 45000, budget_ms: 120000, targets}});
const failure = {idempotency_key: 'k', outcomes: [{target: 'bad-bot', outcome: 'failed', detail: 'reply extraction: reply is not valid json', retryable: false}]};
const cases = [
  ['federated: absent attempts names class, never waiting', async c => {
    c.response({messages: [live([{target: 'peer/bot', class: 'federated'}])]});
    await c.poll();
    assert.match(c.text(), /federated.*no attempt evidence/i);
    assert.doesNotMatch(c.text(), /no attempt has returned yet|waiting|still pending/i);
  }],
  ['HTTP 200 invalid JSON: recorded failure wins', async c => {
    c.response({messages: [live([{target: 'bad-bot', attempts: 1, status: 200}])]});
    await c.poll();
    c.state.messages = [failure];
    c.response('<html>not JSON</html>');
    await c.poll();
    assert.match(c.text(), /reply extraction: reply is not valid json/);
    assert.match(c.text(), /could not parse.*HTTP 200/i);
    assert.doesNotMatch(c.text(), /still pending|outcome.*unknown/i);
    assert.equal(c.element('send-progress-spinner').hidden, true);
    assert.equal(c.state.sendWatch.done, true);
  }],
  ['120000ms server budget at 45000ms: not exhausted', async c => {
    c.state.sendWatch.startedAt = Date.now() - 45001;
    c.response({messages: [live([{target: 'slow', delivery_class: 'blocking_webhook', attempts: 0}])]});
    await c.poll();
    assert.equal(c.reads(), 1, 'must consult server before any observation cutoff');
    assert.match(c.text(), /45\.0s of the 120\.0s delivery budget/);
    assert.doesNotMatch(c.text(), /past its budget|budget exhausted|stopped watching/);
  }],
  ['blocking webhook: explicit zero versus absent evidence', async c => {
    assert.match(c.line({target: 'slow', delivery_class: 'blocking_webhook', attempts: 0}), /no attempt has returned yet/);
    assert.match(c.line({target: 'slow', delivery_class: 'blocking_webhook'}), /attempt evidence.*not reported/i);
    assert.doesNotMatch(c.line({target: 'unknown'}), /yet|waiting/);
  }],
  ['missing budget stays unknown, never zero or client default', async c => {
    const record = live([]); delete record.dispatch_live.budget_ms;
    c.response({messages: [record]}); await c.poll();
    assert.match(c.text(), /budget.*not reported|budget.*unknown/i);
    assert.doesNotMatch(c.text(), /of the (0\.0|30\.0)s/);
  }],
  ['unparseable HTTP 200 without verdict: explicitly unknown', async c => {
    c.response('not json'); await c.poll();
    assert.match(c.text(), /could not parse.*HTTP 200/i);
    assert.match(c.text(), /outcome.*unknown/i);
    assert.doesNotMatch(c.text(), /still pending|Delivery finished|endpoint answered/);
  }],
  ['returned HTTP 200 is not a pending delivery outcome', async c => {
    const text = c.line({target: 'bad-bot', attempts: 1, status: 200});
    assert.match(text, /HTTP 200/);
    assert.doesNotMatch(text, /still pending|delivery.*succeeded/);
  }],
  ['terminal transcript outcome overrides live snapshot for same pass', async c => {
    c.response({messages: [{...live([{target: 'bad-bot', attempts: 1, status: 200}]), ...failure}]});
    await c.poll();
    assert.match(c.text(), /reply is not valid json/);
    assert.doesNotMatch(c.text(), /still pending/);
    assert.equal(c.element('send-progress-spinner').hidden, true);
  }],
  ['prior failed pass is not the current server-reported retry', async c => {
    const record = {...live([{target: 'bad-bot', delivery_class: 'blocking_webhook', attempts: 0}]),
      outcomes: [{...failure.outcomes[0], updated_at: '2026-10-21T11:59:00Z'}]};
    c.response({messages: [record]}); await c.poll();
    assert.match(c.text(), /Delivery in flight/);
    assert.equal(c.state.sendWatch.done, false);
    assert.equal(c.element('send-progress-spinner').hidden, false);
  }],
  ['in_progress is not terminal and retryability is not invented', async c => {
    c.response({messages: [{idempotency_key: 'k', outcomes: [{target: 'busy', outcome: 'in_progress', retryable: true}]}]});
    await c.poll();
    assert.doesNotMatch(c.text(), /Delivery finished/);
    assert.match(c.text(), /in progress/);
    assert.match(c.text(), /do not send again/i);
    assert.equal(c.state.sendWatch.done, false);
  }],
  ['parse failure during retry does not resurrect prior failure', async c => {
    const prior = {...failure, outcomes: [{...failure.outcomes[0], updated_at: '2026-10-21T11:59:00Z'}]};
    c.state.messages = [prior];
    c.response({messages: [{...live([{target: 'bad-bot', delivery_class: 'blocking_webhook', attempts: 0}]), outcomes: prior.outcomes}]});
    await c.poll();
    c.response('bad json'); await c.poll();
    assert.match(c.text(), /could not parse.*HTTP 200/i);
    assert.doesNotMatch(c.text(), /Delivery finished|reply is not valid json/);
    assert.equal(c.state.sendWatch.done, false);
  }],
  ['terminal failure is read before the browser observation cutoff', async c => {
    c.state.sendWatch.startedAt = Date.now() - 300000;
    c.response({messages: [failure]}); await c.poll();
    assert.match(c.text(), /reply is not valid json/);
    assert.doesNotMatch(c.text(), /stopped watching|still pending/);
  }],
  ['browser cutoff never claims an unknown server budget expired', async c => {
    c.state.sendWatch.startedAt = Date.now() - 300000;
    const record = live([]); delete record.dispatch_live.budget_ms;
    c.response({messages: [record]}); await c.poll();
    assert.match(c.text(), /budget unknown/);
    assert.match(c.text(), /browser observation window ended/);
    assert.doesNotMatch(c.text(), /past its budget|budget exhausted/);
    assert.equal(c.element('send-progress-spinner').hidden, true);
  }],
  ['server retryability is displayed verbatim, not inferred from failed', async c => {
    c.response({messages: [failure]}); await c.poll();
    assert.match(c.text(), /retryable=false/);
    c.state.sendWatch.done = false;
    c.response({messages: [{...failure, outcomes: [{...failure.outcomes[0], retryable: true}]}]}); await c.poll();
    assert.match(c.text(), /retryable=true/);
  }],
  ['terminal verdict also replaces the expired browser-wait reassurance', async c => {
    c.state.sendWatch.browserWaitEnded = true;
    c.element('send-result').textContent = 'Still delivering or awaiting confirmation';
    c.response({messages: [failure]}); await c.poll();
    assert.match(c.element('send-result').textContent, /reply is not valid json/);
    assert.doesNotMatch(c.element('send-result').textContent, /Still delivering|awaiting confirmation/);
  }],
  ['federated delivery_class and absent class are distinct', async c => {
    assert.match(c.line({target: 'remote', delivery_class: 'federated', attempts: 0}), /federated.*no attempt evidence/i);
    assert.match(c.line({target: 'local', attempts: 0}), /class.*not reported/i);
    assert.doesNotMatch(c.line({target: 'local', attempts: 0}), /yet|waiting/);
  }],
  // DF-CRIER-467: the explicit-task send used to reach its POST with key "", and
  // the watcher then fell back to the LAST transcript message — so an earlier,
  // unrelated delivery was printed as this request's outcome. A keyless ticket
  // has no per-request record to identify: it must say so and stop, never match
  // by position.
  ['keyless ticket never borrows another message delivery', async c => {
    c.state.sendWatch = {...c.state.sendWatch, key: ''};
    c.response({messages: [{idempotency_key: 'earlier-send', outcomes: [{target: 'bad-bot', outcome: 'delivered', retryable: false}]}]});
    await c.poll();
    assert.match(c.text(), /No per-request delivery record exists/i);
    assert.match(c.text(), /without an idempotency key/i);
    assert.doesNotMatch(c.text(), /Delivery finished|bad-bot: delivered/i);
    assert.equal(c.element('send-progress-spinner').hidden, true);
    assert.equal(c.state.sendWatch.done, true, 'a keyless ticket has nothing truthful left to poll for');
    assert.equal(c.reads(), 0, 'a keyless ticket must not read the transcript at all, so it cannot borrow');
  }],
  // The positive control for the same rule: a KEYED ticket reads its own record,
  // and does so over a later message that a positional match would have chosen.
  ['keyed ticket reads its own record, not the newest message', async c => {
    c.state.sendWatch = {...c.state.sendWatch, key: 'mine'};
    c.response({messages: [
      {idempotency_key: 'mine', outcomes: [{target: 'mine-bot', outcome: 'delivered', retryable: false}]},
      {idempotency_key: 'theirs', outcomes: [{target: 'other-bot', outcome: 'failed', retryable: false}]}
    ]});
    await c.poll();
    assert.match(c.text(), /mine-bot: delivered/);
    assert.doesNotMatch(c.text(), /other-bot/);
    assert.equal(c.reads(), 1);
  }],
];
(async () => {
  let failures = 0;
  for (const [name, test] of cases) {
    try { const c = client(); await test(c); console.log('PASS ' + name + '\n  ' + c.text().replaceAll('\n', ' | ')); }
    catch (error) { failures++; console.error('FAIL ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failures}/${cases.length} passed; ${failures} failed (stub HTTP; shipped client functions)`);
  if (failures) process.exitCode = 1;
})();
