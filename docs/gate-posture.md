# Crier gate posture

Crier keeps optional and operator-only HTTP surfaces explicit. The OpenAPI
document marks each conditional path with `x-crier-gate`, and the same route
and gate table in `cmd/server/gates.go` is checked against that document.

## Optional surfaces

- `CR_DAGGER_URL` arms the `/dagger/*` control surface. When it is unset, a
  request receives JSON `{"error":"not found","gate":"CR_DAGGER_URL"}`.
- `CR_DETECT_ENABLED` arms `/delivery-log`, `/delivery-log/verify`, `/alerts`,
  `/canaries`, and the detection-layer `/agents/{id}/kill-switch` route. When
  it is unset, those paths are not mounted and their 404 names the flag.

## Operator management

`CR_PERMISSIONS_ADMIN_TOKEN` is the distinct Bearer credential for principals,
bindings, grants, and trust-record management. Those paths remain visible so
an unarmed deployment answers a named 403 rather than making the API look
missing; the body includes `"gate":"CR_PERMISSIONS_ADMIN_TOKEN"`.

The trust verification operation is a read-only evaluation using the ordinary
reach credential; trust anchors and delegations are the management records.

## WebSocket transport

`GET /relay/subscribe/{topic}` is the WebSocket handshake route, not a normal
JSON GET endpoint. A plain GET is rejected before the upgrade with the JSON
error `websocket upgrade required`. Clients must send a WebSocket Upgrade
request.