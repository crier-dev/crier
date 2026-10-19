// Shared agent-ecosystem consumer — receives messages through crier
// (blocking webhook) and answers with a real harness runtime when the
// harness's live-mode opt-in and provider credential are set.
//
// Parameterized by env (set per service in docker-compose.yml / Dockerfile):
//   AGENT_ID         registry id + in-cluster webhook host (default "agent")
//   HARNESS          display name + runtime dispatch key (default AGENT_ID)
//   PORT             in-container HTTP port (default 9100)
//   CRIER_URL        crier base URL (default http://crier:8767)
//   Provider key    DEEPSEEK_API_KEY / OPENAI_API_KEY / ANTHROPIC_API_KEY
//   <HARNESS>_LIVE   per-harness opt-in flag: "1" = run the real CLI
//
// Harnesses: pi-agent (SDK session — live whenever a key is present),
// opencode, claude-code, codex, aider, goose (CLI — live only with
// <HARNESS>_LIVE=1 AND a key; the CLIs' model bootstrap is heavy and can
// exceed the blocking-delivery budget, so canned keeps the battery
// deterministic on any host).
import { spawn } from "node:child_process";
import { accessSync, constants, readFileSync } from "node:fs";
import { delimiter, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "node:http";
import { createHash } from "node:crypto";

const REGISTRATION_TEMPLATE = JSON.parse(
  readFileSync(new URL("./registration-payloads.json", import.meta.url), "utf8")
).consumer;

const AGENT_ID = process.env.AGENT_ID || "agent";
const HARNESS = process.env.HARNESS || AGENT_ID;
const PORT = Number(process.env.PORT || "9100");
const CRIER = process.env.CRIER_URL || "http://crier:8767";
// These are the providers selected by the runtime dispatch below, not a global
// DeepSeek gate. Empty (including Compose's ${VAR:-}) is never a credential.
const KEY_ENV = {
  "pi-agent": "DEEPSEEK_API_KEY",
  opencode: "DEEPSEEK_API_KEY",
  "claude-code": "ANTHROPIC_API_KEY",
  codex: "OPENAI_API_KEY",
  aider: "DEEPSEEK_API_KEY",
  goose: "OPENAI_API_KEY",
}[HARNESS];
const HAS_KEY = KEY_ENV && !!process.env[KEY_ENV]?.trim();

// Per-harness live-mode env name (used in the canned reply + the live gate).
const LIVE_ENV = {
  "pi-agent": "DEEPSEEK_API_KEY",
  opencode: "OPENCODE_LIVE",
  "claude-code": "CLAUDE_CODE_LIVE",
  codex: "CODEX_LIVE",
  aider: "AIDER_LIVE",
  goose: "GOOSE_LIVE",
}[HARNESS] || `${HARNESS.toUpperCase().replace(/-/g, "_")}_LIVE`;
const OPTED_IN = HARNESS === "pi-agent" || process.env[LIVE_ENV] === "1";
const LIVE = OPTED_IN && HAS_KEY;

let registered = false;
let lastStatus = null;
let lastBody = "";

async function register() {
  const pub = [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, "0")).join("");
  // `POST /agents` decodes the `webhook` object STRICTLY — an unknown key is a
  // 400 at registration, not a silent drop (d97b777, DF-CRIER-150). Accepted
  // keys are exactly url, auth_type, auth_value_ref, schema_template,
  // custom_schema, delivery_mode, batch, retries, timeout_ms — so a
  // webhook-level `response_map` is refused. Reply extraction lives on
  // `custom_schema.response_map`; `schema_template: "generic"` needs none (the
  // template's own default, `raw`, returns the response body).
  // The checked-in payload is the contract source shared with the push-time Go
  // gate. Clone it before filling per-container runtime values.
  const body = structuredClone(REGISTRATION_TEMPLATE);
  body.id = AGENT_ID;
  body.public_key = pub;
  body.webhook.url = `http://${AGENT_ID}:${PORT}/hook`;
  const r = await fetch(`${CRIER}/agents`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  lastStatus = r.status;
  lastBody = (await r.text()).slice(0, 300);
  registered = r.ok || r.status === 409;
  if (registered) {
    console.log(`${AGENT_ID} registered: ${r.status}`);
  } else {
    // A rejected registration must be impossible to miss: the response body
    // names the exact field the server refused.
    console.log(`${AGENT_ID} registration REJECTED: HTTP ${r.status} — ${lastBody}`);
  }
}

// A live marker is emitted only AFTER a successful runtime call with output.
// Pi uses an SDK rather than a CLI; stdout_bytes then counts its emitted text.
function liveReply(stdout, command) {
  if (!stdout.trim()) throw new Error(`${HARNESS} produced no output`);
  const reply = stdout.trim();
  // Extract only from the completed runtime's output, NEVER from the request.
  const echoes = [...reply.matchAll(/\[crier-battery-nonce:([0-9a-f-]{36})\]/g)];
  return { reply, live: true, exec: command, status: "OK", runtime: HARNESS, stdout_bytes: Buffer.byteLength(stdout),
    stdout_sha256: createHash("sha256").update(reply).digest("hex"),
    nonce: echoes.length === 1 ? echoes[0][1] : null };
}

function resolveCommand(bin) {
  for (const dir of (process.env.PATH || "").split(delimiter)) {
    const command = resolve(dir, bin);
    try {
      accessSync(command, constants.X_OK);
      return command;
    } catch { /* Try the next PATH entry, just like execFile. */ }
  }
  throw new Error(`${HARNESS} executable not found: ${bin}`);
}

// Keep runtime failures in the generic/raw HTTP 200 envelope so crier does not
// replace diagnostics with a webhook transport error. FAILED never earns live=true.
function failed(reason, detail = {}) {
  const result = { status: "FAILED", runtime: HARNESS, live: false, error: reason, reason, ...detail };
  console.error(JSON.stringify(result));
  return result;
}

function runtimeBudget() {
  const ms = Number(process.env.RUNTIME_TIMEOUT_MS || "20000");
  if (!Number.isInteger(ms) || ms < 1 || ms > 25000) {
    throw new Error("RUNTIME_TIMEOUT_MS must be an integer from 1 to 25000 (delivery budget is 30000ms)");
  }
  return ms;
}

function runCli(bin, args, extraEnv = {}, runtime = undefined) {
  let command, budget;
  try {
    budget = runtimeBudget();
    command = runtime ? bin : resolveCommand(bin);
  } catch (err) {
    return Promise.resolve(failed(err.message, { exit_code: null, timed_out: false, stdout: "", stderr: "" }));
  }
  return new Promise((resolve) => {
    // Separate process group lets a hard deadline also kill CLI descendants.
    // Pi imports/prompts/disposes in this same killable boundary, not an
    // uncancellable Promise.race in the HTTP process.
    const child = spawn(command, args, { env: { ...process.env, ...extraEnv },
      detached: process.platform !== "win32", stdio: ["ignore", "pipe", "pipe"] });
    let stdout = Buffer.alloc(0), stderr = Buffer.alloc(0), settled = false;
    const kill = () => {
      try {
        if (process.platform === "win32") child.kill("SIGKILL");
        else process.kill(-child.pid, "SIGKILL");
      } catch { /* Already exited, or spawn failed. */ }
    };
    const finish = (cause, code = null, signal = null, timedOut = false) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      kill();
      // Do not wait for inherited pipe handles after the deadline.
      child.stdout.destroy();
      child.stderr.destroy();
      const out = stdout.toString("utf8"), err = stderr.subarray(0, 2048).toString("utf8");
      if (!cause && code === 0 && out.trim()) {
        resolve(liveReply(out, runtime || command));
        return;
      }
      const excerpt = stdout.subarray(0, 2048).toString("utf8");
      let reason = `${HARNESS}: ${cause || (code === 0 ? "exit code 0 but no stdout" : `exit code ${code ?? "unknown"}${signal ? ` (${signal})` : ""}`)}`;
      if (HARNESS === "goose") reason += "; configured openai/gpt-4o-mini requires OPENAI_API_KEY; DeepSeek is not wired for Goose";
      reason += `; stderr: ${err || "(empty)"}; stdout: ${excerpt || "(empty)"}`;
      const result = failed(reason, { exec: runtime || command, exit_code: code, signal,
        timed_out: timedOut, timeout_ms: budget, stdout: excerpt, stderr: err });
      resolve(result);
    };
    const timer = setTimeout(() => finish(`runtime timeout after ${budget}ms`, null, "SIGKILL", true), budget);
    child.stdout.on("data", (chunk) => {
      // Retain normal answers up to 1 MiB, but keep failure excerpts small.
      stdout = Buffer.concat([stdout, chunk]).subarray(0, 1024 * 1024 + 1);
      if (stdout.length > 1024 * 1024) finish("stdout exceeded 1 MiB capture limit");
    });
    child.stderr.on("data", (chunk) => { stderr = Buffer.concat([stderr, chunk]).subarray(0, 2048); });
    child.once("error", (err) => finish(`could not start runtime: ${err.message}`));
    child.once("close", (code, signal) => finish(null, code, signal));
  });
}

