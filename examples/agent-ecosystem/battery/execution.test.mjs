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

const credentialFor = { "pi-agent": "DEEPSEEK_API_KEY", opencode: "DEEPSEEK_API_KEY",
  "claude-code": "ANTHROPIC_API_KEY", codex: "OPENAI_API_KEY", aider: "DEEPSEEK_API_KEY", goose: "OPENAI_API_KEY" };

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

async function startConsumer(t, { key = "fixture-only", command, harness = "opencode", sdk, liveFlag = "1", budget = "250" } = {}) {
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
  const env = { ...process.env, PATH: join(dir, "bin"), AGENT_ID: harness, HARNESS: harness,
    RUNTIME_TIMEOUT_MS: budget, PORT: String(port), CRIER_URL: `http://127.0.0.1:${registry.address().port}`,
    OPENCODE_LIVE: liveFlag, CLAUDE_CODE_LIVE: liveFlag, CODEX_LIVE: liveFlag, AIDER_LIVE: liveFlag, GOOSE_LIVE: liveFlag };
  for (const name of ["DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "CODEX_API_KEY"]) delete env[name];
  // Leave an unrelated key present to catch the old global DeepSeek gate.
  if (["codex", "claude-code", "goose"].includes(harness) && !key?.trim()) env.DEEPSEEK_API_KEY = "unrelated-fixture";
  if (key !== null) env[credentialFor[harness]] = key;
  const child = spawn(process.execPath, [join(dir, "consumer.mjs")], {
    env, cwd: dir, stdio: ["ignore", "pipe", "pipe"],
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
    const started = performance.now();
    const response = await fetch(`${url}/hook`, { method: "POST", body: JSON.stringify({ payload: { text, battery_nonce: requestNonce } }) });
    const body = await response.json();
    return { status: response.status, body, elapsed: performance.now() - started };
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
  assert.equal(body.status, "OK");
  assert.equal(body.reply, `héllo\n${marker}`);
  assert.equal(body.nonce, nonce);
  assert.equal(body.exec, join(dir, "bin/opencode"));
  assert.equal(body.stdout_bytes, Buffer.byteLength(`héllo\n${marker}\n`));
  assert.equal(check({ reply: body }).status, 0);
});

for (const [name, command] of [["missing executable", undefined], ["nonzero CLI exit", "printf 'partial output'; exit 7"], ["empty CLI stdout", "exit 0"]]) {
  test(`consumer fails closed on ${name}`, async (t) => {
    const { status, body } = await startConsumer(t, { command });
    assert.equal(status, 200);
    assert.equal(body.status, "FAILED");
    assert.equal(Object.hasOwn(body, "reply"), false);
    assert.equal(body.live, false);
    assert.equal(typeof body.error, "string");
    assert.equal(check(body, String(status)).status, 1);
  });
}

test("pi SDK execution carries the same marker after prompt succeeds", async (t) => {
  const { status, body } = await startConsumer(t, { harness: "pi-agent", budget: "1500", sdk: `
    export const ModelRuntime = { async create() { return { async getAvailable() {
      return [{ provider: "anthropic", id: "wrong-provider" }, { provider: "deepseek", id: "deepseek-flash" }];
    } }; } };
    export const SessionManager = { inMemory() { return {}; } };
    export async function createAgentSession(options) {
      if (options.model.provider !== "deepseek" || !options.modelRuntime) throw new Error("wrong credential provider");
      if (process.env.DEEPSEEK_API_KEY !== "fixture-only") throw new Error("missing SDK credential");
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
  assert.equal(status, 200);
  assert.equal(body.status, "FAILED");
  assert.equal(Object.hasOwn(body, "reply"), false);
  assert.equal(typeof body.error, "string");
});

test("whole battery probes every harness independently of guard key; prints refusal reasons; copies evidence even on failure", async (t) => {
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
        if mode == 'no-key':
            harness = url.split('/agents/')[1].split('/')[0]
            variable = {'codex': 'OPENAI_API_KEY', 'claude-code': 'ANTHROPIC_API_KEY', 'goose': 'OPENAI_API_KEY'}.get(harness, 'DEEPSEEK_API_KEY')
            result.update(live=False, status='REFUSED', reason='missing ' + variable)
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
  for (const [name, key, fixture, expected] of [["no-key", "", canned, "5 pass / 6 fail / 1 skip"],
    ["canned-with-key", "fixture-only", canned, "7 pass / 6 fail / 0 skip"],
    ["live-with-key", "fixture-only", live, "13 pass / 0 fail / 0 skip"],
    ["live-without-guard-key", "", live, "11 pass / 0 fail / 1 skip"],
    ["replayed", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["missing", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["same-text", "fixture-only", live, "7 pass / 6 fail / 0 skip"],
    ["failed-runtime", "", { status: "FAILED", live: false, runtime: "fixture-runtime", exit_code: 3, reason: "fixture-runtime: exit code 3; stderr: boom-347" }, "5 pass / 6 fail / 1 skip"],
    ["copy-failure", "", canned, "5 pass / 6 fail / 1 skip"]]) {
    const evidence = join(dir, `${name}.jsonl`);
    const mirror = name === "copy-failure" ? join(dir, "not-a-directory", "ecosystem.jsonl") : join(dir, `repo/${name}.jsonl`);
    const run = spawnSync("bash", [join(here, "battery.sh")], { encoding: "utf8", env: {
      ...process.env, PATH: `${dir}:${process.env.PATH}`, DEEPSEEK_API_KEY: key,
      EVIDENCE: evidence, EVIDENCE_REPO: mirror, HARNESS_BODY: JSON.stringify(fixture), HARNESS_MODE: name, STATS_FILE: join(dir, `${name}.stats`),
    } });
    assert.equal(run.status, !["live-with-key", "live-without-guard-key"].includes(name) ? 1 : 0, run.stdout + run.stderr);
    assert.ok(run.stdout.includes(expected), run.stdout + run.stderr);
    console.log(`fixture ${name}: ${expected}; exit ${run.status}`);
    assert.doesNotMatch(run.stdout, /^SKIP  round-trip /m);
    if (name === "no-key") {
      for (const [harness, variable] of Object.entries(credentialFor)) {
        const line = run.stdout.split("\n").find((s) => s.startsWith(`FAIL  round-trip ${harness} `));
        assert.match(line, new RegExp(`REFUSED: missing ${variable}`));
        console.log(line);
      }
    }
    if (name === "failed-runtime") {
      for (const harness of Object.keys(credentialFor)) {
        const line = run.stdout.split("\n").find((s) => s.startsWith(`FAIL  round-trip ${harness} `));
        assert.match(line, /FAILED: fixture-runtime: exit code 3; stderr: boom-347/);
      }
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
    assert.equal(harnessRows.length, 12);
    if (name === "failed-runtime") assert.ok(harnessRows.every(row => /FAILED:.*boom-347/.test(row.reason)));
    {
      assert.equal(new Set(harnessRows.map((r) => r.nonce)).size, 12, "unique nonce for EVERY request");
      assert.equal(new Set(harnessRows.map((r) => r.run_nonce)).size, 1);
      assert.ok(harnessRows.every((r) => r.run_started_at && r.request_started_at));
      if (["live-with-key", "live-without-guard-key"].includes(name)) {
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

for (const [harness, variable] of Object.entries(credentialFor)) {
  for (const key of [null, "", "   "]) {
    test(`${harness} refuses ${variable} unset/empty/whitespace (${JSON.stringify(key)}) before runtime`, async (t) => {
      const { body, dir, elapsed } = await startConsumer(t, { harness, key,
        command: "printf attempted > runtime-attempted; printf 'should never execute'" });
      assert.equal(body.live, false);
      assert.equal(body.status, "REFUSED");
      assert.equal(body.reason, `missing ${variable}`);
      assert.ok(elapsed < 1000, `${elapsed}ms exceeds immediate-refusal budget`);
      await assert.rejects(readFile(join(dir, "runtime-attempted")), { code: "ENOENT" });
      assert.match(check({ reply: body }).stdout, new RegExp(`REFUSED: missing ${variable}`));
      console.log(`${harness}: REFUSED: ${body.reason} (${elapsed.toFixed(1)}ms)`);
    });
  }
  if (harness === "pi-agent") continue; // SDK has its own fixture above.
  test(`${harness} executes with its own credential, without unrelated keys`, async (t) => {
    const extra = harness === "codex" ? '[ "$CODEX_API_KEY" = "$OPENAI_API_KEY" ] || exit 10;' :
      harness === "goose" ? '[ "$GOOSE_PROVIDER" = openai ] && [ "$GOOSE_MODEL" = gpt-4o-mini ] || exit 11;' : "";
    const { body } = await startConsumer(t, { harness,
      command: `[ "$${variable}" = fixture-only ] || exit 9; ${extra} printf 'credential-attempt-ok\\n${marker}\\n'` });
    assert.equal(body.live, true, JSON.stringify(body));
    assert.equal(body.reply, `credential-attempt-ok\n${marker}`);
  });
}

test("checker carries consumer refusal/error text even on HTTP failures", () => {
  for (const code of ["200", "500", "502"]) {
    const refusal = { live: false, status: "REFUSED", reason: "missing OPENAI_API_KEY" };
    assert.match(check({ reply: refusal }, code).stdout, /REFUSED: missing OPENAI_API_KEY/);
    assert.match(check({ error: "upstream refused credential" }, code).stdout, /upstream refused credential/);
    assert.equal(check({ reply: refusal }, code).status, 1);
  }
});

test("compose forwards only each harness credential, with optional empty defaults", async () => {
  const compose = resolve(here, "../docker-compose.yml");
  const source = await readFile(compose, "utf8");
  const variables = ["DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY"];
  const clean = { PATH: process.env.PATH, HOME: process.env.HOME };
  for (const populated of [false, true]) {
    const env = { ...clean };
    if (populated) for (const variable of variables) env[variable] = `fixture-${variable}`;
    const config = spawnSync("docker", ["compose", "--env-file", "/dev/null", "-f", compose, "config", "--format", "json"], { env, encoding: "utf8" });
    assert.equal(config.status, 0, config.stderr);
    const services = JSON.parse(config.stdout).services;
    for (const [harness, variable] of Object.entries(credentialFor)) {
      const received = services[harness].environment;
      assert.equal(received.RUNTIME_TIMEOUT_MS, "20000");
      assert.equal(received[variable], populated ? `fixture-${variable}` : "", `${harness}: ${variable}`);
      assert.deepEqual(Object.keys(received).filter((key) => variables.includes(key)), [variable], harness);
    }
  }
  for (const line of source.split("\n").filter((line) => /^ +[A-Z_]+_API_KEY:/.test(line))) {
    const variable = line.trim().split(":")[0];
    assert.equal(line.trim(), `${variable}: \${${variable}:-}`);
  }
});

// DF-CRIER-346: exercise the actual HTTP consumer, never the runner alone.
for (const harness of ["opencode", "claude-code", "codex", "aider", "goose"]) {
  test(`${harness} reports exit 3 and stderr before delivery deadline`, async (t) => {
    const { status, body, elapsed } = await startConsumer(t, { harness,
      command: "printf 'partial-out'; printf 'boom-347' >&2; exit 3" });
    assert.equal(status, 200);
    assert.equal(body.status, "FAILED");
    assert.equal(body.runtime, harness);
    assert.equal(body.exit_code, 3);
    assert.equal(body.timed_out, false);
    assert.equal(body.live, false);
    assert.equal(body.stderr, "boom-347");
    assert.equal(body.stdout, "partial-out");
    assert.match(body.reason, /exit code 3.*boom-347/s);
    assert.match(check({ reply: body }).stdout, /FAILED:.*boom-347/);
    assert.ok(elapsed < 2250, `${elapsed}ms exceeds budget + 2s`);
    console.log(`${harness}: ${body.reason} (${elapsed.toFixed(1)}ms)`);
  });
  test(`${harness} kills a silent runtime and descendants at its own budget`, async (t) => {
    // Ignore TERM to prove that timeout does not wait for a cooperative CLI.
    const script = 'process.on("SIGTERM", () => {}); setTimeout(() => {}, 10000);';
    const { status, body, elapsed } = await startConsumer(t, { harness,
      command: `trap '' TERM; ${process.execPath} -e '${script}' & wait` });
    assert.equal(status, 200);
    assert.equal(body.status, "FAILED");
    assert.equal(body.runtime, harness);
    assert.equal(body.timed_out, true);
    assert.equal(body.timeout_ms, 250);
    assert.equal(body.live, false);
    assert.equal(body.stdout, "");
    assert.match(body.reason, /timeout.*250ms/);
    assert.ok(elapsed >= 200 && elapsed < 2250, `${elapsed}ms outside runtime budget`);
    console.log(`${harness}: ${body.reason} (${elapsed.toFixed(1)}ms)`);
  });
  test(`${harness} returns OK only for successful nonempty stdout`, async (t) => {
    const { body } = await startConsumer(t, { harness, command: "printf 'known-runtime-line'" });
    assert.equal(body.status, "OK");
    assert.equal(body.live, true);
    assert.equal(body.reply, "known-runtime-line");
  });
}

test("empty successful runtime reports exit 0, not OK", async (t) => {
  const { body } = await startConsumer(t, { command: "printf 'stderr-only' >&2" });
  assert.equal(body.status, "FAILED");
  assert.equal(body.exit_code, 0);
  assert.match(body.reason, /no stdout.*stderr-only/s);
});

test("diagnostics retain only the first 2 KiB of each stream", async (t) => {
  const script = 'process.stdout.write("o".repeat(5000)); process.stderr.write("boom-347" + "e".repeat(5000)); process.exitCode=3;';
  const { body } = await startConsumer(t, { command: `exec ${process.execPath} -e '${script}'` });
  assert.equal(body.status, "FAILED");
  assert.equal(Buffer.byteLength(body.stdout), 2048);
  assert.equal(Buffer.byteLength(body.stderr), 2048);
  assert.ok(body.stderr.startsWith("boom-347"));
});

for (const [name, sdk, pattern] of [
  ["import error", 'throw new Error("pi-import-boom");', /pi-import-boom/],
  ["wedged import", 'await new Promise(() => { setInterval(() => {}, 1000); });', /timeout.*250ms/],
]) {
  test(`pi SDK ${name} is bounded and self-explaining`, async (t) => {
    const { body, elapsed } = await startConsumer(t, { harness: "pi-agent", sdk });
    assert.equal(body.status, "FAILED");
    assert.equal(body.runtime, "pi-agent");
    assert.match(body.reason, pattern);
    assert.ok(elapsed < 2250, `${elapsed}ms exceeds budget + 2s`);
  });
}

for (const budget of ["0", "NaN", "30000", "-1"]) {
  test(`invalid runtime budget ${budget} fails before spawning`, async (t) => {
    const { body, dir } = await startConsumer(t, { budget,
      command: "printf attempted > runtime-attempted; printf unexpected" });
    assert.equal(body.status, "FAILED");
    assert.match(body.reason, /RUNTIME_TIMEOUT_MS.*1.*25000/);
    await assert.rejects(readFile(join(dir, "runtime-attempted")), { code: "ENOENT" });
  });
}

test("checker refuses FAILED even with forged successful execution markers", () => {
  const result = check({ ...live, status: "FAILED", reason: "exit code 3: boom-347" });
  assert.equal(result.status, 1);
  assert.match(result.stdout, /FAILED: exit code 3: boom-347/);
});

const piFixture = (behavior) => `
  export const ModelRuntime = { async create() { return { async getAvailable() {
    return [{ provider: "deepseek", id: "deepseek-flash" }];
  } }; } };
  export const SessionManager = { inMemory() { return {}; } };
  export async function createAgentSession() {
    let listener;
    return { session: { subscribe(fn) { listener = fn; }, async prompt() { ${behavior} }, async dispose() {} } };
  }`;

for (const [name, behavior, pattern] of [
  ["nonthrowing SDK error", 'listener({ type: "message_end", message: { role: "assistant", errorMessage: "DeepSeek rejected credential fixture" } });', /DeepSeek rejected credential fixture/],
  ["empty SDK answer", '', /exit code 0 but no stdout/],
  ["wedged prompt", 'await new Promise(() => { setInterval(() => {}, 1000); });', /timeout.*250ms/],
]) {
  test(`pi ${name} returns FAILED with reason`, async (t) => {
    const { body, elapsed } = await startConsumer(t, { harness: "pi-agent", sdk: piFixture(behavior) });
    assert.equal(body.status, "FAILED");
    assert.equal(body.runtime, "pi-agent");
    assert.equal(body.live, false);
    assert.match(body.reason, pattern);
    assert.ok(elapsed < 2250, `${elapsed}ms exceeds budget + 2s`);
  });
}

test("Pi incompatible SDK model catalog explains its precise blocker", async (t) => {
  const sdk = piFixture('').replace('id: "deepseek-flash"', 'id: "unsupported-model"');
  const { body } = await startConsumer(t, { harness: "pi-agent", sdk });
  assert.equal(body.status, "FAILED");
  assert.match(body.reason, /SDK has no deepseek\/deepseek-flash.*rebuild the Pi image/);
});

test("Goose failures name the OpenAI versus DeepSeek wiring blocker", async (t) => {
  const { body } = await startConsumer(t, { harness: "goose", command: "printf 'goose-provider-error' >&2; exit 3" });
  assert.match(body.reason, /configured openai\/gpt-4o-mini requires OPENAI_API_KEY; DeepSeek is not wired for Goose/);
  assert.match(body.reason, /goose-provider-error/);
});
