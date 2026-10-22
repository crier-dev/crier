#!/usr/bin/env node
// DF-CRIER-460: stub HTTP + DOM operations; execute the shipped client, not a copied predicate.
// node scripts/chat-lifecycle-truth.cjs [--html /absolute/pre-change-chat.html]
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
class Element {
  constructor() { this.hidden = false; this.disabled = false; this.textContent = ''; this.className = ''; this.title = ''; this.value = ''; this.dataset = {}; this.listeners = {}; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  setAttribute() {}
  removeAttribute() {}
  get classList() { return {add: (...names) => { this.className += ' ' + names.join(' '); }}; }
  async click() { if (!this.disabled && !this.hidden) await this.listeners.click?.(); }
}
function client(surface = 'disabled', sessionState = 'open') {
  const elements = new Map(), requests = [];
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  // Initialize the actual shipped state, including the default unknown posture.
  const stateSource = html.match(/^const state = .*;$/m)?.[0];
  assert.ok(stateSource, 'shipped state declaration');
  let session = {id: 'room', state: sessionState, title: 'Fixture room', kind: 'channel', participant_count: 1, audience_count: 0};
  const responses = {'/status': {status: 200, body: {management_surface: surface, require_agent_signature: false}}};
  const context = vm.createContext({$, document: {querySelectorAll: () => []}, Response, AbortSignal, DOMException,
    Date, setTimeout, clearTimeout,
    sessionPath: (id, suffix) => `/sessions/${id}/${suffix}`,
    renderDirectory() {}, renderSessions() {}, renderThreadOptions() {}, renderMessages() {}, renderParticipants() {}, renderPermissions() {}, renderTasks() {}, updateMessageTaskLinks() {},
    fetch: async (route, options = {}) => {
      requests.push({route, ...options});
      let response = responses[route];
      if (typeof response === 'function') response = await response();
      if (response instanceof Error) throw response;
      if (!response && options.method === 'POST') {
        session = {...session, state: JSON.parse(options.body).state};
        response = {status: 200, body: session};
      }
      response ||= {status: 200, body: route.endsWith('/messages') ? {session, messages: []} : {agents: [], sessions: [session]}};
      return new Response(typeof response.body === 'string' ? response.body : JSON.stringify(response.body), {status: response.status});
    },
  });
  vm.runInContext(stateSource + '\n' + extract('function errorText(', 'function renderManagementFailures(') +
    extract('function composerState(', 'function renderReply(') + extract('function renderPosture(', 'function openSession(') +
    // DF-CRIER-449: refresh() now re-reads the named-group panel, so the shipped
    // group surface ships with the region this harness executes.
    extract('function groupMembers(', 'function renderPermissions(') +
    html.split('\n').filter(line => /^\$\("(?:close|reopen)-session"\)\.addEventListener/.test(line)).join('\n'), context);
  const run = expression => vm.runInContext(expression, context);
  run('state.current = "room"; state.agent = "atlas";');
  return {$, run, responses, requests, refresh: () => run('refresh()'), posts: () => requests.filter(r => r.method === 'POST'),
    button: () => $(sessionState === 'open' ? 'close-session' : 'reopen-session'), next: sessionState === 'open' ? 'closed' : 'open'};
}
const cause = 'admin on the session is required to close or reopen it';
const forbidden = {status: 403, body: {error: 'SESSION_STATE_FORBIDDEN', detail: cause}};
const cases = [
  ...['enabled-unarmed', 'admin-token-gated'].flatMap(surface => ['open', 'closed'].flatMap(sessionState => [
    [`${surface}/${sessionState}: (a) visible lifecycle control is disabled`, async () => {
      const c = client(surface, sessionState); await c.refresh();
      assert.equal(c.button().hidden, false);
      assert.equal(c.button().disabled, true, 'ACL-enabled is not lifecycle authority, even if management reads are authorized');
    }],
    [`${surface}/${sessionState}: (a) visible reason names bound principal and missing browser mechanism`, async () => {
      const c = client(surface, sessionState); await c.refresh();
      const reason = c.$('session-lifecycle-reason');
      assert.equal(reason.hidden, false);
      assert.match(reason.textContent, /bound principal/i);
      assert.match(reason.textContent, /admin on (the|this) session/i);
      assert.match(reason.textContent, /this page cannot present one.*DF-CRIER-333/i);
      assert.equal(c.button().title, reason.textContent);
    }],
    [`${surface}/${sessionState}: (a) click and direct handler cannot post`, async () => {
      const c = client(surface, sessionState); await c.refresh();
      await c.button().click();
      await c.run(`changeSessionState(${JSON.stringify(c.next)})`);
      assert.equal(c.posts().length, 0, 'neither a disabled click nor direct invocation may send the agent claim');
    }],
  ])),
  ...['open', 'closed'].map(sessionState => [`disabled/${sessionState}: (b) enabled control posts the unchanged body and reads resulting state`, async () => {
    const c = client('disabled', sessionState); await c.refresh();
    assert.equal(c.button().hidden, false); assert.equal(c.button().disabled, false);
    await c.button().click();
    assert.equal(c.posts().length, 1);
    const [request] = c.posts();
    assert.equal(request.route, '/sessions/room/state');
    assert.equal(request.body, JSON.stringify({state: c.next, sender: 'atlas'}));
    assert.equal(request.headers.Authorization, undefined, 'no new credential mechanism');
    assert.equal(c.run('state.session.state'), c.next);
    assert.match(c.$('session-lifecycle-result').textContent, /State transition recorded/);
    assert.doesNotMatch(c.$('session-lifecycle-result').className, /\berror\b/);
  }]),
  ['race: (c) 403 is refused, not red; server cause is verbatim', async () => {
    const c = client(); await c.refresh();
    assert.equal(c.button().disabled, false, 'the race starts with observed unarmed posture');
    c.responses['/sessions/room/state'] = forbidden;
    c.responses['/status'].body.management_surface = 'enabled-unarmed';
    await c.button().click();
    assert.equal(c.posts().length, 1);
    const result = c.$('session-lifecycle-result');
    assert.match(result.className, /\brefused\b/); assert.doesNotMatch(result.className, /\berror\b/);
    assert.ok(result.textContent.includes('HTTP 403: SESSION_STATE_FORBIDDEN — ' + cause), result.textContent);
    assert.match(result.textContent, /^Refused/);
    assert.equal(c.run('state.session.state'), 'open');
    assert.equal(c.button().disabled, true, 'fresh armed posture disables the next click');
  }],
  ...[undefined, 'future-posture', false].map(surface => [`unknown posture ${String(surface)}: disabled, explicitly unknown and no POST`, async () => {
    const c = client(surface);
    if (surface === undefined) delete c.responses['/status'].body.management_surface;
    await c.refresh();
    assert.equal(c.button().disabled, true);
    assert.match(c.$('session-lifecycle-reason').textContent, /ACL authority.*unknown/i);
    await c.run('changeSessionState("closed")'); assert.equal(c.posts().length, 0);
  }]),
  ['failed status read invalidates an earlier unarmed observation', async () => {
    const c = client(); await c.refresh(); assert.equal(c.button().disabled, false);
    c.responses['/status'] = new TypeError('connection refused'); await c.refresh();
    assert.equal(c.button().disabled, true);
    assert.match(c.$('session-lifecycle-reason').textContent, /unknown/i);
    await c.run('changeSessionState("closed")'); assert.equal(c.posts().length, 0);
  }],
  ['pending status read never enables using the previous unarmed observation', async () => {
    const c = client(); await c.refresh();
    let release;
    c.responses['/status'] = () => new Promise(resolve => { release = resolve; });
    const pending = c.refresh();
    try { assert.equal(c.button().disabled, true, 'disable immediately while current posture is unread'); }
    finally { release({status: 200, body: {management_surface: 'disabled', require_agent_signature: false}}); await pending; }
    assert.equal(c.button().disabled, false);
  }],
  ['signature required, missing actor, and busy operation remain disabled', async () => {
    for (const change of ['state.requireAgentSignature = true', 'state.agent = ""', 'state.sending = true']) {
      const c = client(); await c.refresh(); c.run(change + '; updateComposer()');
      assert.equal(c.button().disabled, true, change);
      await c.run('changeSessionState("closed")'); assert.equal(c.posts().length, 0);
    }
  }],
  ['unreadable session hides lifecycle controls despite unarmed ACL', async () => {
    const c = client(); c.responses['/sessions/room/messages'] = {status: 403, body: {error: 'SESSION_READ_FORBIDDEN'}};
    await c.refresh();
    assert.equal(c.$('close-session').hidden, true); assert.equal(c.$('reopen-session').hidden, true);
    await c.run('changeSessionState("closed")'); assert.equal(c.posts().length, 0);
  }],
  ['500 and malformed 403 remain red failures, not policy refusals', async () => {
    for (const response of [{status: 500, body: {error: 'STORE_ERROR', detail: 'store unavailable'}}, {status: 403, body: 'not JSON'}]) {
      const c = client(); await c.refresh(); c.responses['/sessions/room/state'] = response;
      await c.button().click();
      const result = c.$('session-lifecycle-result');
      assert.match(result.className, /\berror\b/); assert.doesNotMatch(result.className, /\brefused\b/);
      assert.match(result.textContent, /^Failed/);
    }
  }],
  ['shipped buttons describe their visible authority reason and result', async () => {
    for (const id of ['close-session', 'reopen-session']) {
      const button = html.match(new RegExp('<button[^>]+id="' + id + '"[^>]*>'))?.[0];
      assert.match(button, /aria-describedby="session-lifecycle-reason session-lifecycle-result"/);
    }
    assert.match(html, /id="session-lifecycle-reason"[^>]*role="status"/);
  }],
];
(async () => {
  let failures = 0;
  for (const [name, test] of cases) {
    try { await test(); console.log('PASS ' + name); }
    catch (error) { failures++; console.error('FAIL ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failures}/${cases.length} passed; ${failures} failed (stub HTTP + DOM; shipped state, refresh, predicates, click handlers and rendering)`);
  if (failures) process.exitCode = 1;
})();