async function piPrompt(prompt) {
  const { createAgentSession, SessionManager, ModelRuntime } =
    await import("@earendil-works/pi-coding-agent");
  const modelRuntime = await ModelRuntime.create();
  // Verified against the installed image: DeepSeek exposes deepseek-flash and
  // deepseek-v4-pro, not the OpenCode provider/model spelling.
  const model = (await modelRuntime.getAvailable()).find((candidate) =>
    candidate.provider === "deepseek" && candidate.id === "deepseek-flash");
  if (!model) throw new Error("pi-agent: SDK has no deepseek/deepseek-flash model for DEEPSEEK_API_KEY; rebuild the Pi image with a compatible SDK");
  const { session } = await createAgentSession({
    sessionManager: SessionManager.inMemory(), modelRuntime, model,
  });
  const out = [];
  let runtimeError;
  try {
    session.subscribe((ev) => {
      if (ev.type === "message_update" && ev.assistantMessageEvent?.type === "text_delta") {
        out.push(ev.assistantMessageEvent.delta);
      }
      // The SDK can complete a prompt with an error message instead of throwing.
      if (ev.type === "message_end" && ev.message?.role === "assistant" && ev.message.errorMessage) {
        runtimeError = ev.message.errorMessage;
      }
    });
    await session.prompt(`Answer in one sentence, no tools: ${prompt}`);
    if (runtimeError) throw new Error(runtimeError);
    return out.join("");
  } finally {
    await session.dispose();
  }
}

