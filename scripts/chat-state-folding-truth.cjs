#!/usr/bin/env node
// DF-CRIER-473: execute the shipped session and permission renderers.
// Use --html with a historical page to reproduce the pre-fix folds.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-state-folding-truth.cjs [--html FILE]');
const source = args[1] || path.join(__dirname, '../cmd/server/chat.html');
const html = fs.readFileSync(source, 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
class Element {
  constructor() { this.textContent = ''; this.innerHTML = ''; this.className = ''; }
}
const elements = new Map();
const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
const document = {createElement: () => ({textContent: '', get innerHTML() { return this.textContent.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }})};
const context = vm.createContext({$, document, Object, Array, updateComposer() {}});
vm.runInContext(
  html.match(/^const state = .*;$/m)[0] + '\n' +
  extract('function esc(', 'function author(') +
  extract('function renderHeader()', 'function renderReply(') +
  extract('function renderPermissions(', 'function renderTasks(') +
  'state.current = null; state.session = null; state.sessionRead = "not-selected";',
  context,
);
function sessionBadge(sessionRead, session) {
  vm.runInContext(`state.sessionRead = ${JSON.stringify(sessionRead)}; state.session = ${JSON.stringify(session)}; renderHeader();`, context);
  return $('session-state').textContent;
}
function permissionText(allowed) {
  const row = {member_type: 'agent', member_id: 'fixture-member', role: 'fixture-role'};
  if (allowed !== 'absent') row.allowed = allowed;
  vm.runInContext(`renderPermissions(${JSON.stringify({actions: ['send', 'read', 'invite', 'admin'], rows: [row]})});`, context);
  return $('permissions').innerHTML.replace(/<[^>]*>/g, '');
}
const badgeFixtures = [
  ['absent fixture / no session selected', 'not-selected', null],
  ['empty-object fixture / selected read pending', 'pending', {}],
  ['false fixture / failed read', 'failed', null],
  ['refused read', 'refused', null],
];
const badges = badgeFixtures.map(([name, readState, session]) => [name, sessionBadge(readState, session)]);
const permissionFixtures = [
  ['absent fixture', 'absent'],
  ['empty-object fixture', {}],
  ['false fixture', {send: false}],
];
const permissions = permissionFixtures.map(([name, allowed]) => [name, permissionText(allowed)]);
if (args.length) {
  assert.equal(badges[1][1], 'Unavailable', 'pre-fix selected read fold should reproduce Unavailable');
  assert.match(permissions[1][1], /send: denied/, 'pre-fix empty-object permission fold should reproduce denied');
  console.log(`RED REPRODUCED: session=${badges[1][1]}; permissions=${permissions[1][1]}`);
} else {
  const expectedBadges = ['No session', 'Read not attempted', 'Read failed', 'Read refused'];
  badges.forEach(([name, actual], index) => assert.equal(actual, expectedBadges[index], `${name}: ${actual}`));
  assert.equal(new Set(badges.slice(0, 3).map(([, value]) => value)).size, 3, 'absent, pending and failed badges must be distinct');
  assert.match(permissions[0][1], /send: not reported/);
  assert.match(permissions[1][1], /send: not reported/);
  assert.match(permissions[2][1], /send: denied/);
  assert.match(permissions[2][1], /read: not reported/);
  assert.equal((permissions[0][1].match(/denied/g) || []).length, 0, 'absent allowed map must not become denied');
  assert.equal((permissions[1][1].match(/denied/g) || []).length, 0, 'empty allowed map must not become denied');
  assert.equal((permissions[2][1].match(/denied/g) || []).length, 1, 'only evaluated false action is denied');
  assert.equal(permissions[0][1], permissions[1][1], 'missing and empty allowed maps share the not reported rendering');
  assert.equal(new Set([badges[0][1], badges[1][1], permissions[2][1]]).size, 3, 'absent, pending and false fixtures have three distinct renderings');
  console.log(`PASS: chat state folding (${source}); badges=${badges.map(([name, value]) => `${name}:${value}`).join(' | ')}; permissions=${permissions.map(([name, value]) => `${name}:${value}`).join(' | ')}`);
}
