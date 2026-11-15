# BOARDCTL-HOOK-001 verification evidence

Tested published crier tree: 35ca5bf8f87254b1b24fd8750369ff719249c651.

Acceptance criterion requiring owner action: configure CR_INGEST_WEBHOOK_ENABLED and CR_INGEST_STORE_FILE in the supervised crier service environment, perform the approved restart, then rerun the scratch-repository boardctl delivery loop and verify the 202-to-inbox-to-doctor-depth-0 path. This tick is not authorized to perform that shared-service configuration or restart.

Observed live service:
- /version returned HTTP 200 with version 0.5.0-28-g83d28c13, commit 83d28c13, modified=false.
- /health returned HTTP 200 with status=ok.
- /ingress/sources returned HTTP 401 with missing Authorization header.
- The supervised environment file /home/kara/.hermes/secrets/crier-fleet.env contains neither CR_INGEST_WEBHOOK_ENABLED nor CR_INGEST_STORE_FILE.

Implementation provenance: the implementation is published in the sibling coding-hermes/boardctl repository at commit 2667a63; no crier implementation diff exists for this row. Therefore no crier coding worker or GitReins implementation lifecycle was created.

Local gate: verification-only; prior published crier event records go build ./..., go vet ./..., go test ./... -count=1, and bash scripts/check-board.sh as PASS. No source was changed in this tick.

Remote CI: observed-only mandatory top-three query showed completed success, pending, completed success; no completed failure.

STATUS: blocked
TIER: T3
EVIDENCE: Fresh live probes, exact published SHA, sibling implementation provenance, and a zero-worker owner-gated decision.
ch:trace row=BOARDCTL-HOOK-001 spec=none:sibling-repo-implementation wave=.coding-hermes/waves/crier-foreman-2026-11-15-06-56-07.json#none:verification-only test=none:owner-deployment-gate evidence=.coding-hermes/evidence/crier-foreman-2026-11-15-06-56-07-BOARDCTL-HOOK-001.md witness=live:http://127.0.0.1:8767/version verdict=none:owner-external-gate commit=none:verification-only memory=pending:write-after-publication