async function answer(text, nonce) {
  // Refuse before resolving/importing/starting any runtime. Keep HTTP 200 so
  // crier's generic webhook envelope preserves the consumer's diagnostic.
  if (KEY_ENV && !HAS_KEY) {
    const reason = `missing ${KEY_ENV}`;
    return { reply: `${HARNESS}(REFUSED: ${reason})`, live: false, canned: true, status: "REFUSED", reason };
  }
  if (!OPTED_IN) {
    const reason = `requires ${LIVE_ENV}=1`;
    return { reply: `${HARNESS}(canned, ${reason}): received "${text.slice(0, 80)}"`, live: false, canned: true, reason };
  }
  // Put the challenge AFTER the bounded question; no runner may truncate it.
  // Payload (not a custom HTTP header) survives crier's generic webhook mapping.
  if (nonce !== undefined && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(nonce)) {
    throw new Error("invalid battery nonce");
  }
  const prompt = nonce === undefined ? text : `${text.slice(0, 350)}\nAnswer the question, then echo exactly this marker on a separate line: [crier-battery-nonce:${nonce}]`;
  if (HARNESS === "pi-agent") {
    return runCli(process.execPath, [fileURLToPath(import.meta.url), "--pi-runtime",
      nonce === undefined ? text.slice(0, 500) : prompt], {}, "sdk:@earendil-works/pi-coding-agent");
  }
  switch (HARNESS) {
    case "opencode":
      return runCli("opencode", ["run", `Answer in one sentence, no tools. Question: ${nonce === undefined ? text.slice(0, 400) : prompt}`, "--model", "deepseek/deepseek-v4-flash"]);
    case "claude-code":
      return runCli("claude", ["-p", prompt, "--output-format", "text"]);
    case "codex":
      // Noninteractive Codex reads CODEX_API_KEY; keep the public stack
      // credential OPENAI_API_KEY and pass the alias only in the child env.
      return runCli("codex", ["exec", "--skip-git-repo-check", prompt], { CODEX_API_KEY: process.env.OPENAI_API_KEY });
    case "aider":
      return runCli("aider", ["--model", "deepseek/deepseek-v4-flash", "--message", prompt, "--no-auto-commits", "--yes-always"]);
    case "goose":
      return runCli("goose", ["run", "--no-session", "--no-profile", "--quiet", "--text", prompt], { GOOSE_PROVIDER: "openai", GOOSE_MODEL: "gpt-4o-mini" });
    default:
      throw new Error(`${HARNESS}: no live runner configured`);
  }
}

async function ensureRegistered() {
  // Live check: if crier lost us (restart/recreate), re-register.
  const r = await fetch(`${CRIER}/agents/${AGENT_ID}`, { method: "GET" });
  if (r.ok) return true;
  await register();
  return registered;
}

if (process.argv[2] === "--pi-runtime") {
  try {
    process.stdout.write(await piPrompt(process.argv[3]));
  } catch (err) {
    console.error(err.stack || err.message);
    process.exitCode = 1;
  }
} else createServer(async (req, res) => {
  const send = (code, obj) => {
    const b = JSON.stringify(obj);
    res.writeHead(code, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(b) });
    res.end(b);
  };
  if (req.method === "GET" && req.url === "/ready") {
    // 200 ONLY once registered; until then 503 naming the last registration
    // attempt, so an unregistered agent cannot masquerade as ready
    // (INT-CI-007). The process keeps serving either way — never exit 0
    // pretending to be ready.
    const ok = await ensureRegistered();
    if (ok) return send(200, { ready: true, registered: true });
    return send(503, { ready: false, registered: false, last_status: lastStatus, last_body: lastBody });
  }
  if (req.method === "GET" && req.url === "/health") return send(200, { status: "ok" });
  if (req.method === "POST" && req.url === "/hook") {
    let raw = "";
    for await (const c of req) raw += c;
    try {
      const msg = JSON.parse(raw);
      const payload = msg.payload || {};
      const text = typeof payload === "string" ? payload : payload.text || JSON.stringify(payload);
      return send(200, await answer(text, payload.battery_nonce));
    } catch (e) {
      return send(200, failed(e.message, { exit_code: null, timed_out: false, stdout: "", stderr: "" }));
    }
  }
  send(404, { error: "not found" });
}).listen(PORT, async () => {
  console.log(`${HARNESS} consumer :${PORT} (live mode: ${LIVE ? "on" : "off — " + (!HAS_KEY ? "missing " + KEY_ENV : "requires " + LIVE_ENV + "=1")})`);
  await register();
});
