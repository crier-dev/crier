# Installing crier on a fresh box

This is the install guide for a machine that has **none of this project's
prerequisites**: no Go, no Docker, no git, no `curl`. Every step below is written
for that box, and every claim in it was measured on one (see
[§9 Reproducing this verification](#9-reproducing-this-verification)) rather than
assumed from a developer machine.

If you already have a Go toolchain and a checkout, you do not need this file —
`make build` and the [README](README.md) are enough.

**Two paths, and only one of them works today.** Read
[§3](#3-path-a--install-the-prebuilt-binaries-no-toolchain) before you copy its
command: the prebuilt path is complete and verified, but the releases it installs
from have **no published assets yet**, so on the public release it fails closed
and installs nothing. [§4](#4-path-b--build-from-source) is the path that works
right now.

---

## 0. What your box actually has

Measured on the official `debian:13` (trixie) image — a stock, minimal Debian,
which is the smallest thing "a fresh box" means:

| tool | on a bare `debian:13` | why it matters here |
| --- | --- | --- |
| `curl` | **absent** | the documented one-liner is `curl … \| sh` — it cannot even start |
| `wget` | **absent** | the installer's only alternative fetch tool |
| `tar`, `gzip` | present | the Go toolchain tarball (Path B) unpacks with these |
| `sha256sum` | present | the installer verifies every download against it |
| `git` | **absent** | Path B clones the source with it |
| `make` | **absent** | Path B builds with it |
| `go` | **absent** | Path B needs Go 1.26.9+ |
| `useradd` | present | the system service wants its own account (§6.1) |
| `systemd` | present (on a real host) | §6 runs crier as a service |
| `openssl`, `xxd` | **absent** | only the *hand-written signing* recipes need these, not the install |

Two consequences you should act on before anything else:

1. **A fresh box has no fetch tool at all.** Neither `curl` nor `wget`. The
   README's claim that the installer "needs `curl` (or `wget`) and nothing else"
   is true of the *installer* — it is not true of the box. §2.1 installs one.
2. **Nothing here assumes root.** Both a root path (§2.1 with `apt-get`) and a
   no-root path are given where one exists, and the install itself never needs
   root: it writes to `$HOME`.

---

## 1. Choose a path

| | Path A — prebuilt binaries | Path B — build from source |
| --- | --- | --- |
| needs a toolchain | no | Go 1.26.9+, git, make |
| needs a fetch tool | yes (`curl`/`wget`) | yes (for git/Go, or bring a tarball) |
| needs root | no | no (no-root recipe in §2.2) |
| installs to | `$HOME/.local/bin` | anywhere you choose |
| ships checksums | yes, verified against `SHA256SUMS` | no — you built it from the source you cloned |
| **works on the public release today** | **no — no assets published (§3)** | **yes (§4)** |
| best for | testers, and any box without a compiler | anyone who wants it running now |

---

## 2. Prerequisites

### 2.1 A fetch tool (both paths)

```sh
# check first — a bare Debian answers "absent" for both
command -v curl || command -v wget || echo "NO FETCH TOOL — install one:"
```

With root:

```sh
apt-get update && apt-get install -y curl ca-certificates    # Debian/Ubuntu
```

`ca-certificates` matters as much as `curl`: without it `https://` fetches fail
with a TLS trust error rather than a 404, which looks like a network problem.

Without root there is no supported no-root route to a TLS-capable fetch tool
(`curl` and `wget` are not portable single-file binaries here, and the shell's
`/dev/tcp` cannot do TLS). If you have no root on a box with no fetch tool, use
a machine that has one to carry the Go tarball in (§2.2), or ask for `curl`.

### 2.2 Go 1.26.9 or later (Path B only)

A no-root install of the official tarball. **Unpack it somewhere other than
`$HOME/go`** — that path is the default `GOPATH`, and pointing `GOROOT` and
`GOPATH` at one directory makes the toolchain warn on every command and install
its own binaries into itself:

```sh
mkdir -p "$HOME/sdk" && cd "$HOME/sdk"
curl -LO https://go.dev/dl/go1.26.9.linux-amd64.tar.gz   # linux-arm64 on ARM
tar -C "$HOME/sdk" -xzf go1.26.9.linux-amd64.tar.gz      # -> $HOME/sdk/go/bin/go
export PATH="$HOME/sdk/go/bin:$PATH"
echo 'export PATH="$HOME/sdk/go/bin:$PATH"' >> "$HOME/.bashrc"
go version            # go version go1.26.9 linux/amd64
go env GOROOT GOPATH  # MUST print two DIFFERENT paths, and no warning
```

Measured on a bare box: the archive is 66,890,545 bytes, `tar -xzf` exits 0 and
produces `$HOME/sdk/go`, and with `GOPATH` unset the toolchain resolves
`GOPATH=$HOME/go` / `GOROOT=$HOME/sdk/go` with **no collision warning**. If you
see `warning: both GOPATH and GOROOT are the same directory`, you unpacked into
`$HOME` (giving `GOROOT=$HOME/go`); fix it with `export GOPATH="$HOME/gopath"`
rather than moving the toolchain.

`git` and `make` are also Path B prerequisites:

```sh
apt-get install -y git make        # with root
```

---

## 3. Path A — install the prebuilt binaries (no toolchain)

> ### ⚠️ This path fails closed on the release that exists today
>
> `crier-dev/crier` currently has exactly one release, `v0.1.0-rc2`, and its
> asset list is **empty** (`gh release view v0.1.0-rc2 --json assets` →
> `{"assets":[]}`). The installer fetches `SHA256SUMS` first and refuses to
> install anything it cannot verify, so on a fresh box the documented one-liner
> ends like this — measured, exit status 1, **nothing installed**:
>
> ```
> ==> download
> curl: (22) The requested URL returned error: 404
> install.sh: FAIL: download failed: https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
>        (a 404 here means the tag has no such asset, or the release is a
>        prerelease and so is not 'latest' — pass --version <tag> explicitly;
>        see https://github.com/crier-dev/crier/releases)
> ```
>
> That is the installer working as designed — but it means **there is no
> prebuilt install to be had on the public release right now**. Use §4.
> A release manager publishes the assets with
> `make release-upload VERSION=<tag>` ([docs/releases.md](docs/releases.md) §4);
> the acceptance drive for the whole front door is
> `make install-path-selftest`, which passes.

Once a release carries its assets, the documented command is:

```sh
curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh
```

It downloads the pair for your OS and architecture, verifies **each** binary
against the release's own `SHA256SUMS`, installs both into `$HOME/.local/bin`,
and runs them to prove they execute. Flags (`sh -s --` passes them through a
pipe):

| flag | effect |
| --- | --- |
| `--version v0.1.0-rc3` | install that tag instead of the newest release |
| `--dir "$HOME/bin"` | install somewhere else |
| `--base-url <url>` | fetch from a mirror or a local directory server |
| `--help` | usage |

Env equivalents: `CRIER_VERSION`, `CRIER_INSTALL_DIR`, `CRIER_BASE_URL`.

There is **no flag to skip checksum verification**, by design. These all install
nothing and exit non-zero: a missing `SHA256SUMS`, a manifest with no line for
your binary, a digest that does not match, or a box with neither `sha256sum` nor
`shasum`.

The release publishes **bare static executables** — `crier_linux_amd64`,
`crier-mcp_linux_amd64`, and so on — plus `install.sh` and `SHA256SUMS`. There is
no archive to extract: nothing is tarred, and the README's description of the
asset set as "archives" is inaccurate. (The only tarball in either install path
is the Go toolchain's, in §2.2.)

---

## 4. Path B — build from source

```sh
git clone --depth 1 https://github.com/crier-dev/crier.git
cd crier
make build build-mcp
```

Then place the binaries (§5) and [run it](#6-run-it-as-a-service). Verified
end-to-end on a bare box as an unprivileged user, with a no-root Go install:

```
clone      : git clone --depth 1 https://github.com/crier-dev/crier.git  -> rc 0
HEAD       : d6a8e2a
make       : build + build-mcp -> rc 0, bin/crier 31,628,333 bytes
             (module downloads included; 11s on this host's link)
-version   : crier v0.1.0-rc2-c34a6c25     (a tag build)  /  d6a8e2a (branch build)
boot       : ./bin/crier -port 18923 -pidfile "$HOME/crier.pid"  -> /health {"status":"ok"}
stop       : ./bin/crier -stop -pidfile "$HOME/crier.pid"        -> "pid stopped"
```

### ⚠️ Do not pin `--branch v0.1.0-rc2` — the README's from-source recipe does

`README.md` §"Install both binaries from source" tells you to
`git clone --branch v0.1.0-rc2 --depth 1 …`. That clones a **tag cut on
2026-09-21**, and the tag's tree does not have all of the configuration this
repository documents at `main`. Measured, same box, same command, two checkouts:

| checkout | `.go` files mentioning `CR_SESSION_BACKEND` | `CR_SESSION_BACKEND=sqlite` | sqlite file created |
| --- | --- | --- | --- |
| `--branch v0.1.0-rc2` (`c34a6c2`) | **0** | **silently ignored** | **no** |
| default branch (`d6a8e2a`) | 12 | honored | yes, 106,496 bytes |

A pinned tag is a legitimate thing to want — do pin it when you mean *"test
exactly what was released"*. But understand what it gives you: that tag's code,
and an unknown env var is not an error, it is a no-op. If you are installing
crier rather than testing a release, clone the default branch as above. (This
is why [TESTERS.md](TESTERS.md) §1's clone has no `--branch` — and why the
tag-pinned snippet in the README does.)

---

## 5. Put the binaries somewhere on your PATH

The installer already used `$HOME/.local/bin`. For a source build:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/crier bin/crier-mcp "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"    # every shell
```

For a **system-wide** install (any user, and what §6.1's unit expects):

```sh
sudo install -m 0755 bin/crier bin/crier-mcp /usr/local/bin/
```

Confirm what you installed, and that it is the thing you think it is:

```sh
crier -version          # the build's own identity — a tag, or a commit
crier -help             # the full flag + environment surface
```

`crier -version` is the only trustworthy answer to "which build is this?" — a
file name and a release page can both name a version the binary does not carry.

---

## 6. Run it as a service

Two unit files ship in [`deploy/`](deploy/): a **system** unit for a whole box,
and a **per-user** unit for a no-root install. Both are verified; pick one.

### 6.1 The system unit — `deploy/crier.service`

One service for the whole machine, its own account, state in `/var/lib/crier`.
Needs root:

```sh
# 1. a service account (the unit's User=/Group= name it)
sudo useradd --system --home-dir /var/lib/crier --create-home \
     --shell /usr/sbin/nologin crier
sudo install -m 0755 bin/crier /usr/local/bin/crier

# 2. configuration (optional — the file is optional in the unit, and crier boots
#    on its defaults without it). Copy the documented surface and edit it:
sudo install -d -m 0750 -o crier -g crier /etc/crier
sudo install -m 0640 -o crier -g crier .env.example /etc/crier/crier.env
sudoedit /etc/crier/crier.env      # set CRIER_PORT, and see §6.3 for the store

# 3. install, enable, start
sudo install -m 0644 deploy/crier.service /etc/systemd/system/crier.service
sudo systemctl daemon-reload
sudo systemctl enable --now crier.service
```

### 6.2 The per-user unit — `deploy/crier.user.service`

For the `$HOME/.local/bin` install and no root. It reads
`~/.config/crier/crier.env` (optional), keeps state in `$XDG_STATE_HOME/crier`,
and needs **no** `User=` account.

```sh
mkdir -p ~/.config/crier ~/.config/systemd/user
cp deploy/crier.user.service ~/.config/systemd/user/crier.service
cat > ~/.config/crier/crier.env <<'EOF'
CRIER_PORT=8767
CR_SESSION_BACKEND=sqlite
CR_SQLITE_PATH=crier.db
EOF
systemctl --user daemon-reload
systemctl --user enable --now crier.service
loginctl enable-linger "$USER"     # keep it running when you are not logged in
```

`loginctl enable-linger` is the difference between "runs while I am logged in"
and "runs". Without it the user manager stops the service on your last logout.

### 6.3 Choose your durable store before you start

**Read this before you set an env file, or you will get a server that silently
keeps nothing.** Measured on a fresh boot with only `CR_SESSION_BACKEND=sqlite`
set, the server logs:

```
level=WARN msg="registry backend is the in-memory store — DEMO-ONLY and non-durable:
agents and undelivered inbox messages are lost when this process exits.
Set CR_DATABASE_URL (docker compose up -d postgres) for the durable inbox the docs document"
```

There are two different stores and only one of them is what the docs mean by
"durable":

| store | selected by | durable? |
| --- | --- | --- |
| **registry + inboxes** | `CR_DATABASE_URL` (PostgreSQL) | yes — this is the durable path the README documents |
| sessions / chat | `CR_SESSION_BACKEND=sqlite` (+ `CR_SQLITE_PATH`) | yes, for that store only |
| everything, with neither set | — | **no**: process-lifetime, lost on restart |

So: for a durable box, run PostgreSQL (`docker compose up -d postgres`, or any
PostgreSQL 14+ server) and point `CR_DATABASE_URL` at it. Setting only the
sqlite session store does not make the registry durable, and a relative
`CR_SQLITE_PATH=crier.db` resolves against the unit's `WorkingDirectory` — which
is `$XDG_STATE_HOME/crier` for the user unit and `/var/lib/crier` for the system
unit (verified: `crier.db` appeared exactly there).

### 6.4 What the units do

Both units hand the process to systemd the same way, and both were verified
against a live `systemd --user` manager (state of that verification is stated
honestly below):

```sh
systemctl --user status crier.service     # or: systemctl status crier
curl -s localhost:8767/health             # {"status":"ok"}
curl -s localhost:8767/version            # the build identity the server runs
crier -stop -pidfile /run/user/1000/crier/crier.pid   # the user unit
```

- **`Restart=on-failure`** — verified by `kill -9` of the main process: systemd
  started a new one and the replacement answered `/health`.
- **Graceful stop** — `systemctl stop` sends `SIGTERM`; crier drains and stops
  (`KillSignal=SIGTERM`, `TimeoutStopSec=20s`). It does **not** get killed
  abruptly. The `-pidfile` flag is what pairs a running server with
  `crier -stop` / `make stop`, so the unit always passes one.
- **`KillMode=control-group`** — the manager signals the whole cgroup, so nothing
  the server spawns can reparent to the user manager and outlive the unit. That
  failure mode is measured in this repo (73 → 278 orphaned processes, loadavg
  220; [docs/load-reproduction.md](docs/load-reproduction.md)).
- **Logs go to a file**, not the journal (`StandardOutput=append:` the unit sets),
  because a user journal is not guaranteed to capture anything — on the host
  these units were verified on, `journalctl --user -u <unit>` printed
  `No entries` even for a trivial transient unit. Read the file:
  `$XDG_STATE_HOME/crier/crier.log` (user) or `/var/lib/crier/crier.log` (system).
  This matches the fleet's own production unit shape.
- **The pidfile lives inside the unit's `RuntimeDirectory`** (`%t/crier/`,
  `/run/crier`), which systemd deletes on stop — so a `SIGKILL`ed run cannot
  leave a stale pidfile behind for the next `crier -stop` to trip over. (Parking
  it at the runtime *root* does leave one: measured.)

**Verification state, stated plainly.** The **user** unit was started, probed,
`SIGKILL`ed, restarted, stopped, and checked for leftovers under a live
`systemd --user` manager — all of the above is measured, not asserted. The
**system** unit passes `systemd-analyze verify` with zero findings once a binary
exists at its `ExecStart` path (before placement the only finding is that path);
it was **not** started on a live system here, because that needs root on the
host. Treat its behavior as inherited from the user unit's measurements and the
shared settings, and read `systemctl status crier` after your first start.

---

## 7. Verify the install — no assumptions

Run these in order on the box you installed onto. Each one answers a question
the previous one cannot.

```sh
# 1. the binaries exist, are executable, and are where you think
ls -l "$HOME/.local/bin/crier" "$HOME/.local/bin/crier-mcp"

# 2. they RUN and report their own identity (a wrong-arch or truncated
#    download is not caught by a checksum — the file is the file that was
#    published; running it is the only proof it is for THIS machine)
crier -version
crier-mcp --version

# 3. the server binds and answers
crier -port 8767 -pidfile "$HOME/crier.pid" &      # add your CR_* env first
sleep 1
curl -s localhost:8767/health      # {"status":"ok"}
curl -s localhost:8767/version     # the same build identity as step 2

# 4. it stops the supported way, and the process is really gone
crier -stop -pidfile "$HOME/crier.pid"
pgrep -af 'bin/crier' || echo "no crier process left"
```

`/health` and `/version` are deliberately unauthenticated (see
`internal/middleware/auth.go`), so they work before any agent credentials exist.
Everything else needs an agent identity — start with
[TESTERS.md](TESTERS.md) §2.

---

## 8. Troubleshooting — the failures actually seen

| symptom | cause | fix |
| --- | --- | --- |
| `install.sh: FAIL: download failed … SHA256SUMS` (HTTP 404) | the release has no assets published | Path B (§4), or `--version <tag>` if only the latest is missing |
| `curl: command not found` on the documented one-liner | a fresh box has no `curl` **and no `wget`** | §2.1 |
| checksum mismatch | a truncated download, or you are fetching from a mirror that changed the file | retry; if it persists the mirror is not the release |
| `crier: -stop needs a pidfile` | you started it without `-pidfile`/`CR_PIDFILE` | always pass `-pidfile`; §6's units do |
| `warning: both GOPATH and GOROOT are the same directory` | the Go tarball was unpacked into `$HOME` | §2.2 |
| server boots but nothing survives a restart | neither durable store is configured — the registry is in-memory | §6.3: set `CR_DATABASE_URL` |
| `CR_SESSION_BACKEND=sqlite` seems to do nothing | you are running a build from the `v0.1.0-rc2` tag, which predates it | §4: clone the default branch |
| a service that logs nothing, `journalctl` empty | the user journal is not capturing on that host | §6.4: the units log to a file; read it |

---

## 9. Reproducing this verification

Every measurement in this file comes from a container that starts with **nothing
installed**, so nothing here depends on a developer machine's state:

```sh
# a bare box, no Go, no curl, no git
docker run -it debian:13 bash

# inside — confirm §0 for yourself before installing anything
for t in curl wget tar gzip sha256sum git make go; do
  command -v "$t" >/dev/null 2>&1 && echo "present: $t" || echo "ABSENT : $t"
done
```

For Path A against a *served* release tree (which is how the prebuilt path was
verified end-to-end, since the public release has no assets), build the asset set
and serve it in the shape GitHub uses:

```sh
make release-artifacts VERSION=v0.1.0-rc2 RELEASE_OUTDIR=/tmp/dist
mkdir -p /tmp/release/latest/download /tmp/release/download/v0.1.0-rc2
cp /tmp/dist/* /tmp/release/latest/download/
cp /tmp/dist/* /tmp/release/download/v0.1.0-rc2/
(cd /tmp/release && python3 -m http.server 18911)
# then, on the clean box:
#   sh install.sh --version v0.1.0-rc2 --base-url http://<host>:18911
```

The repo's own acceptance drive for the front door does all of this, plus two
negative controls, with no network and no docker:

```sh
make install-path-selftest
```

---

## 10. What this guide does *not* verify

Stated so nobody reads more into it than was measured:

- **The public release cannot install anything today** (§3). The prebuilt path is
  verified against a locally served asset set; whether it works against GitHub
  depends on a release manager publishing the assets.
- **The system unit was not started on a live system** (§6.4) — only structurally
  verified. The user unit was run in full.
- **PostgreSQL-backed durability was not exercised here.** The guide tells you to
  set `CR_DATABASE_URL` (§6.3) because the server itself says the in-memory
  registry is non-durable; the durable path is covered by
  [docs/integration-guide.md](docs/integration-guide.md) and
  `specs/ci-003b-postgresql-persistence.md`, not by this file.
- **No `deploy/` file is covered by a repo gate.** `make shell-yaml-check` reads
  shell scripts and workflows, `make make-docker-check` reads Makefiles and
  Dockerfiles — neither reads a `.service` file, so the units are verified by the
  commands recorded above, not by CI.
