#!/usr/bin/env node
// DF-CRIER-468: the agent-class read is enabled by capability, never blanket.
// Stub HTTP + DOM; execute the shipped state, /status handling and the class
// control's predicate — not a copied predicate.
//
// Red reproduction (run BEFORE the fix lands, as the foreman's reviewer did):
//   git show HEAD~1:cmd/server/chat.html > /tmp/df-crier-468-before.html
//   node scripts/chat-class-truth.cjs --html /tmp/df-crier-468-before.html
// Against the pre-fix page the disabled/named-reason arms FAIL — the control
// rendered focused-and-enabled while the management credential could not be
// presented. This commit's git history is the RED half of that reproduction.
//
// Design contract (recorded divergence from the dispatch brief's arm (c)):
// cmd/server/status.go:534-538 reports management_surface="admin-token-gated"
// only when THIS request passed the admin gate — an authorized deployment
// gateway holds the credential (the DF-CRIER-410 product posture). That is the
// one posture in which this viewer's credential is PROVEN by the server's own
// readback, so the control MUST be enabled there: disabling it there would be
// exactly the blanket-disable the acceptance forbids. management_surface=
// "disabled" means the delivery ACL is off: permissions_read.go's
// managementSnapshot (managementAuthorized needs a wired permissions store AND
// the armed admin token) then refuses GET /agents/{id}/class for EVERY client
// with "the permissions management surface is not armed" — the live browser
// battery (scripts/chat-management-browser.cjs, the ACL-off cell asserting the
// class submit refuses) and scripts/chat-refusal-truth.cjs pin that refusal.
// So the per-capability arm is: enabled exactly under "admin-token-gated";
// disabled with the missing authority named (Authorization: Bearer
// CR_PERMISSIONS_ADMIN_TOKEN) under "enabled-unarmed", "disabled", unknown
// postures, unread posture and failed status reads — same fail-closed shape
// DF-CRIER-460 gave the lifecycle buttons. The reachable NOT-disabled control
// is proven twice: the class control itself under "admin-token-gated", and
// POST /trust/verify under ACL-off (ordinary reach, no admin credential —
// enabled there, disabled nowhere by this rule).
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
  constructor() { this.hidden = false; this.disabled = false; this.textContent = ''; this.className = ''; this.title = ''; this.value = ''; this.dataset = {}; this.listeners = {}; this.children = []; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  setAttribute() {}
  removeAttribute() {}
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  set innerHTML(value) { this._html = String(value); }
  get innerHTML() { return this._html ?? ''; }
  get classList() { return {add: (...names) => { this.className += ' ' + names.join(' '); }}; }
  async click() { if (!this.disabled && !this.hidden) await this.listeners.click?.(); }
}
function client(surface) {
  const elements = new Map(), requests = [];
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  // Initialize the actual shipped state, including the default unread posture.
  const stateSource = html.match(/^const state = .*;$/m)?.[0];
  assert.ok(stateSource, 'shipped state declaration');
  const responses = {'/status': {status: 200, body: {require_agent_signature: false}}};
  // An absent field is part of the contract under test: omit the key entirely.
  if (surface !== undefined) responses['/status'].body.management_surface = surface;
  const context = vm.createContext({$, document: {createElement: () => new Element(), querySelectorAll: () => []}, Response, AbortSignal, DOMException,
    Date, setTimeout, clearTimeout,
    sessionPath: (id, suffix) => `/sessions/${id}/${suffix}`,
    renderDirectory() {}, renderSessions() {}, renderThreadOptions() {}, renderMessages() {}, renderParticipants() {}, renderPermissions() {}, renderTasks() {}, updateMessageTaskLinks() {}, renderHeader() {},
    fetch: async (route, options = {}) => {
      requests.push({route, ...options});
      let response = responses[route];
      if (typeof response === 'function') response = await response();
      if (response instanceof Error) throw response;
      response ||= {status: 200, body: {}};
      return new Response(typeof response.body === 'string' ? response.body : JSON.stringify(response.body), {status: response.status});
    },
  });
  vm.runInContext(stateSource + '\n' +
    extract('function esc(', 'function errorText(') +
    extract('function errorText(', '$("trust-query-form")') +
    extract('function composerState(', 'function renderReply(') +
    extract('function renderPosture(', 'function openSession(') +
    extract('function groupMembers(', 'function renderPermissions('), context);
  const run = expression => vm.runInContext(expression, context);
  run('state.current = "room"; state.agent = "atlas";');
  return {$, run, responses, requests, refresh: () => run('refresh()'),
    button: () => $('class-query-submit'), reason: () => $('class-read-reason'),
    trust: () => $('trust-query-submit'), panel: () => $('management-class'),
    fire: async () => {
      $('class-agent').value = 'atlas';
      const submit = $('agent-class-form').listeners.submit;
      assert.ok(submit, 'shipped submit handler registered');
      await submit({preventDefault() {}});
    }};
}
const classBody = {id: 'atlas', class: 'personal', owner: 'principal-1', namespace: '', capabilities: [], scopes: []};
const cases = [
  ['shipped markup: the control names its authority (aria-describedby) and the requirement precedes the input', () => {
    const button = html.match(/<button[^>]+id="class-query-submit"[^>]*>/)?.[0];
    assert.ok(button, 'class-query-submit markup');
    assert.match(button, /aria-describedby="class-read-reason class-read-requirement"/);
    const requirement = html.indexOf('id="class-read-requirement"'), form = html.indexOf('id="agent-class-form"'), input = html.indexOf('id="class-agent"');
    assert.ok(requirement >= 0 && requirement < form && form < input, 'the requirement statement precedes the form and its input');
    assert.match(html.slice(requirement, form), /CR_PERMISSIONS_ADMIN_TOKEN/, 'the requirement names the credential before anything is typed');
    assert.match(html, /id="class-read-reason"[^>]*role="status"/, 'the named reason is a live status slot');
  }],
  ['enabled-unarmed: (a) the control renders disabled with the authority named before any submit', async () => {
    const c = client('enabled-unarmed'); await c.refresh();
    assert.equal(c.button().disabled, true, 'the gate is live and this viewer holds no credential');
    assert.match(c.button().title, /Authorization: Bearer CR_PERMISSIONS_ADMIN_TOKEN/);
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ADMIN_TOKEN/);
  }],
  ['enabled-unarmed: (a) direct submit cannot fire a structurally refused read', async () => {
    const c = client('enabled-unarmed'); await c.refresh();
    await c.fire();
    assert.equal(c.requests.filter(r => r.route === '/agents/atlas/class').length, 0, 'no request leaves the page');
    assert.equal(c.panel().dataset.readState, undefined, 'no read state was recorded — the refusal never ran');
    assert.match(html, /id="management-class"[^>]*>No agent class query submitted\./, 'the shipped panel starts unqueried');
  }],
  ['admin-token-gated: (c) the gate readback proves THIS viewer — the control is NOT disabled and submits', async () => {
    const c = client('admin-token-gated'); await c.refresh();
    assert.equal(c.button().disabled, false, 'this viewer\'s /status request itself passed the admin gate — capability, not blanket');
    assert.equal(c.button().title, '');
    assert.equal(c.reason().textContent, '');
    c.responses['/agents/atlas/class'] = {status: 200, body: classBody};
    await c.fire();
    const hits = c.requests.filter(r => r.route === '/agents/atlas/class');
    assert.equal(hits.length, 1);
    assert.ok(!hits[0].method, 'the read is a plain GET — no credential header invented');
    assert.equal(c.panel().dataset.readState, 'populated');
    assert.match(c.panel().textContent, /Populated — GET \/agents\/atlas\/class succeeded\. 1 record\(s\) returned\./);
    assert.equal(c.button().disabled, false);
  }],
  ['admin-token-gated: the read in flight disables the control with the busy reason, then recovers', async () => {
    const c = client('admin-token-gated'); await c.refresh();
    let release;
    c.responses['/agents/atlas/class'] = () => new Promise(r => { release = r; });
    const firing = c.fire();
    assert.equal(c.button().disabled, true, 'the read is in progress');
    assert.match(c.button().title, /wait for it to finish/i);
    release({status: 200, body: classBody});
    await firing;
    assert.equal(c.button().disabled, false);
    assert.equal(c.panel().dataset.readState, 'populated');
  }],
  ['disabled surface: the route refuses every client (no store wired) — the not-armed reason names the configuration, and the reachable trust control is NOT disabled', async () => {
    const c = client('disabled'); await c.refresh();
    assert.equal(c.button().disabled, true, 'CR_PERMISSIONS_ENABLED=false wires no permissions store: GET /agents/{id}/class refuses every client, never only this page');
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ENABLED=false/);
    assert.match(c.reason().textContent, /not armed/);
    assert.equal(c.trust().disabled, false, 'POST /trust/verify evaluates with ordinary reach on this deployment — the capability rule is per-control, not blanket');
  }],
  ['unknown future posture: disabled, reason names the credential', async () => {
    const c = client('future-posture'); await c.refresh();
    assert.equal(c.button().disabled, true);
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ADMIN_TOKEN/);
  }],
  ['unreported posture (field absent): disabled, never inferred capable', async () => {
    const c = client(undefined);
    assert.ok(!JSON.stringify(c.responses['/status'].body).includes('management_surface'), 'the /status fixture omits the field entirely');
    await c.refresh();
    assert.equal(c.button().disabled, true);
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ADMIN_TOKEN/);
  }],
  ['failed status read invalidates an earlier capable observation and blocks submit', async () => {
    const c = client('admin-token-gated'); await c.refresh();
    assert.equal(c.button().disabled, false);
    c.responses['/status'] = new TypeError('connection refused');
    await c.refresh();
    assert.equal(c.button().disabled, true);
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ADMIN_TOKEN/);
    await c.fire();
    assert.equal(c.requests.filter(r => r.route === '/agents/atlas/class').length, 0);
  }],
  ['pending status read never enables on the previous observation', async () => {
    const c = client('admin-token-gated'); await c.refresh();
    assert.equal(c.button().disabled, false);
    let release;
    c.responses['/status'] = () => new Promise(r => { release = r; });
    const pending = c.refresh();
    assert.equal(c.button().disabled, true, 'disable immediately while this refresh\'s posture read is unread');
    release({status: 200, body: {management_surface: 'admin-token-gated', require_agent_signature: false}});
    await pending;
    assert.equal(c.button().disabled, false);
  }],
  ['unread posture at page load: the control is not focusable-and-enabled before the first /status answer', () => {
    const c = client('disabled');
    c.run('updateClassControls()');
    assert.equal(c.button().disabled, true);
    assert.match(c.reason().textContent, /CR_PERMISSIONS_ADMIN_TOKEN/);
  }],
];
(async () => {
  let failures = 0;
  const failLabel = process.argv[2] === '--html' ? 'RED (pre-existing defect)' : 'FAIL';
  for (const [name, test] of cases) {
    try { await test(); console.log('PASS ' + name); }
    catch (error) { failures++; console.error(failLabel + ' ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failures}/${cases.length} passed; ${failures} failed (stub HTTP + DOM; shipped state, /status handling, predicate, submit handler and rendering)`);
  if (failures) process.exitCode = 1;
})();