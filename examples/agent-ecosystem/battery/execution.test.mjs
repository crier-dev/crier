// Offline regression battery: fake CLIs/SDK and transport, never real credentials.
// Run: node --test examples/agent-ecosystem/battery/execution.test.mjs
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, mkdir, readFile, writeFile, copyFile, rm } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const here = fileURLToPath(new URL(".", import.meta.url));
const consumer = resolve(here, "../consumer/consumer.mjs");
const checker = join(here, "check-harness.py");
const nonce = "4bb57a79-dd26-45be-a3ed-bf78d52e7b68";
const marker = `[crier-battery-nonce:${nonce}]`;
const runtimeText = `executed\n${marker}`;
const live = { reply: runtimeText, live: true, exec: "/fixture/opencode", stdout_bytes: Buffer.byteLength(runtimeText),
  nonce, stdout_sha256: createHash("sha256").update(runtimeText).digest("hex") };
const canned = { reply: "canned", live: false, canned: true };

function check(body, code = "200", requestNonce = nonce, previousDigest = "") {
  return spawnSync("python3", [checker, code, requestNonce, previousDigest], { input: JSON.stringify(body), encoding: "utf8" });
}

test("execution assertion rejects canned, errored, missing and invalid markers", () => {
  for (const body of [canned, { reply: canned }, { reply: "(opencode error: missing executable)" },
    { ...live, error: "auth failed" }, { ...live, canned: true }, { ...live, live: "true" },
    { ...live, exec: "" }, { ...live, stdout_bytes: 0 }, { ...live, stdout_bytes: true },
    { ...live, reply: "" }, { reply: { ...live, error: "auth failed" } }]) {
    assert.equal(check(body).status, 1, JSON.stringify(body));
  }
  assert.match(check(canned).stdout, /live=false/);
  assert.equal(check(live, "500").status, 1);
  assert.equal(check({ error: "delivery failed", reply: live }).status, 1);
  assert.equal(spawnSync("python3", [checker, "200", nonce], { input: "not JSON" }).status, 1);
  assert.equal(check(live).status, 0);
  assert.equal(check({ reply: live }).status, 0, "crier generic/raw reply envelope");
});

