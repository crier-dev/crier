# Harness consumer posture

Status: repository-owned contract, live deployment owner-gated.

Crier classifies every registered agent with `registry.ClassifyConsumerCensus`:

- `wired` requires a valid webhook configuration. Blocking consumers should set
  `require_response_token: true`; each run then receives a fresh
  `X-Crier-Response-Token` and must echo it before Crier accepts the reply.
- `archived` requires `archived_reason`, an explicit operator explanation.
- `unclassified` is fail-closed and names the missing or invalid configuration.

The census proves only repository state. It does not prove that a remote host,
credential, DNS name, or live endpoint exists. The deployment owner must run a
live liveness check against every `wired` endpoint and retain its named failure
output. A refused connection is a failed delivery, not a 202 success; the
webhook driver's blocking path returns a named `WEBHOOK_FAILED`/transport error
and its attempt observer classifies unreachable endpoints.

The live fleet registration, signed webhook credentials, and host-level
liveness check are not available from this repository and remain unimplemented
owner-gated prerequisites. This document is the durable boundary, not a claim
that those external legs are complete.

ch:trace row=HARNESS-CRIER-001 spec=none:board-acceptance wave=.coding-hermes/waves/crier-foreman-2026-11-14-05-22-52.json test=internal/registry/consumer_contract_test.go;internal/registry/webhook_failure_test.go doc=docs/harness-consumer-posture.md evidence=go-test-registry verdict=none:foreman-owned commit=worker-sha memory=none:foreman-owned
STATUS: complete
TIER: T2
EVIDENCE: named test output and commit
