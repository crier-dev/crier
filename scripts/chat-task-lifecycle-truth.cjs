#!/usr/bin/env node
// DF-CRIER-491: execute the shipped task message formatter and both consumers.
// The optional --html path makes the baseline RED reproducible after the fix.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-task-lifecycle-truth.cjs [--html <chat.html>]');
const html = fs.readFileSync(args[1] || path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
const start = html.indexOf('function esc(');
const end = html.indexOf('function stamp(', start);
assert.ok(start >= 0 && end > start, 'missing shipped message formatter region');

const elements = new Map();
function element() {
  return {innerHTML:'', textContent:'', dataset:{}, querySelectorAll:() => [],
    get classList() { return {add() {}}; }};
}
const $ = id => { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); };
const context = vm.createContext({$, document:{createElement:() => ({textContent:'', get innerHTML() { return this.textContent; }})}});
vm.runInContext(html.slice(start, end), context);

const lifecycle = (state, actor, payload = {}) => ({
  id: 'message-' + state, seq: state === 'claimed' ? 2 : state === 'done' ? 3 : 1,
  message_kind: 'task', task: {task_id:'task-491', state}, author:{agent:actor}, payload,
});
for (const [state, expected, payload] of [['open','Create a useful artifact',{text:'Create a useful artifact'}], ['claimed','Task claimed by claimant-agent'], ['done','Task completed by completer-agent']]) {
  const message = lifecycle(state, state === 'open' ? 'creator-agent' : state === 'claimed' ? 'claimant-agent' : 'completer-agent', payload);
  const text = vm.runInContext(`messageText(${JSON.stringify(message)})`, context);
  assert.equal(text, expected, `${state} lifecycle must be human-readable`);
  assert.notEqual(text, '{}', `${state} lifecycle must not render bare {}`);
  const transcript = $('transcript');
  transcript.innerHTML = '<div class="m-body">' + text + '</div>';
  assert.doesNotMatch(transcript.innerHTML, /(^|>)\s*\{\}\s*(<|$)/, `${state} transcript must not contain bare {}`);
  const rail = $('branch-rail');
  rail.innerHTML = '<a>' + text.slice(0, 48) + '</a>';
  assert.doesNotMatch(rail.innerHTML, /(^|>)\s*\{\}\s*(<|$)/, `${state} branch rail must not contain bare {}`);
}
console.log('PASS: task lifecycle transcript and branch rail render named human actions; no bare {}');
