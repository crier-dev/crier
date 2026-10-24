#!/usr/bin/env node
// DF-CRIER-474: execute the shipped transcript renderer for thread filtering.
// RED: node scripts/chat-thread-filter-truth.cjs --html /tmp/df-crier-474-before.html
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-thread-filter-truth.cjs [--html <path>]');
const html = fs.readFileSync(args[1] || path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
function client(thread = '') {
  const elements = new Map();
  const $ = id => {
    if (!elements.has(id)) elements.set(id, {innerHTML:'',scrollHeight:100,scrollTop:0,clientHeight:100});
    return elements.get(id);
  };
  const state = {
    messages: [{id:'in-thread', seq:1, thread_id:'thread-a', thread_depth:0,
      payload:{text:'message in selected thread'}, message_kind:'plain', outcomes:[]}],
    systemEvents: [{seq:2, text:'session lifecycle event', actor:'system', ts:'2026-10-24T12:00:00Z'}],
    thread,
    expandedBranches: new Set(), locationMessage: null,
  };
  const context = vm.createContext({
    $, state, console, Map, Set,
    esc: value => String(value).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'),
    author: () => 'system', stamp: String, messageText: m => m.payload?.text || '',
    messageAnchorID: id => 'message-' + id, avatar: () => 'S', anchorLink: () => '',
    taskMessageLink: () => '', negativeOutcome: () => false, outcomeLine: () => '',
    collapsedThreadGroup: () => null, renderBranchRail() {},
  });
  vm.runInContext(extract('function renderMessages(', '// D11:'), context);
  return {$, state, render: () => vm.runInContext('renderMessages()', context)};
}
const cases = [
  ['unfiltered view keeps lifecycle event visible without a thread marker', () => {
    const c = client(); c.render();
    assert.match(c.$('transcript').innerHTML, /message in selected thread/);
    assert.match(c.$('transcript').innerHTML, /session lifecycle event/);
    assert.doesNotMatch(c.$('transcript').innerHTML, /SESSION-WIDE/);
  }],
  ['selected thread marks a session-wide event instead of presenting it as thread content', () => {
    const c = client('thread-a'); c.render();
    const rendered = c.$('transcript').innerHTML;
    assert.match(rendered, /message in selected thread/);
    assert.match(rendered, /session lifecycle event/);
    assert.match(rendered, /SESSION-WIDE/);
    assert.match(rendered, /data-session-wide="true"/);
  }],
];
let failed = 0;
for (const [name, test] of cases) {
  try { test(); console.log('PASS ' + name); }
  catch (error) { failed++; console.error('FAIL ' + name + '\n  ' + error.message); }
}
console.log(`RESULT ${cases.length - failed}/${cases.length} passed (stub DOM, shipped renderMessages)`);
if (failed) process.exitCode = 1;
