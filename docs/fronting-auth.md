# Browser access to an auth-enabled relay (`crier-front`)

When a relay runs with `CR_AUTH_TOKEN` set, every request except the public paths
(`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs`) must carry
`Authorization: Bearer <token>`. That includes ordinary navigation, so a browser
opening `GET /chat` is refused `401`: a browser cannot attach that header on its
own.

The shipped arrangement is an authenticating **front**: a process that holds the
credential and injects it on the wire, so `GET /chat` and every API call work in a
browser while the shared token never reaches page source, cookies, localStorage or
sessionStorage. `crier-front` is that process, built from this repo.

```sh
make build-front

# relay, closed to direct browser access
CR_AUTH_TOKEN="$(cat /run/secrets/crier-token)" ./bin/crier -port 8765

# front on loopback, injecting the same credential
CR_AUTH_TOKEN="$(cat /run/secrets/crier-token)" \
  ./bin/crier-front -listen 127.0.0.1:8770 -upstream http://127.0.0.1:8765
```

Then browse `http://127.0.0.1:8770/chat`. `http://127.0.0.1:8765/chat` keeps
answering `401` with the fronting-requirement page, which is the intended signal
for anyone who lands on the relay directly.

## What the front guarantees

| Property | Why it matters |
| --- | --- |
| Token read from the environment only | It is never a CLI argument, so it does not appear in `ps`, and never a config file the front reads at runtime. |
| Client `Authorization` is replaced, never forwarded | A caller cannot present a different credential through the front; the value on the wire is always the front's own. |
| Token never echoed | No response header or body carries it, so page source, storage and cookies stay clean. |
| Loopback bind by default | `-listen` defaults to `127.0.0.1:8770`; exposing it is a deliberate act. |
| Refuses to start without a token | An auth-enabled relay cannot be accidentally fronted by an unauthenticated hop. |
| Streams pass through unbuffered | Chat reads and SSE are not held in a buffer or severed by a write deadline. |
| Named upstream failure | An unreachable relay yields `502 FRONT_UPSTREAM_UNAVAILABLE`, not a hang. |
| `/front-health` | Liveness for the front alone, without the credential. |

## Deploying it

Put the front behind whatever terminates TLS for you and bind it so only that
layer can reach it. A systemd unit is enough:

```ini
[Unit]
Description=Crier authenticating front
After=network-online.target crier.service
Requires=crier.service

[Service]
User=crier
EnvironmentFile=/etc/crier/front.env      # CR_AUTH_TOKEN=... CR_FRONT_UPSTREAM=...
ExecStart=/opt/crier/bin/crier-front -listen 127.0.0.1:8770
Restart=on-failure
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
```

Keep `/etc/crier/front.env` at mode `0600` and owned by the service user.

## What this does not do

- It does not authenticate individual humans: anyone who can reach the front
  gets the relay's shared credential. Put it behind an identity-aware proxy if you
  need per-person access control, or use the relay's own agent-signature path for
  agents.
- It does not mint per-user tokens. A token-mint route was the other arrangement
  considered for `DF-CRIER-333`; it was not built.
- It does not make the relay safe to expose publicly on its own. The relay keeps
  its documented posture: agent request signatures for agent actions, and the
  shared bearer for the fronted surface.

## Verifying a deployment

```sh
# the relay must stay closed
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8765/chat      # 401

# the front must carry the credential
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8770/chat      # 200

# a wrong credential must still be refused at the relay
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer wrong' \
  http://127.0.0.1:8765/sessions                                        # 401
```

Measured on a scratch relay at `1fda6419`: `401`, `200`, `401`, and
`GET /sessions` through the front `200`. In a real headless Chromium session the
panel loaded through the front, an agent registered `201`, a message was recorded
`accepted: 1`, and the token appeared in neither page source, `localStorage`,
`sessionStorage` nor cookies.
