# CR-GAP-050 WS subscribe + mesh demo — TRANSCRIPT

- date: 2026-08-31T17:28:20Z
- repo: 70eef95 (foreman tick [ci skip])
- relay: http://127.0.0.1:18767 (auth-disabled)

==> [1/7] build crier + demo client (go toolchain only)
    built /tmp/tmp.GjLCmborFJ/crier and /tmp/tmp.GjLCmborFJ/ws-mesh-demo

==> [2/7] start relay on :18767 (auth-disabled)
time=2026-08-31T12:28:21.050-05:00 level=WARN source=/home/kara/crier/internal/middleware/auth.go:19 msg="auth disabled, all requests pass through (development mode)"
{"status":"ok"} <- relay healthy

==> [3/7] spawn subscriber on /relay/subscribe/demo
    subscriber ready (pid 3197225)

==> [4/7] spawn two mesh peers
    demo-agent-a and demo-agent-b connected

==> [5/7] GET /mesh/peers (must show count 2 with both agent IDs)
    {"count":2,"peers":[{"agent_id":"demo-agent-b"},{"agent_id":"demo-agent-a"}]}
    PASS: both mesh peers registered, count 2

==> [6/7] publish an event (X-Agent-ID: demo-publisher)
    POST /relay/publish -> HTTP 202

==> [7/7] subscriber must receive the event over WS /relay/subscribe
    subscriber received: EVENT {"msg":"hello from run-demo","ts":"crier-demo"}
    PASS: event fanned out to subscriber over WebSocket

==> DEMO PASS: WS subscribe fan-out + mesh peer registration verified live
