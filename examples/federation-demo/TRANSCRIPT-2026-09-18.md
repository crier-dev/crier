# CR-FEAT-006 relay-to-relay federation demo — TRANSCRIPT

- date: 2026-09-19T04:09:04Z
- repo: c6a6e4e (feat(status): expose effective runtime configuration — make live posture observable. Addresses DF-CRIER-113.)
- relay-1: http://127.0.0.1:18771 (CR_FED_LINKS=http://127.0.0.1:18776, CR_FED_NAME=relay-1)
- relay-2: http://127.0.0.1:18776 (no links)
- webhook: http://127.0.0.1:18781/webhook

==> [1/8] build crier
    built /tmp/tmp.LiNm2Gj2I5/crier

==> [2/8] start echo webhook on :18781
[webhook] listening on :18781
    webhook pid 2943269

==> [3/8] start relay-2 on :18776 (no federation links)
time=2026-09-18T23:09:04.850-05:00 level=WARN msg="auth disabled, all requests pass through (development mode)"
time=2026-09-18T23:09:04.850-05:00 level=INFO msg="registry backend" type=memory hint="set CR_DATABASE_URL for PostgreSQL"
time=2026-09-18T23:09:04.851-05:00 level=INFO msg="message guard" enabled=true model=deepseek-v4-flash timeout=10s max_concurrent=8 circuit_threshold=10 kanban_queue=100 kanban_writer=cli
time=2026-09-18T23:09:04.851-05:00 level=INFO msg="webhook delivery" enabled=true timeout=30s retries=5 signing=false
time=2026-09-18T23:09:04.851-05:00 level=INFO msg="crier starting" port=18776 services=relay+mesh+registry version=dev-c6a6e4e1-dirty
time=2026-09-18T23:09:05.059-05:00 level=INFO msg=request method=GET path=/health status=200 duration=7.825µs request_id=06b70eacc72912dd7b2f0e38
time=2026-09-18T23:09:05.141-05:00 level=INFO msg=request method=GET path=/health status=200 duration=3.065µs request_id=caa71b4976b4ed3b9f2e0df5
{"status":"ok"} <- relay-2 healthy

