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
import { createServer } from "node:http";
import { execFile } from "node:child_process";

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

async function register() {
  const pub = [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, "0")).join("");
  const body = {
    id: AGENT_ID,
    public_key: pub,
    webhook: {
      url: `http://${AGENT_ID}:${PORT}/hook`,
      delivery_mode: "blocking",
      schema_template: "generic",
      response_map: { reply: "reply" },
    },
    guard: { policies: [{ id: "default" }] },
  };
  const r = await fetch(`${CRIER}/agents`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  registered = r.ok || r.status === 409;
  console.log(`${AGENT_ID} registered: ${r.status}`);
}

// Run a CLI harness binary; 60s cap so a slow model bootstrap never exceeds
// the blocking-delivery budget. Errors become the reply text (best-effort).
function runCli(bin, args) {
  return new Promise((resolve) => {
    execFile(
      bin, args,
      { env: { ...process.env, DEEPSEEK_API_KEY: KEY }, timeout: 60000 },
      (err, stdout) => resolve(err ? `(${HARNESS} error: ${err.message})` : (stdout.trim() || `(${HARNESS} produced no output)`))
    );
  });
}

async function answer(text) {
  if (!LIVE) {
    return `${HARNESS}(canned, no ${LIVE_ENV}): received "${text.slice(0, 80)}"`;
  }
  try {
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
      session.subscribe((ev) => {
        if (ev.type === "message_update" && ev.assistantMessageEvent?.type === "text_delta") {
          out.push(ev.assistantMessageEvent.delta);
        }
      });
      await session.prompt(`Answer in one sentence, no tools: ${text.slice(0, 500)}`);
      await session.dispose();
      return (out.join("") || "(pi session produced no text)").trim();
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
        return `${HARNESS}(no live runner configured for this harness)`;
    }
  } catch (e) {
    return `(${HARNESS} error: ${e.message})`;
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
    const ok = await ensureRegistered();
    return send(200, { registered: ok });
  }
  if (req.method === "GET" && req.url === "/health") return send(200, { status: "ok" });
  if (req.method === "POST" && req.url === "/hook") {
    let raw = "";
    for await (const c of req) raw += c;
    try {
      const msg = JSON.parse(raw);
      const payload = msg.payload || {};
      const text = typeof payload === "string" ? payload : payload.text || JSON.stringify(payload);
      const reply = await answer(text);
      return send(200, { reply });
    } catch (e) {
      return send(500, { error: e.message });
    }
  }
  send(404, { error: "not found" });
}).listen(PORT, async () => {
  console.log(`${HARNESS} consumer :${PORT} (live mode: ${LIVE ? "on" : "off — " + LIVE_ENV + (HARNESS === "pi-agent" ? "" : "=1") + " + DEEPSEEK_API_KEY required"})`);
  await register();
});
