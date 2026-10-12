# Install leg on a fresh box — 2026-10-11 — prebuilt path + from-source path, both measured

Angle (the INSTALL-crier leg, DF-CRIER-307): the *documented* install paths,
exercised on boxes that start with **nothing installed**. The 2026-10-10 run
took the install leg too, but on one bunker box and with a **tar-of-tree
substitute** for the clone, recorded as a substitute — never as a clean-machine
clone proof. This run replaces the substitute with a real clean-box install of
each documented path, and records what the documents get wrong.

## Bare-box baseline (the thing every claim below is relative to)

Official `debian:13` (trixie) image, nothing installed:

```
ABSENT : curl, wget, git, make, go, python3, xxd, openssl, sudo, ss
PRESENT: sha256sum, tar, gzip, useradd, getent
```

Two follow-ups that decide the whole install story:

1. A fresh box has **no fetch tool at all** — not `curl`, not `wget`. The
   documented one-liner (`curl … | sh`) cannot start, and TESTERS.md §1 called
   that the "fastest path".
2. The **only** release is `v0.1.0-rc2`, and `gh release view v0.1.0-rc2 --json
   assets` returns `{"assets":[]}`. `…/releases/latest/download/SHA256SUMS`
   answers **HTTP 404**.

## Path A — prebuilt binaries: verified, and blocked on the public release

The documented one-liner, run on a clean `env -i` box, verbatim:

```
==> crier latest installer (CR-FEAT-028) — no Go toolchain needed
    platform : linux/amd64 (uname: Linux/x86_64)
    source   : https://github.com/crier-dev/crier/releases
==> download
curl: (22) The requested URL returned error: 404
install.sh: FAIL: download failed: https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
EXIT=1        # and the install directory is EMPTY
```

That is the installer's fail-closed design working — but it means **no user can
install crier from the public release today**. The fix is a release-manager
action (`make release-upload VERSION=<tag>`; docs/releases.md §4), not code.

To prove the mechanism itself rather than the missing assets, the asset set was
built (`make release-artifacts`) and served as a release tree
(`/latest/download/`, `/download/v0.1.0-rc2/`), then installed on a bare
`debian:13` box as an **unprivileged user** (`tester`, uid 1000, no root, no Go):

```
  running as : tester (HOME=/home/tester)      go on PATH : ABSENT
==> verify against SHA256SUMS
    crier_linux_amd64       sha256 d3250b63… (matches SHA256SUMS)
    crier-mcp_linux_amd64   sha256 2dd691fc… (matches SHA256SUMS)
==> install
    crier     -> /home/tester/.local/bin/crier        (22,020,258 B, 0755)
    crier-mcp -> /home/tester/.local/bin/crier-mcp    (12,009,634 B, 0755)
install.sh: OK — crier and crier-mcp are installed in /home/tester/.local/bin
  install exit=0
  -version : crier v0.1.0-rc2-d6a8e2a9-dirty
  boot     : yes
  /health  : {"status":"ok"}
  /version : {"version":"0.1.0-rc2","commit":"d6a8e2a9",…}
  sqlite db: 106496 bytes   (CR_SESSION_BACKEND=sqlite, CR_SQLITE_PATH=$HOME/crier.db)
  crier: stopping pid 3150 (SIGTERM)  /  crier: pid 3150 stopped     (graceful -stop)
```

Both documented forms work: the pinned one (`--version v0.1.0-rc2`) and the
default one (`latest`). The repo's own acceptance drive for all of this
(`make install-path-selftest`) passes: asset set → served tree → clean box with a
**failing `go` shim that was never invoked** → installed binary runs → tampered
artifact and manifest-with-no-entry both refused.

Aside, measured: the release publishes **bare static executables** plus
`install.sh` + `SHA256SUMS`. README called them "archives"; nothing is
tarred or zipped. Corrected.

## Path B — from source: works, but the README's tag pin silently changes the binary

Two checkouts, same box, same commands, one variable (`--branch`):

| checkout | `.go` files mentioning `CR_SESSION_BACKEND` | `CR_SESSION_BACKEND=sqlite` | sqlite file |
| --- | --- | --- | --- |
| `--branch v0.1.0-rc2` (`c34a6c2`, 2026-09-21) — **README's recipe** | **0** | **silently ignored** | **not created** |
| default branch (`d6a8e2a`) — TESTERS.md's recipe | 12 | honored | 106,496 B |

