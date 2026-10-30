#!/usr/bin/env node
// DF-CRIER-489: exercise shipped /chat group loading against controlled races.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const htmlPath = process.argv[2] || path.join(__dirname, '..', 'cmd/server/chat.html');
const html = fs.readFileSync(htmlPath, 'utf8');
const match = html.match(/async function loadGroups\([\s\S]*?\n\}/);
if (!match) throw new Error('could not extract shipped loadGroups()');
const refreshMatch = html.match(/async function refresh\(\) \{[\s\S]*?\n\}/);
if (!refreshMatch) throw new Error('could not extract shipped refresh()');
const makeHarness = () => {
  const state = { generation: 0, namespace: '', groups: [] };
  const pending = [];
  const panels = new Map();
  const call = route => {
    assert.equal(route, '/groups', 'unexpected route');
    return new Promise((resolve, reject) => pending.push({ resolve, reject }));
  };
  const renderGroups = body => {
    const groups = Array.isArray(body?.groups) ? body.groups : [];
    state.groups = groups;
    const realm = state.namespace ? ' in realm ' + state.namespace : '';
    $('groups-count').textContent = groups.length
      ? (groups.length === 1 ? '1 named group' : groups.length + ' named groups') + realm
      : 'No named groups' + realm + '.';
  };
  const unavailable = (id, error) => { panels.set(id, error.message); };
  const $ = id => ({
    set textContent(value) { panels.set(id + ':text', value); },
    set innerHTML(value) { panels.set(id + ':html', value); },
  });
  const loadGroups = new Function('state', 'call', 'renderGroups', 'unavailable', '$',
    `${match[0]}; return loadGroups;`)(state, call, renderGroups, unavailable, $);
  return { state, pending, panels, loadGroups };
};

async function run() {
  // Slower realm-A success must not overwrite the already-rendered realm-B roster.
  {
    const h = makeHarness();
    h.state.generation = 1; h.state.namespace = 'realm-A';
    const a = h.loadGroups(1, 'realm-A');
    h.state.generation = 2; h.state.namespace = 'realm-B';
    const b = h.loadGroups(2, 'realm-B');
    h.pending[1].resolve({ groups: [{ name: 'only-in-B' }] });
    await b;
    h.pending[0].resolve({ groups: [{ name: 'only-in-A' }] });
    await a;
    assert.deepEqual(h.state.groups.map(g => g.name), ['only-in-B'],
      'stale overwrite: slower realm-A success replaced realm-B groups');
  }

  // A stale request's failure must not clear the successful current roster.
  {
    const h = makeHarness();
    h.state.generation = 10; h.state.namespace = 'realm-A';
    const a = h.loadGroups(10, 'realm-A');
    h.state.generation = 11; h.state.namespace = 'realm-B';
    const b = h.loadGroups(11, 'realm-B');
    h.pending[1].resolve({ groups: [{ name: 'only-in-B' }] });
    await b;
    h.pending[0].reject(new Error('stale realm-A failure'));
    await a;
    assert.deepEqual(h.state.groups.map(g => g.name), ['only-in-B'],
      'stale failure cleared the current realm-B roster');
    assert.equal(h.panels.get('groups-count:text'), '1 named group in realm realm-B',
      'stale failure replaced current realm-B status');
  }

  // An active failure remains visible and clears the stale roster as before.
  {
    const h = makeHarness();
    h.state.generation = 20; h.state.namespace = 'realm-B';
    h.state.groups = [{ name: 'previous-roster' }];
    const active = h.loadGroups(20, 'realm-B');
    const error = new Error('active realm-B failure');
    h.pending[0].reject(error);
    await active;
    assert.deepEqual(h.state.groups, [], 'active failure must clear the stale roster');
    assert.equal(h.panels.get('groups-count:text'), 'Named groups unavailable.',
      'active failure must remain visible');
    assert.equal(h.panels.get('groups'), error.message,
      'active failure detail must remain visible');
  }
  assert.match(refreshMatch[0], /loadGroups\(generation,\s*realm\)/,
    'refresh must pass its captured generation and realm to loadGroups');
  console.log('PASS: stale success ignored; stale failure ignored; active-B failure visible.');
}
run().catch(error => { console.error(error); process.exitCode = 1; });
