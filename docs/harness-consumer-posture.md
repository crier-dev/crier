# Harness consumer posture

Status: repository-owned contract, live deployment owner-gated.

Crier classifies every registered agent with `registry.ClassifyConsumerCensus` and
serves the result at `GET /harness/consumer-census`:

- `wired` requires a valid consumer declaration plus a valid webhook
  configuration. Blocking consumers must set `require_response_token: true`; each
  run then receives a fresh `X-Crier-Response-Token` and must echo it before Crier
  accepts the reply.
- `archived` requires `archived_reason`, an explicit operator explanation.
- `unclassified` is fail-closed and names the missing or invalid configuration.

The compose registration fixture is repository-owned and now carries signed-key
registration data, a harness consumer contract, blocking delivery, and the
per-run response-token requirement for the blocking sink/consumer rows. The
fixture is loaded by the real sink and consumer entrypoints, and `internal/ci`
checks every fixture against the strict production decoder.

The census proves repository configuration, not that a remote host, credential,
DNS name, or endpoint is currently reachable. The agent-ecosystem battery's
registration gate (`GET /agents/{id}`) plus each consumer's `/ready` path is the
standing liveness loop: registration loss is retried and readiness stays 503
until the row is restored. A refused webhook endpoint is a failed delivery, not
an accepted 2xx; the blocking driver returns a named transport/budget error and
its response-token tests reject unproven replies.

The live fleet's external credentials and deployment execution remain owner/
runner-gated. This repository now owns the fixture wiring, census route, CI
contract test, and battery census/liveness assertions; it does not claim a live
bunker witness from a local checkout.

ch:trace row=HARNESS-CRIER-001 spec=none:board-acceptance test=internal/registry/consumer_census_test.go;internal/webhook/response_token_test.go;internal/ci/contracts_test.go;examples/agent-ecosystem/battery/battery.sh doc=docs/harness-consumer-posture.md evidence=none:live-bunker-run-not-available verdict=none:foreman-owned commit=worker-sha memory=none:foreman-owned
STATUS: implementation-complete
TIER: T2
EVIDENCE: repository-side census route, signed webhook fixture, blocking response-token tests, and harness-e2e liveness loop; none:live-bunker-witness
