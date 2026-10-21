#!/usr/bin/env node
// DF-CRIER-454: dependency-free execution of the shipped management table renderer.
// Reproducible RED: --html <historical-chat.html>; fixtures are synthetic, never live credentials.
// Baseline: git show 52ad78492c1ae7d4f28841868b09d68f351c9b36:cmd/server/chat.html > /tmp/df-crier-454-pre-change.html
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
if (process.argv.length !== 2 && !(process.argv.length === 4 && process.argv[2] === '--html')) {
  console.error('Usage: node scripts/chat-authority-truth.cjs [--html <chat.html>]');
  process.exit(2);
}
const source = process.argv[3] || path.join(__dirname, '../cmd/server/chat.html');
const html = fs.readFileSync(source, 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
class Element {
  constructor() { this.children = []; this._text = ''; this._html = ''; }
  set textContent(value) { this._text = String(value); this._html = undefined; this.children = []; }
  get innerHTML() { return this._html ?? this._text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  set innerHTML(value) { this._html = String(value); this._text = ''; this.children = []; }
  append(...nodes) { this.children.push(...nodes); }
  setAttribute() {}
}
function render(rows, columns = [], route = 'GET /principals') {
  const panel = new Element();
  const context = vm.createContext({document: {createElement: () => new Element()}, $: () => panel,
    // Only DOM/state plumbing is simulated. Classification and table HTML are shipped code.
    managementState: () => { panel.children = []; }, rows, columns, route});
  vm.runInContext(extract('function esc(', 'function author(') +
    extract('const managementValue =', '// DF-CRIER-384: the Authority view') +
    'managementRows("fixture", route, rows, columns, "No records");', context);
  const markup = panel.children.map(node => node.innerHTML).join('');
  const text = markup.replace(/<[^>]*>/g, '').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
  const cells = [...markup.matchAll(/<td>(.*?)<\/td>/gs)].map(m => m[1].replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&'));
  return {markup, text, cells};
}
const hex = '0123456789abcdef'.repeat(4);
const blob = 'Ab3_defGHI7jklMN9opQR2stuVW4xyz56ab';
const prefixed = [
  ['sk-', 'sk-' + 'fixtureOnly0123456789'],
  ['ghp_', 'ghp_' + 'fixtureOnly0123456789'],
  ['xox', 'xoxb-' + 'fixtureOnly0123456789'],
  ['AKIA', 'AKIA' + '0123456789ABCDEF'],
  ['eyJ', ['eyJ', 'hbGciOiJub25lIn0', '.eyJ', 'zdWIiOiJmaXh0dXJlIn0', '.Zml4dHVyZQ'].join('')],
  ['PRIVATE KEY', ['-----BEGIN', 'PRIVATE KEY-----'].join(' ') + '\nfixture-only\n' + ['-----END', 'PRIVATE KEY-----'].join(' ')],
];
const cases = [];
function test(name, run) { cases.push([name, run]); }
function withheld(result, secret, why) {
  assert.ok(!result.markup.includes(secret), 'LEAK: fixture value appears in shipped table: ' + result.text);
  assert.match(result.text, /withheld/i, result.text);
  assert.match(result.text, why, result.text);
}
for (const [prefix, value] of prefixed) {
  test(`display_name: ${prefix} material is withheld with its reason`, () => {
    const r = render([{display_name: value}]);
    withheld(r, value, prefix === 'PRIVATE KEY' ? /private key/i : /looks like key material.*prefix/i);
    assert.ok(r.text.includes(prefix), 'reason identifies the known shape, not the secret');
  });
}
for (const [name, value] of [['hex', hex], ['base64url', blob], ['base64', blob.replace(/_/g, '/') + '==']]) {
  test(`non-token name: long ${name} blob is withheld`, () => withheld(render([{display_name: value}]), value, /looks like key material.*(hex|base64)/i));
}
for (const field of ['apikey', 'auth', 'secret_ref', 'credential', 'token', 'password', 'tokenValue', 'API_KEY']) {
  test(`${field}: short opaque credential is withheld by first-pass signal`, () => withheld(render([{[field]: 'fixture-only-private'}]), 'fixture-only-private', /credential|secret|token|auth/i));
}
for (const field of ['fingerprint', 'digest', 'sha256', 'token_fingerprint']) {
  test(`${field}: published 64-hex fingerprint remains complete and labelled`, () => {
    const r = render([{[field]: hex}]);
    assert.ok(r.cells.some(cell => cell.includes(hex)), r.text);
    assert.match(r.text, /published fingerprint/i, r.text);
    assert.doesNotMatch(r.text, /withheld/i);
  });
}
test('existing short token fingerprint remains visible and labelled as a prefix', () => {
  const r = render([{token_fingerprint: '1a2b3c4d'}]);
  assert.ok(r.cells.some(cell => cell.includes('1a2b3c4d')));
  assert.match(r.text, /short prefix/i);
});
test('fingerprint name cannot exempt prefix-style key material', () => withheld(render([{fingerprint: prefixed[0][1]}]), prefixed[0][1], /prefix/i));
test('explicit secret flag overrides the published-fingerprint exception', () => withheld(render([{secret: true, fingerprint: hex}]), hex, /marked secret/i));
test('record secret flag protects an innocent field with a short value', () => withheld(render([{secret: true, display_name: 'fixture-only-private'}]), 'fixture-only-private', /marked secret/i));
for (const value of [
  {secret: true, value: 'fixture-only-private'},
  {sensitive: true, value: 'fixture-only-private'},
  {type: 'apikey', value: 'fixture-only-private'},
  {kind: 'credential', value: 'fixture-only-private'},
  {scheme: 'bearer', value: 'fixture-only-private'},
  {auth: {value: 'fixture-only-private'}},
  {apikey: {value: 'fixture-only-private'}},
  [{payload: {type: 'api_key', value: 'fixture-only-private'}}],
]) {
  test(`neutral field: secret/auth object shape ${JSON.stringify(value)} is withheld`, () => withheld(render([{metadata: value}]), 'fixture-only-private', /marked secret|credential|auth/i));
}
test('credential-shaped record protects value independent of its field name', () => withheld(render([{type: 'apikey', value: 'fixture-only-private'}]), 'fixture-only-private', /credential/i));
test('nested array key material is not JSON-serialized into a cell', () => withheld(render([{metadata: [{name: prefixed[1][1]}]}]), prefixed[1][1], /prefix/i));
test('key material embedded in descriptive prose is withheld', () => withheld(render([{display_name: 'pasted: ' + prefixed[0][1] + ' (oops)'}]), prefixed[0][1], /prefix/i));
test('absent, empty and withheld have three explicit distinct renderings', () => {
  const r = render([{}, {display_name: ''}, {display_name: prefixed[0][1]}], ['display_name']);
  assert.equal(r.cells.length, 3);
  assert.match(r.cells[0], /not reported|absent/i);
  assert.match(r.cells[1], /empty/i);
  assert.match(r.cells[2], /withheld.*prefix/i);
  assert.equal(new Set(r.cells).size, 3);
  console.log('  STATES: ' + r.cells.join(' | '));
});
test('null remains distinct from absent and empty', () => {
  const r = render([{}, {display_name: ''}, {display_name: null}], ['display_name']);
  assert.match(r.cells[2], /null.*no value/i);
  assert.equal(new Set(r.cells).size, 3);
});
test('ordinary values, empty collections, booleans and secret:false are preserved', () => {
  const r = render([{display_name: 'Operator <example>', secret: false, enabled: true, count: 2, scopes: [], metadata: {label: 'read only'}}]);
  assert.ok(r.markup.includes('Operator &lt;example&gt;'));
  assert.ok(r.cells.includes('false')); assert.ok(r.cells.includes('true')); assert.ok(r.cells.includes('2'));
  assert.ok(r.cells.includes('[]')); assert.ok(r.cells.includes('{"label":"read only"}'));
  assert.doesNotMatch(r.text, /withheld/i);
});
test('blob boundary: 31 characters stay visible; 32 are withheld', () => {
  const below = 'a1b2c3d4'.repeat(4).slice(0, 31);
  assert.ok(render([{description: below}]).cells.includes(below));
  const at = below + 'e';
  withheld(render([{description: at}]), at, /long hex blob/i);
});
test('token hash has no fingerprint publication exemption', () => withheld(render([{token_hash: hex}]), hex, /long hex blob/i));
test('uppercase published fingerprint is not normalized or shortened', () => {
  const value = hex.toUpperCase(), r = render([{fingerprint: value}]);
  assert.ok(r.cells.includes(value + ' (published fingerprint)'));
});
test('nested secret:false is not a secret marker', () => {
  const r = render([{metadata: {secret: false, label: 'public'}}]);
  assert.ok(r.cells.includes('{"secret":false,"label":"public"}'));
});
test('credential columns distinguish absent, empty, null and withheld', () => {
  const r = render([{}, {apikey: ''}, {apikey: null}, {apikey: 'fixture-only-private'}], ['apikey']);
  assert.match(r.cells[0], /not reported/i); assert.match(r.cells[1], /empty/i);
  assert.match(r.cells[2], /null/i); assert.match(r.cells[3], /withheld/i);
  assert.equal(new Set(r.cells).size, 4);
});
test('ordinary long prose is not silently shortened', () => {
  const text = 'A readable operator description with spaces. '.repeat(12);
  assert.ok(render([{display_name: text}]).cells.includes(text));
});
for (const route of ['/principals', '/bindings', '/grants', '/fed/peers', '/agents/fixture/class', '/trust/anchors', '/trust/verify']) {
  test(`${route}: shared shipped renderer withholds secret values`, () => withheld(render([{display_name: prefixed[0][1]}], [], route), prefixed[0][1], /prefix/i));
}
let passed = 0, failed = 0;
console.log('Authority value truth — shipped renderer: ' + source);
for (const [name, run] of cases) {
  try { run(); passed++; console.log('PASS ' + name); }
  catch (error) { failed++; console.error('FAIL ' + name + ': ' + error.message); }
}
console.log(`Authority value truth: ${passed} passed; ${failed} failed; ${cases.length} total`);
console.log('LIMITS: synthetic records; shipped JS table markup in a DOM shim, not a live browser/API or an exhaustive secret detector.');
process.exitCode = failed ? 1 : 0;
