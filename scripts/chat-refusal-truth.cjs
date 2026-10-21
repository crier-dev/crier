#!/usr/bin/env node
// DF-CRIER-459: execute shipped HTTP/read/render code, no dependencies or copied classifier.
// Historical input: node scripts/chat-refusal-truth.cjs --html /tmp/df-crier-459-before.html
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
  constructor() { this.hidden = false; this.className = ''; this.dataset = {}; this.children = []; this._text = ''; this.open = true; }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this._text = ''; this.children = nodes; }
  setAttribute() {}
  get classList() { return {add: (...names) => { this.className += ' ' + names.join(' '); }}; }
}
function client(responses = {}) {
  const elements = new Map();
  // Mirror only element operations; all classification, fetch and rendering run from chat.html.
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const document = {createElement: () => new Element()};
  const context = vm.createContext({$, document, state: {agent: '', namespace: ''}, Response, AbortSignal, DOMException,
    setTimeout, clearTimeout,
    fetch: async route => {
      const r = responses[route] || {status: 200, body: {items: [], peers: [], anchors: []}};
      if (r instanceof Error) throw r;
      return new Response(typeof r.body === 'string' ? r.body : JSON.stringify(r.body), {status: r.status});
    },
  });
  vm.runInContext(extract('function errorText(', '// DF-CRIER-384: the Authority view') +
    extract('function authorityOpen(', '$("management-refresh").addEventListener') +
    extract('function renderPosture(', 'async function refresh()'), context);
  const run = expression => vm.runInContext(expression, context);
  const visible = () => {
    const leaves = [];
    const visit = el => { if (el.hidden) return; if (/\bmanagement-read\b/.test(el.className) && el.textContent) leaves.push(el); else el.children.forEach(visit); };
    elements.forEach(visit);
    return leaves;
  };
  return {$, run, visible, responses, posture: b => { context.posture = b; run('renderPosture(posture)'); },
    list: (id, route) => run(`managementList(${JSON.stringify(id)}, ${JSON.stringify(route)}, "items", [], "No records")`),
    red: () => [...elements.values()].filter(e => !e.hidden && /\berror\b/.test(e.className)).length + visible().filter(e => /\berror\b/.test(e.className) && ![...elements.values()].includes(e)).length};
}
const routes = ['/principals', '/bindings', '/grants', '/trust/anchors'];
const disabled = {status: 403, body: {error: 'MANAGEMENT_FORBIDDEN', detail: 'the permissions management surface is not armed'}};
const refused = detail => ({status: 403, body: {error: 'MANAGEMENT_FORBIDDEN', detail}});
const posture = {auth_enabled: false, registry_durable: false, management_surface: 'disabled'};
const cases = [
  ['disabled posture is not red and names its configuration', async () => {
    const c = client(); c.posture(posture);
    assert.equal(c.red(), 0, 'N=0 failing facts: configured auth/durability must not be red');
    assert.doesNotMatch(c.$('posture').className, /\berror\b/);
    for (const knob of ['CR_AUTH_TOKEN', 'auth_enabled:false', 'CR_PERMISSIONS_ENABLED', 'CR_PERMISSIONS_ADMIN_TOKEN']) assert.ok(c.$('posture').textContent.includes(knob), knob);
    assert.match(c.$('posture').textContent, /configured posture, not a failure/);
  }],
  ['a reported health failure is red, not the configured posture', async () => {
    const c = client(); c.posture({...posture, registry_durable: true, health: 'degraded'});
    assert.equal(c.red(), 1); assert.match(c.$('health').className, /\berror\b/);
    assert.match(c.$('health').textContent, /Health: degraded/);
    assert.doesNotMatch(c.$('posture').className, /\berror\b/);
  }],
  ['four same-cause 403s collapse to one disabled statement with route detail', async () => {
    const c = client(Object.fromEntries(routes.map(r => [r, disabled])));
    await c.run('refreshManagement()');
    const items = c.visible().filter(e => /not armed/.test(e.textContent));
    assert.equal(items.length, 1, 'one statement, not four repeated refusals');
    assert.match(items[0].textContent, /Disabled by configuration.*configured posture, not a failure/);
    assert.match(items[0].className, /\bdisabled\b/);
    assert.equal(c.red(), 0);
    for (const r of routes) assert.ok(items[0].textContent.includes('GET ' + r), r);
    assert.ok(items[0].children.some(e => /failure-routes/.test(e.className)), 'routes are a secondary line');
    for (const knob of ['CR_PERMISSIONS_ENABLED', 'CR_PERMISSIONS_ADMIN_TOKEN']) assert.ok(items[0].textContent.includes(knob));
  }],
  ['one distinct 403 plus one 500 stay separate; only the 500 is red', async () => {
    const c = client({'/principals': refused('actor lacks the manage grant'), '/bindings': {status: 500, body: {error: 'permission store unavailable'}}});
    c.posture(posture);
    await Promise.all([c.list('a', '/principals'), c.list('b', '/bindings')]);
    const items = c.visible(); assert.equal(items.length, 2);
    const denial = items.find(e => /actor lacks the manage grant/.test(e.textContent));
    assert.ok(denial); assert.match(denial.textContent, /^Refused/); assert.match(denial.className, /\brefused\b/); assert.doesNotMatch(denial.className, /\berror\b/);
    const failure = items.find(e => /permission store unavailable/.test(e.textContent));
    assert.ok(failure); assert.match(failure.textContent, /^Failed/); assert.match(failure.className, /\berror\b/); assert.equal(c.red(), 1);
  }],
  ['different 403 causes are not collapsed', async () => {
    const c = client({'/principals': refused('actor lacks the manage grant'), '/bindings': refused('actor is suspended')});
    await Promise.all([c.list('a', '/principals'), c.list('b', '/bindings')]);
    assert.equal(c.visible().length, 2); assert.equal(c.red(), 0);
    for (const item of c.visible()) { assert.match(item.textContent, /^Refused/); assert.match(item.className, /\brefused\b/); }
  }],
  ['armed gate requesting admin access is refused, not disabled', async () => {
    const c = client({'/principals': refused('an admin token is required (CR_PERMISSIONS_ADMIN_TOKEN)')});
    await c.list('a', '/principals'); const [item] = c.visible();
    assert.match(item.textContent, /^Refused.*admin token is required/s); assert.match(item.className, /\brefused\b/); assert.equal(c.red(), 0);
    assert.doesNotMatch(item.textContent, /Disabled by configuration|configured posture/);
  }],
  ['connection refusal and malformed 200/403 bodies are failed and red', async () => {
    for (const response of [new TypeError('connection refused'), {status: 200, body: '<html>not JSON</html>'}, {status: 403, body: 'not JSON'}]) {
      const c = client({'/principals': response}); await c.list('a', '/principals');
      const [item] = c.visible(); assert.match(item.textContent, /^Failed/); assert.match(item.className, /\berror\b/); assert.equal(c.red(), 1);
      assert.match(item.textContent, /connection refused|expected JSON/);
    }
  }],
  ['refreshed successes clear grouped failures and unhide route results', async () => {
    const c = client(Object.fromEntries(routes.map(r => [r, disabled])));
    await c.run('refreshManagement()');
    for (const r of routes) delete c.responses[r];
    await c.run('refreshManagement()');
    assert.equal(c.$('management-failures').textContent, '');
    assert.equal(c.$('management-failures').hidden, true);
    assert.equal(c.red(), 0);
    for (const id of ['management-principals', 'management-bindings', 'management-grants', 'management-anchors']) {
      assert.equal(c.$(id).hidden, false); assert.match(c.$(id).textContent, /^Empty/);
    }
  }],
  ['same wording with different server codes is not one cause', async () => {
    const c = client({'/principals': {status: 403, body: {error: {code: 'GRANT_MISSING', message: 'access denied'}}}, '/bindings': {status: 403, body: {error: {code: 'ACTOR_SUSPENDED', message: 'access denied'}}}});
    await Promise.all([c.list('a', '/principals'), c.list('b', '/bindings')]);
    assert.equal(c.visible().length, 2); assert.equal(c.red(), 0);
  }],
  ['401 names its cause without red or an invented disabled posture', async () => {
    const c = client({'/principals': {status: 401, body: {error: 'missing Authorization header'}}});
    await c.list('a', '/principals'); const [item] = c.visible();
    assert.match(item.textContent, /^Refused.*missing Authorization header/s);
    assert.match(item.className, /\brefused\b/); assert.equal(c.red(), 0);
  }],
  ['disabled wording is not enough without the known code and 403 status', async () => {
    for (const response of [{status: 500, body: disabled.body}, {status: 403, body: {error: 'OTHER_DENIAL', detail: disabled.body.detail}}]) {
      const c = client({'/principals': response}); await c.list('a', '/principals');
      const [item] = c.visible(); assert.doesNotMatch(item.textContent, /^Disabled/);
      assert.match(item.textContent, response.status === 500 ? /^Failed/ : /^Refused/);
      assert.equal(c.red(), response.status === 500 ? 1 : 0);
    }
  }],
  ['status success clears its prior refusal in the grouped panel', async () => {
    const c = client({'/status': refused('viewer cannot read status')});
    await c.run('refreshManagement()');
    assert.ok(c.$('management-failures').textContent.includes('viewer cannot read status'));
    delete c.responses['/status']; await c.run('refreshManagement()');
    assert.equal(c.$('management-failures').textContent, '');
    assert.equal(c.$('management-federation-posture').hidden, false);
  }],
  ['CSS distinguishes disabled, refused and failed states', async () => {
    assert.ok(/\.management-read\.disabled\s*\{[^}]*var\(--dim\)/.test(html), 'disabled uses neutral styling');
    assert.ok(/\.management-read\.refused\s*\{[^}]*var\(--amber\)/.test(html), 'refused uses amber styling');
    assert.ok(/\.error\s*\{[^}]*var\(--red\)/.test(html), 'failed retains red styling');
    assert.ok(/id="management-failures"[^>]*role="status"/.test(html), 'group panel exists in the shipped DOM');
  }],
];
(async () => {
  let failures = 0;
  for (const [name, test] of cases) {
    try { await test(); console.log('PASS ' + name); }
    catch (error) { failures++; console.error('FAIL ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failures}/${cases.length} passed; ${failures} failed (stub HTTP; shipped client render + class attributes)`);
  if (failures) process.exitCode = 1;
})();
