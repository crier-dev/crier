#!/usr/bin/env node
// DF-CRIER-447: stub DOM/HTTP, executing the shipped prompt, submit and renderer.
// node scripts/chat-late-join-truth.cjs [--html /absolute/pre-change-chat.html]
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-late-join-truth.cjs [--html <path>]');
const html = fs.readFileSync(args[1] || path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
class Element {
  constructor(tag = '') {
    this.hidden = /\bhidden\b/.test(tag); this.disabled = /\bdisabled\b/.test(tag);
    this.value = ''; this.dataset = {}; this.className = ''; this.listeners = {};
    this.textContent = ''; this.innerHTML = ''; this.scrollHeight = 100; this.scrollTop = 0; this.clientHeight = 100;
  }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  setAttribute() {}
  removeAttribute() {}
  focus() { this.focused = true; }
}
function client(populated = true) {
  const elements = new Map(), requests = [];
  const $ = id => {
    if (!elements.has(id)) {
      const tag = html.match(new RegExp('<[^>]+\\bid="' + id + '"[^>]*>'))?.[0] || '';
      const el = new Element(tag);
      if (tag.startsWith('<select')) {
        const options = html.slice(html.indexOf(tag) + tag.length).split('</select>')[0];
        el.value = options.match(/<option\b[^>]*value="([^"]*)"[^>]*\bselected\b/)?.[1]
          ?? options.match(/<option\b[^>]*value="([^"]*)"/)?.[1] ?? '';
      }
      elements.set(id, el);
    }
    return elements.get(id);
  };
  const state = {current: 'room', session: {id: 'room', state: 'open', kind: 'channel'}, agent: 'atlas',
    requireAgentSignature: false, permissionsArmed: false, messages: [], systemEvents: [], generation: 0};
  const serverEvent = {type: 'session.member.add', seq: 10, ts: '2026-10-21T12:00:00Z',
    member_id: 'nimbus-2', member_type: 'agent', context_share: {mode: 'since', boundary_message_id: 'server-edge'},
    text: 'agent nimbus-2 joined · context since · boundary message server-edge'};
  let readEvents = [];
  const context = vm.createContext({$, state, document: {querySelectorAll: () => []},
    stamp: String, author: () => 'atlas', avatar: () => 'A', messageText: m => m.payload?.text || '',
    taskMessageLink: () => '', resetAudienceWarning() {}, Response, AbortSignal, DOMException, setTimeout, clearTimeout,
    sessionPath: (id, suffix) => `/sessions/${id}/${suffix}`,
    refresh: async () => {
      // A join's system row arrives through the transcript read, not through
      // optimistic client prose. Keep its text deliberately unlike the request.
      const response = await context.fetch('/sessions/room/messages');
      const body = await response.json(); state.systemEvents = body.system_events;
      vm.runInContext('renderMessages()', context);
    },
    fetch: async (route, options = {}) => {
      requests.push({route, ...options});
      if (options.method === 'POST') {
        readEvents = [serverEvent];
        return new Response(JSON.stringify({member_id: 'nimbus-2', member_type: 'agent', role: 'member', context_share: serverEvent.context_share}), {status: 201});
      }
      return new Response(JSON.stringify({system_events: readEvents}), {status: 200});
    },
  });
  // The real esc uses document.createElement. A tiny textContent/innerHTML
  // implementation preserves its escaping, rather than substituting a predicate.
  context.document.createElement = () => {
    let text = '';
    return {set textContent(v) { text = String(v); }, get innerHTML() { return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }};
  };
  vm.runInContext(extract('function esc(', 'function author(') +
    extract('async function call(', '// Management reads deliberately') +
    extract('function composerState(', 'function renderReply(') +
    extract('function renderMessages(', 'function loadedTaskLink(') +
    extract('$("add-participant-form").addEventListener', '$("sessions").addEventListener'), context);
  const run = code => vm.runInContext(code, context);
  if (populated) state.messages = [{id: 'm-first', seq: 1, thread_id: 'thread', payload: {text: 'first'}, outcomes: []}];
  $('participant-agent').value = 'nimbus-2';
  run('updateComposer()');
  return {$, state, run, requests, serverEvent,
    submit: () => $('add-participant-form').listeners.submit({preventDefault() {}}),
    posts: () => requests.filter(r => r.method === 'POST'),
    mode: mode => { $('participant-context-share').value = mode; $('participant-context-share').listeners.change?.(); }};
}
const cases = [
  ['empty session: no visible context question', async () => {
    const c = client(false);
    assert.ok(!html.includes('id="late-join-context"') || c.$('late-join-context').hidden);
    await c.submit(); assert.equal(c.posts().length, 1);
  }],
  ['in flight: question is present and visible', async () => {
    const c = client(); assert.ok(/id="late-join-context"/.test(html), 'shipped context question is absent');
    assert.equal(c.$('late-join-context').hidden, false);
    assert.match(html, /How much context to share/);
  }],
  ['summary is selected by default in the shipped select', async () => {
    const c = client(); assert.equal(c.$('participant-context-share').value, 'summary');
    assert.equal(c.$('participant-context-boundary-row').hidden, true);
  }],
  ['first message arriving reveals the prompt; clearing the session hides it', async () => {
    const c = client(false); c.state.messages = [{id: 'new'}]; c.run('updateComposer()');
    assert.ok(/id="late-join-context"/.test(html), 'shipped context question is absent'); assert.equal(c.$('late-join-context').hidden, false);
    c.state.session = null; c.state.messages = []; c.run('updateComposer()');
    assert.equal(c.$('late-join-context').hidden, true);
  }],
  ['since reveals a boundary control; summary hides it', async () => {
    const c = client(); c.mode('since'); assert.ok(/id="participant-context-boundary"/.test(html), 'shipped boundary control is absent');
    assert.equal(c.$('participant-context-boundary-row').hidden, false);
    c.mode('summary'); assert.equal(c.$('participant-context-boundary-row').hidden, true);
  }],
  ['since without boundary is refused in place, never sent or downgraded', async () => {
    const c = client(); c.mode('since'); c.$('participant-context-boundary').value = '   ';
    await c.submit(); assert.equal(c.posts().length, 0);
    assert.match(c.$('participant-result').textContent, /boundary message (id|ID).*required|required.*boundary message (id|ID)/);
    assert.equal(c.$('participant-agent').value, 'nimbus-2'); assert.equal(c.$('participant-context-share').value, 'since');
  }],
  ...['none', 'summary', 'since', 'full'].map(mode => [`request records ${mode} with only its applicable boundary`, async () => {
    const c = client(); c.mode(mode); c.$('participant-context-boundary').value = 'm-first';
    await c.submit(); assert.equal(c.posts().length, 1);
    const body = JSON.parse(c.posts()[0].body); assert.equal(body.context_share, mode);
    assert.equal(body.context_boundary_message_id, mode === 'since' ? 'm-first' : undefined);
  }]),
  ['recorded system line names returned mode and boundary, never client request prose', async () => {
    const c = client(); c.mode('summary'); await c.submit();
    const rendered = c.$('transcript').innerHTML;
    assert.match(rendered, /SYSTEM EVENT/); assert.match(rendered, /nimbus-2 joined/);
    assert.match(rendered, /context since/); assert.match(rendered, /boundary message server-edge/);
    assert.ok(rendered.indexOf('first') < rendered.indexOf('nimbus-2 joined'), 'interleaved by seq');
    assert.ok(c.requests.some(r => r.route.endsWith('/messages')), 'must read the event from the server');
    assert.doesNotMatch(rendered, /state undefined/);
  }],
  ['normal join is informational, not refused or failed', async () => {
    const c = client(); await c.submit();
    assert.match(c.$('participant-result').textContent, /^Added /);
    assert.doesNotMatch(c.$('participant-result').textContent, /refused|failed/i);
    assert.doesNotMatch(c.$('participant-result').className, /error|refused/);
  }],
];
(async () => {
  let failed = 0;
  for (const [name, test] of cases) {
    try { await test(); console.log('PASS ' + name); }
    catch (error) { failed++; console.error('FAIL ' + name + '\n  ' + error.message); }
  }
  console.log(`RESULT ${cases.length - failed}/${cases.length} passed; ${failed} failed (stub DOM/HTTP, shipped functions)`);
  if (failed) process.exitCode = 1;
})();
