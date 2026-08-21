
══════════════════════════════════════════════════════════
== CR-FEAT-008 demo — 2026-08-21 04:30:59 UTC
  crier port   : 18788 (memory backend, CR_AUTH_TOKEN unset, CR_REQUIRE_AGENT_SIG=false)
  adapter port : 18789
  session_id   : sess-demo-20260820-233059
  thread_id    : thr-demo-20260820-233059 (carried in the deliver payload — see README §session mapping)
  transcript   : /home/kara/crier/examples/hermes-gateway-demo/TRANSCRIPT-2026-08-20.md

══════════════════════════════════════════════════════════
== [0/7] preflight

══════════════════════════════════════════════════════════
== [1/7] build crier server
  built: /tmp/crier-demo.yxA442/crier

══════════════════════════════════════════════════════════
== [2/7] reply backend selection
  DEEPSEEK_API_KEY found in ~/.hermes/.env — live LLM replies (exactly 2 calls)

══════════════════════════════════════════════════════════
== [3/7] start gateway adapter (fake Hermes endpoint)
  adapter up: http://127.0.0.1:18789/webhook (pid 795759, log /tmp/crier-demo.yxA442/adapter.log)

══════════════════════════════════════════════════════════
== [4/7] start crier server
  crier up: http://127.0.0.1:18788 (pid 795853, log /tmp/crier-demo.yxA442/crier.log)

══════════════════════════════════════════════════════════
== [5/7] register agents
  registered harness-agent (inbox-backed — crier-mcp harness side)
  registered gateway-agent (webhook -> http://127.0.0.1:18789/webhook, schema_template=hermes-http-gateway, delivery_mode=blocking)

══════════════════════════════════════════════════════════
== [6/7] puzzle round trip 1 (harness-agent -> gateway-agent, blocking)
  deliver: POST /agents/gateway-agent/inbox req-20260820-233059-1 session=sess-demo-20260820-233059
  200 response: id=d1e450c7261ada1cd565b520 request_id=req-20260820-233059-1 session_id=sess-demo-20260820-233059
  reply: [session=sess-demo-20260820-233059 | thread= | turn=1] 17 times 23 equals **391**.

Here’s a quick way to see it:  
- 17 × 20 = 340  
- 17 × 3 = 51  
- 340 + 51 = **391**
PASS: blocking reply correlated (request_id echoed) = req-20260820-233059-1
PASS: blocking reply session_id echoed = sess-demo-20260820-233059
PASS: reply echoes session_id (sender sees the session mapping)
PASS: adapter counted this as turn 1 of the session

══════════════════════════════════════════════════════════
== envelope session mapping evidence (X-Crier-Session header vs body slot)
  X-Crier-Session header : sess-demo-20260820-233059
  body session_id slot   : ''
  resolved session_id    : sess-demo-20260820-233059
NOTE: body session_id slot rendered empty — known template-engine gap
      (resolvePath cannot descend into struct-typed EnvelopeMeta;
      X-Crier-Session header carried the session per spec §3).
      See README "Known gap". Demo continues — continuity proven via
      the header channel.

══════════════════════════════════════════════════════════
== [7/7] follow-up round trip 2 (same session, turn 2)
  deliver: POST /agents/gateway-agent/inbox req-20260820-233059-2 session=sess-demo-20260820-233059 (same session)
  200 response: id=aaa3ad4c6c5e89b33dc55ca6 request_id=req-20260820-233059-2 session_id=sess-demo-20260820-233059
  reply: [session=sess-demo-20260820-233059 | thread= | turn=2] Sure! Adding 11 to 391:

391 + 11 = **402**
PASS: follow-up correlated (request_id echoed) = req-20260820-233059-2
PASS: follow-up same session_id = sess-demo-20260820-233059
PASS: adapter saw turn 2 in the SAME session — session continuity on the wire

══════════════════════════════════════════════════════════
== adapter session continuity check (GET /sessions)
PASS: adapter recorded 2 turns under ONE session_id sess-demo-20260820-233059 (thread=)
       turn 1: 'What is 17 times 23?' -> '[session=sess-demo-20260820-233059 | thread= | turn=1] 17 times 23 equals **391**.\n\nHere’s a quick way to see it:  \n- 17 × 20 = 340  \n- 17 × 3 = 51  \n- 340 + 51 = **391**' [https://api.deepseek.com/chat/completions]
       turn 2: 'Add 11 to your previous answer.' -> '[session=sess-demo-20260820-233059 | thread= | turn=2] Sure! Adding 11 to 391:\n\n391 + 11 = **402**' [https://api.deepseek.com/chat/completions]

══════════════════════════════════════════════════════════
== inbox round trip (gateway-agent -> harness-agent inbox, retrieve + ack)
  delivered to harness-agent inbox: id=2f6e129781c08b86696b2a99 (HTTP 201)
PASS: harness-agent inbox retrieved the message (lease_id=ba7928d354fd831f8c4afca05c2a4e89)
PASS: inbox ack (204) — harness side round trip complete

══════════════════════════════════════════════════════════
== adapter webhook log (envelope contract evidence)
  {"path": "/webhook", "event": "message", "agent": "harness-agent", "session_id": "sess-demo-20260820-233059", "session_body": "", "session_header": "sess-demo-20260820-233059", "thread_id": "", "turn": 1, "model": "deepseek-v4-flash", "stream": false, "content": "What is 17 times 23?", "backend": "https://api.deepseek.com/chat/completions", "reply": "[session=sess-demo-20260820-233059 | thread= | turn=1] 17 times 23 equals **391**.\n\nHere\u2019s a quick way to see it:  \n- 17 \u00d7 20 = 340  \n- 17 \u00d7 3 = 51  \n- 340 + 51 = **391**", "signature": "verified", "ts": "2026-08-20T23:31:02-0500"}
  {"path": "/webhook", "event": "message", "agent": "harness-agent", "session_id": "sess-demo-20260820-233059", "session_body": "", "session_header": "sess-demo-20260820-233059", "thread_id": "", "turn": 2, "model": "deepseek-v4-flash", "stream": false, "content": "Add 11 to your previous answer.", "backend": "https://api.deepseek.com/chat/completions", "reply": "[session=sess-demo-20260820-233059 | thread= | turn=2] Sure! Adding 11 to 391:\n\n391 + 11 = **402**", "signature": "verified", "ts": "2026-08-20T23:31:03-0500"}

══════════════════════════════════════════════════════════
== summary
  crier server : http://127.0.0.1:18788 (memory backend, webhook driver, HMAC signing on)
  adapter      : http://127.0.0.1:18789/webhook (fake Hermes gateway, signature verified)
  reply backend: https://api.deepseek.com/chat/completions
  round 1      : d1e450c7261ada1cd565b520 -> reply: [session=sess-demo-20260820-233059 | thread= | turn=1] 17 times 23 equals **391**.

Here’s a quick way to see it:  
- 17 × 20 = 340  
- 17 × 3 = 51  
- 340 + 51 = **391**
  round 2      : aaa3ad4c6c5e89b33dc55ca6 -> reply: [session=sess-demo-20260820-233059 | thread= | turn=2] Sure! Adding 11 to 391:

391 + 11 = **402** (same session_id sess-demo-20260820-233059 — continuity)
  inbox        : 2f6e129781c08b86696b2a99 delivered + retrieved + acked by harness-agent
  run artifacts: /tmp/crier-demo.yxA442 (crier.log, adapter.log, json payloads)

DEMO PASSED — transcript: /home/kara/crier/examples/hermes-gateway-demo/TRANSCRIPT-2026-08-20.md
