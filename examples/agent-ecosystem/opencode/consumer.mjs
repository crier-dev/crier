// OpenCode consumer — same mesh pattern with the opencode CLI runtime.
// On a received message it runs `opencode run` on a small task when a key is
// present (writes the task to /work/task.md first), canned reply otherwise.
import { createServer } from "node:http";
import { execFile } from "node:child_process";

const PORT = Number(process.env.PORT || "9102");
const CRIER = process.env.CRIER_URL || "http://crier:8767";
const KEY = process.env.DEEPSEEK_API_KEY || "";
// Real `opencode run` is opt-in (OPENCODE_LIVE=1): the CLI's model bootstrap
// is heavy and can exceed the blocking-delivery budget. Canned reply keeps
// the battery deterministic on any host.
const LIVE = process.env.OPENCODE_LIVE === "1";

let registered = false;

async function register() {
  const pub = [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, "0")).join("");
  const body = {
    id: "opencode",
    public_key: pub,
    webhook: {
      url: `http://opencode:${PORT}/hook`,
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
  console.log(`opencode registered: ${r.status}`);
}

function opencodeRun(text) {
  return new Promise((resolve) => {
    const task = `Answer in one sentence, no tools. Question: ${text.slice(0, 400)}`;
    execFile(
      "opencode", ["run", task, "--model", "deepseek/deepseek-v4-flash"],
      { env: { ...process.env, DEEPSEEK_API_KEY: KEY }, timeout: 60000 },
      (err, stdout) => resolve(err ? `(opencode error: ${err.message})` : (stdout.trim() || "(opencode produced no output)"))
    );
  });
}

async function ensureRegistered() {
  // Live check: if crier lost us (restart/recreate), re-register.
  const r = await fetch(`${CRIER}/agents/opencode`, { method: "GET" });
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
      const reply = (KEY && LIVE)
        ? await opencodeRun(text)
        : `opencode(canned, no OPENCODE_LIVE): received "${text.slice(0, 80)}"`;
      return send(200, { reply });
    } catch (e) {
      return send(500, { error: e.message });
    }
  }
  send(404, { error: "not found" });
}).listen(PORT, async () => {
  console.log(`opencode consumer :${PORT}`);
  await register();
});