==> [4/8] start relay-1 on :18771 (linked to relay-2)
time=2026-09-18T23:09:05.150-05:00 level=WARN msg="auth disabled, all requests pass through (development mode)"
time=2026-09-18T23:09:05.150-05:00 level=INFO msg="registry backend" type=memory hint="set CR_DATABASE_URL for PostgreSQL"
time=2026-09-18T23:09:05.150-05:00 level=INFO msg="message guard" enabled=true model=deepseek-v4-flash timeout=10s max_concurrent=8 circuit_threshold=10 kanban_queue=100 kanban_writer=cli
time=2026-09-18T23:09:05.150-05:00 level=INFO msg=federation links=[http://127.0.0.1:18776] name=relay-1 auth=false max_hold_s=300 hold_queue="memory (process-lifetime)" pending=0
time=2026-09-18T23:09:05.151-05:00 level=INFO msg="webhook delivery" enabled=true timeout=30s retries=5 signing=false
time=2026-09-18T23:09:05.151-05:00 level=INFO msg="crier starting" port=18771 services=relay+mesh+registry version=dev-c6a6e4e1-dirty
time=2026-09-18T23:09:05.359-05:00 level=INFO msg=request method=GET path=/health status=200 duration=2.384µs request_id=c199176acd30b3115cbc1738
time=2026-09-18T23:09:05.439-05:00 level=INFO msg=request method=GET path=/health status=200 duration=2.694µs request_id=b73320d4b0f281dca812b030
{"status":"ok"} <- relay-1 healthy

==> [5/8] register agents
time=2026-09-18T23:09:05.456-05:00 level=INFO msg=request method=POST path=/agents status=201 duration=245.363µs request_id=1c8c64500dc7af8f360c48b0
{"id":"relay-1-agent","public_key":"043f33162c1167f27fa56cfa9097a0102fc9111fb0c9997caa4445e21b768345","capabilities":["echo"],"status":"online","registered_at":"2026-09-18T23:09:05.456155891-05:00","last_seen":"2026-09-18T23:09:05.456155891-05:00"}

    POST relay-1/agents -> 201
time=2026-09-18T23:09:05.473-05:00 level=INFO msg=request method=POST path=/agents status=201 duration=255.072µs request_id=7cae224e44c4ed2b01e56d82
{"id":"relay-2-agent","public_key":"9adbb5580363ca272b7043c8aeb31661f4431521fe7a312aa3261eaa4230e4ae","capabilities":["echo","llm"],"status":"online","registered_at":"2026-09-18T23:09:05.473588494-05:00","last_seen":"2026-09-18T23:09:05.473588494-05:00","webhook":{"url":"http://127.0.0.1:18781/webhook","schema_template":"openai-compatible","delivery_mode":"blocking"}}

    POST relay-2/agents -> 201

==> [6/8] deliver from relay-1 client to relay-2 agent (blocking)
    POST http://127.0.0.1:18771/agents/relay-2-agent/inbox  (sender=relay-1-agent, session_id=sess-1, request_id=req-1)
time=2026-09-18T23:09:05.482-05:00 level=INFO msg="guard router: provider skipped" provider=deepseek model=deepseek-v4-flash reason="no api key" key_ref=env:DEEPSEEK_API_KEY
time=2026-09-18T23:09:05.482-05:00 level=WARN msg=guard msg=3f360c143c334c75cdb78bc2 target=relay-2-agent policy=default chan=sess-1/ kind=message decision=allow risk=medium request_id=717c1c8ad7e3977f468b1fbb provider="" model="" patterns="" errored=true quarantined=false sanitized=false reason="guard_error: all providers failed: no provider api key" ms=0 payload_bytes=29
time=2026-09-18T23:09:05.482-05:00 level=INFO msg="webhook: delivery dispatched" agent=relay-2-agent endpoint=127.0.0.1:18781/webhook messages=1 request_id=717c1c8ad7e3977f468b1fbb message_id=3f360c143c334c75cdb78bc2
[webhook] POST /webhook model='deepseek-v4-flash' content='hello from relay-1'
time=2026-09-18T23:09:05.482-05:00 level=INFO msg="webhook: delivery delivered" agent=relay-2-agent endpoint=127.0.0.1:18781/webhook messages=1 request_id=717c1c8ad7e3977f468b1fbb message_id=3f360c143c334c75cdb78bc2 status=200
time=2026-09-18T23:09:05.482-05:00 level=INFO msg=request method=POST path=/agents/relay-2-agent/inbox status=200 duration=990.871µs request_id=717c1c8ad7e3977f468b1fbb
time=2026-09-18T23:09:05.483-05:00 level=INFO msg=request method=POST path=/agents/relay-2-agent/inbox status=200 duration=1.573539ms request_id=7978a9e8b74cd0bb12d6ba90
    response: {"id":"3f360c143c334c75cdb78bc2","transport":"webhook","reply":"echo: hello from relay-1","session_id":"sess-1","request_id":"req-1"}
    HTTP code: 200
    PASS: reply returned to the original sender (relay-2 webhook reply relayed verbatim)

==> [7/8] ghost agent — all links 404 -> 404
time=2026-09-18T23:09:05.499-05:00 level=INFO msg=request method=POST path=/agents/ghost/inbox status=404 duration=21.63µs request_id=d0f61717bdb80c68a483d71b
time=2026-09-18T23:09:05.499-05:00 level=INFO msg=request method=POST path=/agents/ghost/inbox status=404 duration=299.534µs request_id=5136e4ac0fbc3eb873731a9d
{"error":"agent not found"}

    POST relay-1/agents/ghost/inbox -> 404

==> [8/8] cross-relay discovery: GET /fed/peers
    relay-1 /fed/peers (must show BOTH relays with their agents):
time=2026-09-18T23:09:05.506-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=40.565µs request_id=677cc14c6fdc91a50f73bc87
time=2026-09-18T23:09:05.507-05:00 level=INFO msg=request method=GET path=/fed/peers status=200 duration=380.663µs request_id=c2e56281871729d7ff32e9ad
{
    "peers": [
        {
            "name": "relay-1",
            "url": "http://localhost:18771",
            "agents": [
                {
                    "id": "relay-1-agent",
                    "capabilities": [
                        "echo"
                    ]
                }
            ]
        },
        {
            "name": "127.0.0.1:18776",
            "url": "http://127.0.0.1:18776",
            "agents": [
                {
                    "id": "relay-2-agent",
                    "capabilities": [
                        "echo",
                        "llm"
                    ]
                }
            ]
        }
    ]
}
    PASS: peers listing shows both relays with their agents (relay-2 on the selected :18776)

    relay-2 /fed/peers (no links -> just itself):
time=2026-09-18T23:09:05.546-05:00 level=INFO msg=request method=GET path=/fed/peers status=200 duration=117.998µs request_id=751d80b941adcf22a29541a1
{
    "peers": [
        {
            "name": "localhost:18776",
            "url": "http://localhost:18776",
            "agents": [
                {
                    "id": "relay-2-agent",
                    "capabilities": [
                        "echo",
                        "llm"
                    ]
                }
            ]
        }
    ]
}

==> local agent lists (for completeness)
time=2026-09-18T23:09:05.570-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=86.089µs request_id=2443b2394f23b21a531465e5
    relay-1 /agents: {"agents":[{"id":"relay-1-agent","public_key":"043f33162c1167f27fa56cfa9097a0102fc9111fb0c9997caa4445e21b768345","capabilities":["echo"],"status":"online","registered_at":"2026-09-18T23:09:05.456155891-05:00","last_seen":"2026-09-18T23:09:05.456155891-05:00"}]}
time=2026-09-18T23:09:05.576-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=34.724µs request_id=b0ce3997e423a9b177dea194
    relay-2 /agents: {"agents":[{"id":"relay-2-agent","public_key":"9adbb5580363ca272b7043c8aeb31661f4431521fe7a312aa3261eaa4230e4ae","capabilities":["echo","llm"],"status":"online","registered_at":"2026-09-18T23:09:05.473588494-05:00","last_seen":"2026-09-18T23:09:05.473588494-05:00","webhook":{"url":"http://127.0.0.1:18781/webhook","schema_template":"openai-compatible","delivery_mode":"blocking"}}]}

==> DEMO PASS: relay-1 -> relay-2 delivery via webhook, reply to sender, peers listing shows both
