// DF-CRIER-447: real browser + stock scratch server; no mocked decision/event.
// node scripts/chat-late-join-browser.cjs <base-url> [screenshot-directory]
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {generateKeyPairSync} = require('node:crypto');
let chromium;
try { ({chromium} = require('playwright')); }
catch (e) { if (e.code !== 'MODULE_NOT_FOUND') throw e; console.error('Playwright unavailable; set NODE_PATH'); process.exit(77); }
const base = process.argv[2], evidence = process.argv[3];
assert.ok(base, 'base URL required');
async function api(method, route, body) {
  const response = await fetch(base + route, {method, headers: {'Content-Type': 'application/json'}, body: body && JSON.stringify(body)});
  const result = await response.json();
  assert.ok(response.ok, `${method} ${route}: ${response.status} ${JSON.stringify(result)}`);
  return result;
}
(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROME_BIN || '/usr/bin/google-chrome', headless: true, args: ['--no-sandbox']});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}});
    const errors = [], posts = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('request', request => { if (request.method() === 'POST' && request.url().endsWith('/participants')) posts.push(request.postDataJSON()); });
    const ids = ['late-join-operator', 'late-join-member', 'late-join-empty-member'];
    for (const id of ids) {
      const {publicKey} = generateKeyPairSync('ed25519');
      await api('POST', '/agents', {id, public_key: publicKey.export({type: 'spki', format: 'der'}).subarray(-32).toString('hex')});
    }
    const room = await api('POST', '/sessions', {title: 'Late-join context evidence', kind: 'channel', created_by: {agent: ids[0]}, members: [{member_type: 'agent', member_id: ids[0]}]});
    await page.goto(base + '/chat');
    const idle = () => page.waitForFunction(() => !state.busy && !state.sending && document.getElementById('refresh-state').textContent.startsWith('Last HTTP refresh'));
    await idle();
    await page.locator('#agent').fill(ids[0]); await page.locator('#apply-identity').click(); await idle();
    await page.locator(`[data-session="${room.id}"]`).click(); await idle();
    assert.equal(await page.locator('#late-join-context').isVisible(), false, 'empty session must not ask');
    await page.locator('#participant-agent').fill(ids[2]); await page.locator('#add-participant').click(); await idle();
    let transcript = await api('GET', `/sessions/${room.id}/messages`);
    const empty = transcript.system_events.find(e => e.member_id === ids[2]);
    assert.equal(empty.context_share.mode, 'none'); assert.equal(empty.context_share.boundary_message_id, undefined);
    assert.match(empty.reason, /nothing to share/);
    const first = await api('POST', `/sessions/${room.id}/messages`, {sender: ids[0], targets: [{kind: 'agent', id: ids[0]}], payload: {text: 'The first recorded message is the context boundary.'}});
    await page.locator('#refresh').click(); await idle();
    assert.equal(await page.locator('#late-join-context').isVisible(), true);
    assert.equal(await page.locator('#participant-context-share').inputValue(), 'summary');
    await page.locator('#participant-context-share').selectOption('since');
    assert.equal(await page.locator('#participant-context-boundary').isVisible(), true);
    await page.locator('#participant-agent').fill(ids[1]);
    const countBefore = posts.length;
    await page.locator('#add-participant').click();
    assert.equal(posts.length, countBefore, 'missing since boundary must not POST');
    assert.match(await page.locator('#participant-result').textContent(), /boundary message ID is required/);
    if (evidence) {
      fs.mkdirSync(evidence, {recursive: true});
      await page.locator('#add-participant-form').locator('..').screenshot({path: path.join(evidence, 'chat-late-join-validation.png')});
    }
    await page.locator('#participant-context-boundary').fill(first.id);
    await page.locator('#add-participant').click(); await idle();
    assert.equal(posts.at(-1).context_share, 'since');
    assert.equal(posts.at(-1).context_boundary_message_id, first.id);
    transcript = await api('GET', `/sessions/${room.id}/messages`);
    const joined = transcript.system_events.find(e => e.member_id === ids[1]);
    assert.equal(joined.context_share.mode, 'since');
    assert.equal(joined.context_share.boundary_message_id, first.id);
    const row = page.locator(`[data-event-seq="${joined.seq}"]`);
    assert.ok((await row.textContent()).includes(joined.text), 'render the actual server event');
    assert.match(await row.textContent(), /context since/); assert.ok((await row.textContent()).includes(first.id));
    assert.doesNotMatch(await page.locator('#participant-result').textContent(), /refused|failed/i);
    assert.deepEqual(errors, []);
    if (evidence) {
      await page.screenshot({path: path.join(evidence, 'chat-late-join.png'), fullPage: true});
      await row.screenshot({path: path.join(evidence, 'chat-late-join-event.png')});
      fs.writeFileSync(path.join(evidence, 'chat-late-join-browser.json'), JSON.stringify({posts, empty, joined, errors}, null, 2) + '\n');
    }
    console.log('LIVE PASS: stock Go API + real Chromium; empty/nonempty prompt, default summary, since validation, request mode/boundary and persisted transcript event');
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