async function startConsumer(t, { key = "fixture-only", command, harness = "opencode", sdk, liveFlag = "1" } = {}) {
  const dir = await mkdtemp(join(tmpdir(), "crier-execution-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await copyFile(consumer, join(dir, "consumer.mjs"));
  await copyFile(resolve(here, "../registration-payloads.json"), join(dir, "registration-payloads.json"));
  await mkdir(join(dir, "bin"));
  if (command !== undefined) await writeFile(join(dir, `bin/${harness === "claude-code" ? "claude" : harness}`), `#!/bin/sh\n${command}\n`, { mode: 0o755 });
  if (sdk) {
    const pkg = join(dir, "node_modules/@earendil-works/pi-coding-agent");
    await mkdir(pkg, { recursive: true });
    await writeFile(join(pkg, "package.json"), JSON.stringify({ type: "module", exports: "./index.mjs" }));
    await writeFile(join(pkg, "index.mjs"), sdk);
  }
  const registry = createServer((req, res) => { req.resume(); res.writeHead(200); res.end("{}"); });
  registry.listen(0, "127.0.0.1");
  await once(registry, "listening");
  t.after(() => new Promise((done) => registry.close(done)));
  const reserve = createServer();
  reserve.listen(0, "127.0.0.1");
  await once(reserve, "listening");
  const port = reserve.address().port;
  await new Promise((done) => reserve.close(done));
  const child = spawn(process.execPath, [join(dir, "consumer.mjs")], {
    env: { ...process.env, PATH: join(dir, "bin"), AGENT_ID: harness, HARNESS: harness,
      PORT: String(port), CRIER_URL: `http://127.0.0.1:${registry.address().port}`,
      OPENCODE_LIVE: liveFlag, CLAUDE_CODE_LIVE: liveFlag, CODEX_LIVE: liveFlag, AIDER_LIVE: liveFlag, GOOSE_LIVE: liveFlag, DEEPSEEK_API_KEY: key }, stdio: ["ignore", "pipe", "pipe"],
  });
  let log = "";
  child.stdout.on("data", (b) => { log += b; });
  child.stderr.on("data", (b) => { log += b; });
  t.after(async () => { if (child.exitCode === null) { child.kill(); await once(child, "exit"); } });
  const url = `http://127.0.0.1:${port}`;
  let ready = false;
  for (let i = 0; i < 100; i++) {
    if (child.exitCode !== null) break;
    try { if ((await fetch(`${url}/health`)).ok) { ready = true; break; } } catch {}
    await new Promise((done) => setTimeout(done, 20));
  }
  assert.ok(ready, log);
  const hook = async (text = "hello", requestNonce = nonce) => {
    const response = await fetch(`${url}/hook`, { method: "POST", body: JSON.stringify({ payload: { text, battery_nonce: requestNonce } }) });
    return { status: response.status, body: await response.json() };
  };
  return { ...await hook(), dir, hook };
}

test("consumer labels canned replies and never claims execution", async (t) => {
  const { status, body } = await startConsumer(t, { key: "" });
  assert.equal(status, 200);
  assert.equal(body.live, false);
  assert.equal(body.canned, true);
  assert.ok(JSON.stringify(body).includes('"reply":"'), "old substring check accepted this actual canned consumer");
  assert.equal(check(body).status, 1);
  console.log(`FAIL  actual canned consumer (HTTP ${status}; ${check(body).stdout.trim()}); old reply substring accepted it`);
});

test("key without live opt-in still fails the execution assertion", async (t) => {
  const { status, body } = await startConsumer(t, { liveFlag: "" });
  assert.equal(status, 200);
  assert.equal(body.live, false);
  assert.equal(body.canned, true);
  assert.equal(check(body).status, 1);
});

test("consumer marks successful CLI execution with resolved path and UTF-8 byte count", async (t) => {
  const { status, body, dir } = await startConsumer(t, { command: `printf 'héllo\\n${marker}\\n'` });
  assert.equal(status, 200);
  assert.equal(body.reply, `héllo\n${marker}`);
  assert.equal(body.nonce, nonce);
  assert.equal(body.exec, join(dir, "bin/opencode"));
  assert.equal(body.stdout_bytes, Buffer.byteLength(`héllo\n${marker}\n`));
  assert.equal(check({ reply: body }).status, 0);
});

for (const [name, command] of [["missing executable", undefined], ["nonzero CLI exit", "printf 'partial output'; exit 7"], ["empty CLI stdout", "exit 0"]]) {
  test(`consumer fails closed on ${name}`, async (t) => {
    const { status, body } = await startConsumer(t, { command });
    assert.ok(status >= 400);
    assert.equal(Object.hasOwn(body, "reply"), false);
    assert.equal(body.live, false);
    assert.equal(typeof body.error, "string");
    assert.equal(check(body, String(status)).status, 1);
  });
}

test("pi SDK execution carries the same marker after prompt succeeds", async (t) => {
  const { status, body } = await startConsumer(t, { harness: "pi-agent", sdk: `
    export const AuthStorage = { create() { return {}; } };
    export const ModelRegistry = { create() { return {}; } };
    export const SessionManager = { inMemory() { return {}; } };
    export async function createAgentSession() {
      let listener;
      return { session: { subscribe(fn) { listener = fn; },
        async prompt(text) { listener({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "SDK executed\\n" + text.match(/\\[crier-battery-nonce:[0-9a-f-]+\\]/)[0] } }); },
        async dispose() {} } };
    }` });
  assert.equal(status, 200);
  assert.equal(body.exec, "sdk:@earendil-works/pi-coding-agent");
  assert.equal(body.stdout_bytes, Buffer.byteLength(`SDK executed\n${marker}`));
  assert.equal(check(body).status, 0);
});

test("pi SDK import failure is an error, not a reply", async (t) => {
  const { status, body } = await startConsumer(t, { harness: "pi-agent" });
  assert.ok(status >= 400);
  assert.equal(Object.hasOwn(body, "reply"), false);
  assert.equal(typeof body.error, "string");
});

test("whole battery skips six harnesses without key; rejects canned with key; copies evidence even on failure", async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "crier-battery-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, "sleep"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  // Transport fixture only. The battery's real parser, counters and evidence run unchanged.
  await writeFile(join(dir, "curl"), `#!/usr/bin/env python3
import hashlib, json, os, sys
args = sys.argv[1:]
url = next(a for a in args if a.startswith('http'))
code, body = 200, {}
if url.endswith('/health'): body = {'status': 'ok'}
elif url.endswith('/stats'):
    path = os.environ['STATS_FILE']
    try: n = int(open(path).read())
    except FileNotFoundError: n = 0
    with open(path, 'w') as f: f.write(str(n + 1))
    body = {'deliveries': n}
elif url.endswith('/inbox'):
    data = args[args.index('-d') + 1]
    if '/guarded/' in url:
        code = 403 if 'Ignore all previous' in data else 201
        body = {'error': 'GUARD_BLOCKED'} if code == 403 else {}
    elif '/sink/' in url:
        code = 202 if 'fire and forget' in data else 200
        body = {'reply': 'ECHO: no text'}
    else:
        result = json.loads(os.environ['HARNESS_BODY'])
        mode = os.environ['HARNESS_MODE']
        if result.get('live'):
            payload = json.loads(data)['payload']
            nonce = payload['battery_nonce']
            if mode == 'replayed': nonce = 'abc81347-807b-4b10-aace-9ce6030d233f'
            text = 'one canned answer' if mode == 'same-text' else 'fixture runtime: ' + payload['text']
            text += '\\n[crier-battery-nonce:' + nonce + ']'
            result.update(reply=text, nonce=nonce, stdout_bytes=len(text.encode()), stdout_sha256=hashlib.sha256(text.encode()).hexdigest())
            if mode == 'missing': del result['nonce']
        body = {'reply': result}
if '-o' not in args: print(json.dumps(body, separators=(',', ':')), end='')
if '-w' in args: print(args[args.index('-w') + 1].replace('%{http_code}', str(code)).replace('\\\\n', '\\n'), end='')
`, { mode: 0o755 });
  await writeFile(join(dir, "not-a-directory"), "copy must fail loudly");
  for (const [name, key, fixture, expected] of [["no-key", "", canned, "5 pass / 0 fail / 7 skip"],
    ["canned-with-key", "fixture-only", canned, "7 pass / 6 fail / 0 skip"],
    ["live-with-key", "fixture-only", live, "13 pass / 0 fail / 0 skip"],
    ["replayed", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["missing", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["same-text", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["copy-failure", "", canned, "5 pass / 0 fail / 7 skip"]]) {
    const evidence = join(dir, `${name}.jsonl`);
    const mirror = name === "copy-failure" ? join(dir, "not-a-directory", "ecosystem.jsonl") : join(dir, `repo/${name}.jsonl`);
    const run = spawnSync("bash", [join(here, "battery.sh")], { encoding: "utf8", env: {
      ...process.env, PATH: `${dir}:${process.env.PATH}`, DEEPSEEK_API_KEY: key,
      EVIDENCE: evidence, EVIDENCE_REPO: mirror, HARNESS_BODY: JSON.stringify(fixture), HARNESS_MODE: name, STATS_FILE: join(dir, `${name}.stats`),
    } });
    assert.equal(run.status, !["no-key", "live-with-key"].includes(name) ? 1 : 0, run.stdout + run.stderr);
    assert.ok(run.stdout.includes(expected), run.stdout + run.stderr);
    console.log(`fixture ${name}: ${expected}; exit ${run.status}`);
    if (!key) {
      assert.equal((run.stdout.match(/^SKIP  round-trip /gm) || []).length, 6);
      assert.doesNotMatch(run.stdout, /^PASS  round-trip (pi-agent|opencode|claude-code|codex|aider|goose) /m);
    }
    if (name === "canned-with-key") {
      const line = run.stdout.split("\n").find((s) => s.startsWith("FAIL  round-trip opencode"));
      assert.match(line, /live=false/);
      console.log(line);
    }
    const original = await readFile(evidence, "utf8");
    if (name === "copy-failure") assert.match(run.stderr, /FAIL  evidence copy/);
    else assert.equal(await readFile(mirror, "utf8"), original);
    const rows = original.trim().split("\n").map(JSON.parse);
    const harnessRows = rows.filter((r) => r.test?.startsWith("round-trip ") && r.test !== "round-trip sink echo via crier");
    assert.equal(harnessRows.length, key ? 12 : 6);
    if (!key) assert.ok(harnessRows.every((r) => r.result === "SKIP" && r.nonce_matched === null));
    else {
      assert.equal(new Set(harnessRows.map((r) => r.nonce)).size, 12, "unique nonce for EVERY request");
      assert.equal(new Set(harnessRows.map((r) => r.run_nonce)).size, 1);
      assert.ok(harnessRows.every((r) => r.run_started_at && r.request_started_at));
      if (name === "live-with-key") {
        assert.ok(harnessRows.every((r) => r.result === "PASS" && r.nonce_matched === true && r.nonce_echo === r.nonce));
        for (let i = 0; i < harnessRows.length; i += 2) {
          assert.notEqual(harnessRows[i].prompt, harnessRows[i + 1].prompt);
          assert.equal(harnessRows[i + 1].previous_content_sha256, harnessRows[i].content_sha256);
          assert.notEqual(harnessRows[i].content_sha256, harnessRows[i + 1].content_sha256);
        }
        if (process.env.DF_CRIER_338_EVIDENCE) await writeFile(process.env.DF_CRIER_338_EVIDENCE,
          harnessRows.map((r) => JSON.stringify({ ...r, fixture_only: true, source: "execution.test.mjs transport fixture; NOT vendor runtime evidence" })).join("\n") + "\n");
      } else if (name === "same-text") {
        assert.equal(harnessRows.filter((r) => r.result === "FAIL" && /same runtime stdout digest/.test(r.reason)).length, 6);
      } else assert.ok(harnessRows.every((r) => r.result === "FAIL"));
    }
  }
});

for (const harness of ["opencode", "claude-code", "codex", "aider", "goose"]) {
  test(`${harness} challenge reaches CLI output without truncation`, async (t) => {
    // This fixture executable emits the argv it actually received, through stdout.
    const { body, hook } = await startConsumer(t, { harness, command: `printf '%s\\n' "$@"` });
    assert.equal(body.nonce, nonce);
    assert.equal(check(body).status, 0);
    const nextNonce = "abc81347-807b-4b10-aace-9ce6030d233f";
    const next = await hook("x".repeat(2000), nextNonce);
    assert.equal(next.body.nonce, nextNonce, "challenge survives question truncation");
    assert.ok(next.body.reply.includes(`[crier-battery-nonce:${nextNonce}]`));
  });
}

test("consumer never copies request nonce into constant runtime output", async (t) => {
  const { body } = await startConsumer(t, { command: "printf 'one canned answer'" });
  assert.equal(body.live, true, "a CLI did exit successfully");
  assert.equal(body.nonce, null, "request metadata alone is not runtime proof");
  assert.equal(check(body).status, 1);
  assert.match(check(body).stdout, /nonce/);
});

test("same canned answer with fresh runtime markers still fails the paired digest check", async (t) => {
  const script = 'const text = process.argv.join(" "); console.log("one canned answer\\n" + text.match(/\\[crier-battery-nonce:[0-9a-f-]+\\]/)[0]);';
  const { body, hook } = await startConsumer(t, { command: `exec ${process.execPath} -e '${script}' -- "$@"` });
  const first = check(body);
  assert.equal(first.status, 0, first.stdout);
  const nextNonce = "abc81347-807b-4b10-aace-9ce6030d233f";
  const next = await hook("an entirely different question", nextNonce);
  assert.equal(next.body.nonce, nextNonce);
  assert.notEqual(next.body.stdout_sha256, body.stdout_sha256, "fresh marker alone changes raw digest");
  const second = check(next.body, "200", nextNonce, JSON.parse(first.stdout).content_sha256);
  assert.equal(second.status, 1, second.stdout);
  assert.match(second.stdout, /same runtime stdout digest/);
});

test("replayed CLI stdout retains old nonce and fails this request", async (t) => {
  const oldNonce = "abc81347-807b-4b10-aace-9ce6030d233f";
  const { body } = await startConsumer(t, { command: `printf 'cached answer\\n[crier-battery-nonce:${oldNonce}]'` });
  assert.equal(body.nonce, oldNonce);
  assert.equal(check(body).status, 1);
});
