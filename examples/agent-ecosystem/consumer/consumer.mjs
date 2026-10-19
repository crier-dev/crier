// Shared agent-ecosystem consumer — receives messages through crier
// (blocking webhook) and answers with a real harness runtime when the
// harness's live-mode env + DEEPSEEK_API_KEY are set, canned otherwise.
//
// Parameterized by env (set per service in docker-compose.yml / Dockerfile):
//   AGENT_ID         registry id + in-cluster webhook host (default "agent")
//   HARNESS          display name + runtime dispatch key (default AGENT_ID)
//   PORT             in-container HTTP port (default 9100)
//   CRIER_URL        crier base URL (default http://crier:8767)
//   DEEPSEEK_API_KEY enables live mode (default empty)
//   <HARNESS>_LIVE   per-harness opt-in flag: "1" = run the real CLI
//
// Harnesses: pi-agent (SDK session — live whenever a key is present),
// opencode, claude-code, codex, aider, goose (CLI — live only with
// <HARNESS>_LIVE=1 AND a key; the CLIs' model bootstrap is heavy and can
// exceed the blocking-delivery budget, so canned keeps the battery
// deterministic on any host).
import { execFile } from "node:child_process";
import { accessSync, constants, readFileSync } from "node:fs";
import { delimiter, resolve } from "node:path";
import { createServer } from "node:http";

const REGISTRATION_TEMPLATE = JSON.parse(
  readFileSync(new URL("./registration-payloads.json", import.meta.url), "utf8")
).consumer;

const AGENT_ID = process.env.AGENT_ID || "agent";
const HARNESS = process.env.HARNESS || AGENT_ID;
const PORT = Number(process.env.PORT || "9100");
const CRIER = process.env.CRIER_URL || "http://crier:8767";
const KEY = process.env.DEEPSEEK_API_KEY || "";

// Per-harness live-mode env name (used in the canned reply + the live gate).
const LIVE_ENV = {
  "pi-agent": "DEEPSEEK_API_KEY",
  opencode: "OPENCODE_LIVE",
  "claude-code": "CLAUDE_CODE_LIVE",
  codex: "CODEX_LIVE",
  aider: "AIDER_LIVE",
  goose: "GOOSE_LIVE",
}[HARNESS] || `${HARNESS.toUpperCase().replace(/-/g, "_")}_LIVE`;
const LIVE = HARNESS === "pi-agent" ? !!KEY : process.env[LIVE_ENV] === "1" && !!KEY;

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
  return { reply: stdout.trim(), live: true, exec: command, stdout_bytes: Buffer.byteLength(stdout) };
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

// CLI failure (including timeout/auth failure) rejects; never turn it into reply text.
function runCli(bin, args) {
  const command = resolveCommand(bin);
  return new Promise((resolve, reject) => {
    execFile(
      command, args,
      { env: { ...process.env, DEEPSEEK_API_KEY: KEY }, timeout: 60000 },
      (err, stdout) => {
        if (err) return reject(err);
        try { resolve(liveReply(stdout, command)); } catch (e) { reject(e); }
      }
    );
  });
}

async function answer(text) {
  if (!LIVE) {
    return { reply: `${HARNESS}(canned, requires ${LIVE_ENV} + DEEPSEEK_API_KEY): received "${text.slice(0, 80)}"`, live: false, canned: true };
  }
  if (HARNESS === "pi-agent") {
    // Real pi SDK session: minimal prompt pointing at the message
    // (discovery pattern keeps the prompt small; model resolved from env).
    const { createAgentSession, SessionManager, ModelRegistry, AuthStorage } =
      await import("@earendil-works/pi-coding-agent");
    const auth = AuthStorage.create();
    const modelRegistry = ModelRegistry.create(auth);
    const { session } = await createAgentSession({
      sessionManager: SessionManager.inMemory(),
      authStorage: auth,
      modelRegistry,
    });
    const out = [];
    try {
      session.subscribe((ev) => {
        if (ev.type === "message_update" && ev.assistantMessageEvent?.type === "text_delta") {
          out.push(ev.assistantMessageEvent.delta);
        }
      });
      await session.prompt(`Answer in one sentence, no tools: ${text.slice(0, 500)}`);
      return liveReply(out.join(""), "sdk:@earendil-works/pi-coding-agent");
    } finally {
      await session.dispose();
    }
  }
  switch (HARNESS) {
    case "opencode":
      return runCli("opencode", ["run", `Answer in one sentence, no tools. Question: ${text.slice(0, 400)}`, "--model", "deepseek/deepseek-v4-flash"]);
    case "claude-code":
      return runCli("claude", ["-p", text, "--output-format", "text"]);
    case "codex":
      return runCli("codex", ["exec", "--skip-git-repo-check", text]);
    case "aider":
      return runCli("aider", ["--message", text, "--no-auto-commits", "--yes-always"]);
    case "goose":
      return runCli("goose", ["run", "--text", text]);
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

createServer(async (req, res) => {
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
      return send(200, await answer(text));
    } catch (e) {
      return send(500, { error: e.message, live: false });
    }
  }
  send(404, { error: "not found" });
}).listen(PORT, async () => {
  console.log(`${HARNESS} consumer :${PORT} (live mode: ${LIVE ? "on" : "off — " + LIVE_ENV + (HARNESS === "pi-agent" ? "" : "=1") + " + DEEPSEEK_API_KEY required"})`);
  await register();
});
