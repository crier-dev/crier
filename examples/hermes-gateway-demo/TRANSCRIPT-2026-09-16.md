
══════════════════════════════════════════════════════════
== CR-FEAT-008 demo — 2026-09-16 11:42:26 UTC
  crier port   : 18788 (memory backend, CR_AUTH_TOKEN unset, CR_REQUIRE_AGENT_SIG=false)
  adapter port : 18789
  session_id   : sess-demo-20260916-064226
  thread_id    : thr-demo-20260916-064226 (carried in the deliver payload — see README §session mapping)
  transcript   : /home/kara/crier/examples/hermes-gateway-demo/TRANSCRIPT-2026-09-16.md

══════════════════════════════════════════════════════════
== [0/7] preflight

══════════════════════════════════════════════════════════
== [1/7] build crier server
  built: /tmp/crier-demo.rRo3pY/crier

══════════════════════════════════════════════════════════
== [2/7] reply backend selection
  DEEPSEEK_API_KEY found in ~/.hermes/.env — live LLM replies (exactly 2 calls)

══════════════════════════════════════════════════════════
== [3/7] start gateway adapter (fake Hermes endpoint)
  adapter up: http://127.0.0.1:18789/webhook (pid 1120171, log /tmp/crier-demo.rRo3pY/adapter.log)

══════════════════════════════════════════════════════════
== [4/7] start crier server
  crier up: http://127.0.0.1:18788 (pid 1120358, log /tmp/crier-demo.rRo3pY/crier.log)

══════════════════════════════════════════════════════════
== [5/7] register agents
  registered harness-agent (inbox-backed — crier-mcp harness side)
  registered gateway-agent (webhook -> http://127.0.0.1:18789/webhook, schema_template=hermes-http-gateway, delivery_mode=blocking)

══════════════════════════════════════════════════════════
== [6/7] puzzle round trip 1 (harness-agent -> gateway-agent, blocking)
  deliver: POST /agents/gateway-agent/inbox req-20260916-064226-1 session=sess-demo-20260916-064226
  200 response: id=8ff65edf9117bb18a4a34b34 request_id=req-20260916-064226-1 session_id=sess-demo-20260916-064226
  reply: [session=sess-demo-20260916-064226 | thread= | turn=1] 17 times 23 is **391**.

Calculation:  
17 × 23 = 17 × (20 + 3) = 340 + 51 = **391**.
PASS: blocking reply correlated (request_id echoed) = req-20260916-064226-1
PASS: blocking reply session_id echoed = sess-demo-20260916-064226
PASS: reply echoes session_id (sender sees the session mapping)
PASS: adapter counted this as turn 1 of the session

══════════════════════════════════════════════════════════
== envelope session mapping evidence (X-Crier-Session header vs body slot)
  X-Crier-Session header : sess-demo-20260916-064226
  body session_id slot   : 'sess-demo-20260916-064226'
  resolved session_id    : sess-demo-20260916-064226
PASS: template body carried the session_id (body slot populated)

══════════════════════════════════════════════════════════
== [7/7] follow-up round trip 2 (same session, turn 2)
  deliver: POST /agents/gateway-agent/inbox req-20260916-064226-2 session=sess-demo-20260916-064226 (same session)
  200 response: id=b1c3f6f4c22dd11c8e60f6bd request_id=req-20260916-064226-2 session_id=sess-demo-20260916-064226
  reply: [session=sess-demo-20260916-064226 | thread= | turn=2] 391 + 11 = **402**.
PASS: follow-up correlated (request_id echoed) = req-20260916-064226-2
PASS: follow-up same session_id = sess-demo-20260916-064226
PASS: adapter saw turn 2 in the SAME session — session continuity on the wire

══════════════════════════════════════════════════════════
== adapter session continuity check (GET /sessions)
PASS: adapter recorded 2 turns under ONE session_id sess-demo-20260916-064226 (thread=)
       turn 1: 'What is 17 times 23?' -> '[session=sess-demo-20260916-064226 | thread= | turn=1] 17 times 23 is **391**.\n\nCalculation:  \n17 × 23 = 17 × (20 + 3) = 340 + 51 = **391**.' [https://api.deepseek.com/chat/completions]
       turn 2: 'Add 11 to your previous answer.' -> '[session=sess-demo-20260916-064226 | thread= | turn=2] 391 + 11 = **402**.' [https://api.deepseek.com/chat/completions]

══════════════════════════════════════════════════════════
== inbox round trip (gateway-agent -> harness-agent inbox, retrieve + ack)
  delivered to harness-agent inbox: id=f45d41f0011f42f66ab1b5c7 (HTTP 201)
PASS: harness-agent inbox retrieved the message (lease_id=17b5708f80e186c58a5a342b2ec41903)
PASS: inbox ack (204) — harness side round trip complete

══════════════════════════════════════════════════════════
== adapter webhook log (envelope contract evidence)
  {"path": "/webhook", "event": "message", "agent": "harness-agent", "session_id": "sess-demo-20260916-064226", "session_body": "sess-demo-20260916-064226", "session_header": "sess-demo-20260916-064226", "thread_id": "", "turn": 1, "model": "deepseek-v4-flash", "stream": false, "content": "What is 17 times 23?", "backend": "https://api.deepseek.com/chat/completions", "reply": "[session=sess-demo-20260916-064226 | thread= | turn=1] 17 times 23 is **391**.\n\nCalculation:  \n17 \u00d7 23 = 17 \u00d7 (20 + 3) = 340 + 51 = **391**.", "signature": "verified", "ts": "2026-09-16T06:42:28-0500"}
  {"path": "/webhook", "event": "message", "agent": "harness-agent", "session_id": "sess-demo-20260916-064226", "session_body": "sess-demo-20260916-064226", "session_header": "sess-demo-20260916-064226", "thread_id": "", "turn": 2, "model": "deepseek-v4-flash", "stream": false, "content": "Add 11 to your previous answer.", "backend": "https://api.deepseek.com/chat/completions", "reply": "[session=sess-demo-20260916-064226 | thread= | turn=2] 391 + 11 = **402**.", "signature": "verified", "ts": "2026-09-16T06:42:29-0500"}

══════════════════════════════════════════════════════════
== summary
  crier server : http://127.0.0.1:18788 (memory backend, webhook driver, HMAC signing on)
  adapter      : http://127.0.0.1:18789/webhook (fake Hermes gateway, signature verified)
  reply backend: https://api.deepseek.com/chat/completions
  round 1      : 8ff65edf9117bb18a4a34b34 -> reply: [session=sess-demo-20260916-064226 | thread= | turn=1] 17 times 23 is **391**.

Calculation:  
17 × 23 = 17 × (20 + 3) = 340 + 51 = **391**.
  round 2      : b1c3f6f4c22dd11c8e60f6bd -> reply: [session=sess-demo-20260916-064226 | thread= | turn=2] 391 + 11 = **402**. (same session_id sess-demo-20260916-064226 — continuity)
  inbox        : f45d41f0011f42f66ab1b5c7 delivered + retrieved + acked by harness-agent
  run artifacts: /tmp/crier-demo.rRo3pY (crier.log, adapter.log, json payloads)

DEMO PASSED — transcript: /home/kara/crier/examples/hermes-gateway-demo/TRANSCRIPT-2026-09-16.md
