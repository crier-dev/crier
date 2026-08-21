# CR-FEAT-006 relay-to-relay federation demo — TRANSCRIPT

- date: 2026-08-21T17:16:47Z
- repo: f382452 (feat(federation): relay-to-relay links — CR_FED_LINKS forwarding fallback, cross-relay discovery via GET /fed/peers, live 2-relay E2E demo. Addresses CR-FEAT-006.)
- relay-1: http://127.0.0.1:18771 (CR_FED_LINKS=http://127.0.0.1:18772, CR_FED_NAME=relay-1)
- relay-2: http://127.0.0.1:18772 (no links)
- webhook: http://127.0.0.1:18773/webhook

==> [1/8] build crier
    built /dev/shm/tmp.8ogE8n6fGM/crier

==> [2/8] start echo webhook on :18773
[webhook] listening on :18773
    webhook pid 949257

==> [3/8] start relay-2 on :18772 (no federation links)
time=2026-08-21T12:16:48.279-05:00 level=WARN msg="auth disabled, all requests pass through (development mode)"
time=2026-08-21T12:16:48.279-05:00 level=INFO msg="registry backend" type=memory hint="set CR_DATABASE_URL for PostgreSQL"
time=2026-08-21T12:16:48.279-05:00 level=INFO msg="webhook delivery" enabled=true timeout=30s retries=5 signing=false
time=2026-08-21T12:16:48.279-05:00 level=INFO msg="crier starting" port=18772 services=relay+mesh+registry
time=2026-08-21T12:16:48.283-05:00 level=INFO msg=request method=GET path=/health status=200 duration=1.774µs
time=2026-08-21T12:16:48.288-05:00 level=INFO msg=request method=GET path=/health status=200 duration=4.148µs
{"status":"ok"} <- relay-2 healthy

==> [4/8] start relay-1 on :18771 (linked to relay-2)
time=2026-08-21T12:16:48.292-05:00 level=WARN msg="auth disabled, all requests pass through (development mode)"
time=2026-08-21T12:16:48.292-05:00 level=INFO msg="registry backend" type=memory hint="set CR_DATABASE_URL for PostgreSQL"
time=2026-08-21T12:16:48.292-05:00 level=INFO msg=federation links=[http://127.0.0.1:18772] name=relay-1
time=2026-08-21T12:16:48.292-05:00 level=INFO msg="webhook delivery" enabled=true timeout=30s retries=5 signing=false
time=2026-08-21T12:16:48.293-05:00 level=INFO msg="crier starting" port=18771 services=relay+mesh+registry
time=2026-08-21T12:16:48.297-05:00 level=INFO msg=request method=GET path=/health status=200 duration=4.428µs
time=2026-08-21T12:16:48.302-05:00 level=INFO msg=request method=GET path=/health status=200 duration=2.174µs
{"status":"ok"} <- relay-1 healthy

==> [5/8] register agents
time=2026-08-21T12:16:48.316-05:00 level=INFO msg=request method=POST path=/agents status=201 duration=278.786µs
{"id":"relay-1-agent","public_key":"00e6b84e1b0d87124da6981ad8880d5b027b943926592d0be2aa8398c6f06937","capabilities":["echo"],"status":"online","registered_at":"2026-08-21T12:16:48.316907198-05:00","last_seen":"2026-08-21T12:16:48.316907198-05:00"}

    POST relay-1/agents -> 201
time=2026-08-21T12:16:48.331-05:00 level=INFO msg=request method=POST path=/agents status=201 duration=316.296µs
{"id":"relay-2-agent","public_key":"e53e695079c9a63f05f8f9190114a047d6bceb415318efdc57c3dad6f595743a","capabilities":["echo","llm"],"status":"online","registered_at":"2026-08-21T12:16:48.331309533-05:00","last_seen":"2026-08-21T12:16:48.331309533-05:00","webhook":{"url":"http://127.0.0.1:18773/webhook","schema_template":"openai-compatible","delivery_mode":"blocking"}}

    POST relay-2/agents -> 201

==> [6/8] deliver from relay-1 client to relay-2 agent (blocking)
    POST http://127.0.0.1:18771/agents/relay-2-agent/inbox  (sender=relay-1-agent, session_id=sess-1, request_id=req-1)
[webhook] POST /webhook model='deepseek-v4-flash' content='hello from relay-1'
time=2026-08-21T12:16:48.338-05:00 level=INFO msg=request method=POST path=/agents/relay-2-agent/inbox status=200 duration=1.155881ms
time=2026-08-21T12:16:48.338-05:00 level=INFO msg=request method=POST path=/agents/relay-2-agent/inbox status=200 duration=1.80326ms
    response: {"id":"0600f3983cadc7b61d038ed9","reply":"echo: hello from relay-1","session_id":"sess-1","request_id":"req-1"}
    HTTP code: 200
    PASS: reply returned to the original sender (relay-2 webhook reply relayed verbatim)

==> [7/8] ghost agent — all links 404 -> 404
time=2026-08-21T12:16:48.355-05:00 level=INFO msg=request method=POST path=/agents/ghost/inbox status=404 duration=30.266µs
time=2026-08-21T12:16:48.355-05:00 level=INFO msg=request method=POST path=/agents/ghost/inbox status=404 duration=350.87µs
{"error":"agent not found"}

    POST relay-1/agents/ghost/inbox -> 404

==> [8/8] cross-relay discovery: GET /fed/peers
    relay-1 /fed/peers (must show BOTH relays with their agents):
time=2026-08-21T12:16:48.361-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=45.494µs
time=2026-08-21T12:16:48.361-05:00 level=INFO msg=request method=GET path=/fed/peers status=200 duration=330.221µs
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
            "name": "127.0.0.1:18772",
            "url": "http://127.0.0.1:18772",
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
    PASS: peers listing shows both relays with their agents

    relay-2 /fed/peers (no links -> just itself):
time=2026-08-21T12:16:48.401-05:00 level=INFO msg=request method=GET path=/fed/peers status=200 duration=101.538µs
{
    "peers": [
        {
            "name": "localhost:18772",
            "url": "http://localhost:18772",
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
time=2026-08-21T12:16:48.427-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=67.314µs
    relay-1 /agents: {"agents":[{"id":"relay-1-agent","public_key":"00e6b84e1b0d87124da6981ad8880d5b027b943926592d0be2aa8398c6f06937","capabilities":["echo"],"status":"online","registered_at":"2026-08-21T12:16:48.316907198-05:00","last_seen":"2026-08-21T12:16:48.316907198-05:00"}]}
time=2026-08-21T12:16:48.433-05:00 level=INFO msg=request method=GET path=/agents status=200 duration=32.54µs
    relay-2 /agents: {"agents":[{"id":"relay-2-agent","public_key":"e53e695079c9a63f05f8f9190114a047d6bceb415318efdc57c3dad6f595743a","capabilities":["echo","llm"],"status":"online","registered_at":"2026-08-21T12:16:48.331309533-05:00","last_seen":"2026-08-21T12:16:48.331309533-05:00","webhook":{"url":"http://127.0.0.1:18773/webhook","schema_template":"openai-compatible","delivery_mode":"blocking"}}]}

==> DEMO PASS: relay-1 -> relay-2 delivery via webhook, reply to sender, peers listing shows both
