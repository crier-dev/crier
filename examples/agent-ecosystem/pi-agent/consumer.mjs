// Pi Agent consumer — receives messages through crier (blocking webhook),
// answers with a real pi SDK session when an LLM key is present, canned
// otherwise. Demonstrates: crier webhook delivery + schema template + the
// pi runtime inside the mesh.
import { createServer } from "node:http";

const PORT = Number(process.env.PORT || "9101");
const CRIER = process.env.CRIER_URL || "http://crier:8767";
const KEY = process.env.DEEPSEEK_API_KEY || "";

let registered = false;
let piSession = null;

async function register() {
  const pub = [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, "0")).join("");
  const body = {
    id: "pi-agent",
    public_key: pub,
    webhook: {
      url: `http://pi-agent:${PORT}/hook`,
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
  console.log(`pi-agent registered: ${r.status}`);
}

async function answer(text) {
  if (KEY) {
    try {
      // Real pi session: minimal prompt pointing at the message (discovery
      // pattern keeps the prompt small; model resolved from env).
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
    } catch (e) {
      return `(pi session error: ${e.message})`;
    }
  }
  return `pi-agent(canned, no DEEPSEEK_API_KEY): received "${text.slice(0, 80)}"`;
}

async function ensureRegistered() {
  // Live check: if crier lost us (restart/recreate), re-register.
  const r = await fetch(`${CRIER}/agents/pi-agent`, { method: "GET" });
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
  console.log(`pi-agent consumer :${PORT}`);
  await register();
});
