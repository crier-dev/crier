#!/usr/bin/env node
// DF-CRIER-365: execute the real harness, then prove its rejection paths.
// Same NODE_PATH, CRIER_TEST_BINARY and CHROME_BIN as chat-ui-browser.cjs.
// Mutations live ONLY in temporary copies; neither source nor server is edited.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const root = fs.mkdtempSync(path.join(os.tmpdir(),'crier-browser-selftest-'));
const source = fs.readFileSync(path.join(__dirname,'chat-ui-browser.cjs'),'utf8');
const env = {...process.env};
delete env.CHAT_MATRIX_ONLY; // A partial run cannot satisfy the full-suite control.
delete env.CHAT_TEST_PORT; // Each control gets private scratch listeners.
const results = [];
function run(name, script, extraEnv = {}) {
  const result = spawnSync(process.execPath,[script],{
    env:{...env,CHAT_EVIDENCE:path.join(root,name+'-evidence'),...extraEnv},
    encoding:'utf8',timeout:180000,maxBuffer:8*1024*1024,
  });
  fs.writeFileSync(path.join(root,name+'.stdout'),result.stdout || '');
  fs.writeFileSync(path.join(root,name+'.stderr'),result.stderr || '');
  if (result.error) throw result.error;
  assert.equal(result.signal,null,name+' must settle, not be killed');
  const row = {name,rc:result.status,stdout:path.join(root,name+'.stdout'),stderr:path.join(root,name+'.stderr')};
  results.push(row);
  console.log(name+': rc '+result.status+'; stdout '+row.stdout+'; stderr '+row.stderr);
  return result;
}
function mutated(name, before, after) {
  assert.equal(source.split(before).length,2,'mutation anchor must occur exactly once: '+name);
  const script = path.join(root,name+'.cjs');
  fs.writeFileSync(script,source.replace(before,after));
  return script;
}
try {
  // Copies require the same test helper but never rewrite the worktree.
  fs.copyFileSync(path.join(__dirname,'chat-capability-matrix.cjs'),path.join(root,'chat-capability-matrix.cjs'));
  fs.copyFileSync(path.join(__dirname,'chat-reporting.cjs'),path.join(root,'chat-reporting.cjs'));
  const script = path.join(__dirname,'chat-ui-browser.cjs');
  const clean = run('clean',script);
  if (clean.status === 77) {
    console.error(clean.stderr);
    console.error('SKIP: browser selftest prerequisites unavailable; exit code 77; no regression proof claimed.');
    process.exitCode = 77;
  } else {
    assert.equal(clean.status,0,'clean harness failed: '+clean.stderr);
    assert.match(clean.stdout,/PASS: full browser suite/);
    const report = JSON.parse(fs.readFileSync(path.join(root,'clean-evidence','browser-results.json'),'utf8'));
    assert.deepEqual(report.completion.transitions.map(t => t.state),['open','claimed','done']);
    assert.equal(report.completion.transitions[1].owner,'ui-test-recipient');
    assert.equal(report.completion.transitions[2].owner,'ui-test-recipient');
    assert.ok(fs.existsSync(new URL(report.completion.evidenceArtifact)),'completion evidence artifact exists');
    const evidence = run('missing-evidence',mutated('missing-evidence',"await page.locator('[data-task-evidence]').fill(taskEvidence.text);","await page.locator('[data-task-evidence]').fill(''); taskEvidence.text = '';"));
    assert.equal(evidence.status,1,'empty evidence regression must FAIL, not skip or pass');
    assert.match(evidence.stderr,/UI Complete POST .*\/complete: .*TASK_EVIDENCE_REQUIRED/);
    console.log('REJECTION VERIFIED: UI Complete input/button POST /sessions/:session/tasks/:task/complete: TASK_EVIDENCE_REQUIRED (real server 409, harness rc 1)');
    const revision = run('wrong-revision',mutated('wrong-revision',"'CR-CHAT-038','/chat UI revision must be CR-CHAT-038'","'SEEDED_WRONG_REVISION','/chat UI revision must be CR-CHAT-038'"));
    assert.equal(revision.status,1,'wrong DOM expectation must FAIL');
    assert.match(revision.stderr,/\/chat UI revision must be CR-CHAT-038/);
    console.log('REJECTION VERIFIED: /chat UI revision must be CR-CHAT-038 (DOM assertion, harness rc 1)');
    const skip = run('browser-unavailable',script,{CHROME_BIN:path.join(root,'no-browser')});
    assert.equal(skip.status,77,'missing browser must explicitly SKIP, never PASS');
    assert.match(skip.stderr,/SKIP: Chromium executable unavailable.*exit code 77/);
    assert.doesNotMatch(skip.stdout,/VERIFIED|PASS:/,'skip ran no browser/API assertions');
    console.log('SKIP VERIFIED: missing Chromium; harness rc 77; no browser assertions ran');
    console.log('PASS: clean browser suite + two seeded regression rejections + explicit browser skip. Temporary mutations only; worktree unchanged. Evidence: '+root);
  }
} catch (error) {
  console.error('FAIL: browser harness selftest; exit code 1');
  console.error(error);
  process.exitCode=1;
} finally {
  fs.writeFileSync(path.join(root,'selftest-results.json'),JSON.stringify({results,evidence:root},null,2));
}