The tag-pinned install therefore produces a server that **ignores a documented
env var without a word** — an unknown variable is a no-op, not an error. It also
boots *quieter*: on `main` the same start logs
`level=WARN msg="registry backend is the in-memory store — DEMO-ONLY and
non-durable: agents and undelivered inbox messages are lost when this process
exits…"`, which the rc2 binary does not.

Everything else in the from-source path is clean on a bare box, as an
unprivileged user:

```
  B2  no-root Go install: curl go1.26.6.linux-amd64.tar.gz -> 66,890,545 B
      tar -C "$HOME/sdk" -xzf … -> rc 0 -> $HOME/sdk/go
      go version go1.26.6 linux/amd64
      GOPATH=/home/tester/go  GOROOT=/home/tester/sdk/go     (no collision warning)
  B3  git clone https://github.com/crier-dev/crier.git -> rc 0 (public; HEAD d6a8e2a)
  B4  make build build-mcp -> rc 0, bin/crier 31,628,333 B     (11s)
  B5  install -m 0755 bin/crier bin/crier-mcp "$HOME/.local/bin/" -> ok
  B6  boot -> /health {"status":"ok"} ; CR_SESSION_BACKEND=sqlite HONORED
      (sqlite 106,496 B) ; ./bin/crier -stop -> "pid stopped"
```

**Premise corrected.** The 2026-10-10 diagnostics recorded "documented gitlab
https clone FAILED on the fresh box (private repo)". The *documented* URL is
`https://github.com/crier-dev/crier.git` — a **public** repository — and it
clones clean from a bare box. The failing clone was the gitlab remote, which no
documented recipe names. The substitute was not needed for the documented path.

## The systemd units — a live supervisor, not a syntax check

No unit existed in the repo. Two are added (`deploy/crier.service`,
`deploy/crier.user.service`) and the per-user one was **run under the live
`systemd --user` manager** on this host:

```
  structural verify : systemd-analyze --user verify -> 0 findings about the unit
  start             : active, MainPID 2363815
  pidfile           : /run/user/1000/crier/crier.pid      (inside RuntimeDirectory)
  /health           : {"status":"ok"}      /version : {"version":"0.1.0-rc2",…}
  RELATIVE CR_SQLITE_PATH=crier.db landed at /home/kara/.config/crier/crier.db
  Restart=on-failure: kill -9 -> MainPID 2366306 (changed) -> /health {"status":"ok"}
  systemctl stop    : "shutdown signal received" signal=terminated  (graceful)
  leftovers         : runtime dir gone · port free · 0 stragglers
```

Three things the live run corrected, all now fixed in the shipped unit:

- **`-pidfile %t/crier.pid` left a stale pidfile at the runtime root** after a
  SIGKILLed run. The pidfile belongs *inside* `RuntimeDirectory` (`%t/crier/`),
  which the manager deletes on stop. Changed, re-proved.
- **`journalctl --user -u <unit>` captured nothing** — not even for a trivial
  transient unit, so this host's user journal is not a log sink. The units now
  log to a file (`StandardOutput=append:`), matching the fleet's own production
  unit (`crier-fleet.service`).
- `StateDirectory=`/`WorkingDirectory=%S/crier` and the **relative
  `CR_SQLITE_PATH`** resolution were verified rather than assumed (the db landed
  at `%S/crier/crier.db`, created 106,496 B).

The **system** unit passes `systemd-analyze verify` with **exit 0 and zero
findings** once a binary exists at its `ExecStart` path (with the path
substituted for an existing binary — the only difference from the shipped file);
it was **not started on a live system**, which needs root on the host. That
boundary is stated in INSTALL.md §10.

## Files

- `INSTALL.md` — the fresh-box guide: bare-box baseline, prerequisite chain (with
  the no-root Go recipe), both paths, PATH placement, both units, a verification
  checklist, the failures actually seen, and the docker recipe to reproduce all
  of it.
- `deploy/crier.service`, `deploy/crier.user.service` — the units.
- `README.md`, `TESTERS.md` — the "archives" wording, the tag-pin caveat, and
  TESTERS.md §1's unqualified "fastest path" claim.
