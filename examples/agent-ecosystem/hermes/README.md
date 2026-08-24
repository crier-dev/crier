# Hermes agent in the ecosystem stack

The `hermes` service is **profile-gated** (`docker compose --profile hermes up`)
because the Hermes container is a full agent runtime, not a thin consumer.
It demonstrates the same wiring from the Hermes side: crier is configured as
a webhook target in the Hermes gateway, so agent sessions can post messages
into the bus.

## Prerequisites

Build the image first (per the `hermes-docker-deployment` skill — s6-overlay
is PID 1, so you cannot override `entrypoint`; configuration is mounted via
`/etc/cont-init.d/`):

```bash
# from the hermes-agent source tree
docker build -t hermes-agent:latest .
```

## What's in this directory

- `cont-init.d/10-register-crier.sh` — runs at container start (s6-overlay
  cont-init stage): registers the `hermes` agent with crier (webhook →
  `http://hermes:9000/hook`), with the default guard policy.
- `hermes.config.yaml` — example Hermes gateway config with the crier webhook
  target (`webhook.url: http://crier:8767/agents/hermes/inbox`).

## Wiring notes

- Hermes → crier: the Hermes gateway posts message events to
  `http://crier:8767/agents/hermes/inbox` (async fire-and-forget — the
  `delivery_mode: async` lane, HTTP 202).
- crier → Hermes: deliveries land on the Hermes HTTP gateway endpoint
  (`hermes.config.yaml`), which turns them into a chat session.

## Enabling

```bash
docker compose --profile hermes up -d
docker compose exec hermes bash /etc/cont-init.d/10-register-crier.sh
```
