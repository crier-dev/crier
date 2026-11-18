# RELEASE-018 — `command -v crier` resolves to one release-identifiable binary

Host-level fix, executed 2026-11-18T00:03–00:06Z. No repo code change; evidence only.

## Defect (verified before the fix)

Two byte-identical stale builds shadowed each other on PATH:

    /home/kara/.local/bin/crier   33963175 bytes  Nov  4 03:23  kara:kara
    /usr/local/bin/crier          33963175 bytes  Nov  4 03:23  root:root
    sha256 28187855186833623c19fe3d719f5a5180e37abeceddb0cf0906f10908002b32 (both)

`go version -m` on both reported `mod github.com/crier-dev/crier (devel)` with no
`vcs.revision` and no `vcs.modified`; `crier -version` printed `crier dev`. Neither
copy could be tied to a release or a commit. Proven from the preserved backups in
`before-state.txt`.

## Fix

Installed the published goreleaser asset for the newest release (`v0.6.1`,
2026-11-11) at `/usr/local/bin/crier` and removed the stray user-local copy.

- Source: GitHub release asset `crier_linux_amd64`, 29130914 bytes
- Manifest check: sha256 `c298a2e7c9ad413d4d1ea8163801cd55159dbfbf3ca7bc0b581053a20283fdd3`
  matches the release's published `SHA256SUMS` (`release-asset.txt`)
- Stamp check: `vcs.revision=d7a68d01e85e86aa947b9a7c7a3fa2ad3c04e8f9`, which equals
  `git rev-parse v0.6.1^{commit}` in this repo, `vcs.modified=false`
- Install was staged to `/usr/local/bin/.crier.new`, verified running
  (`crier v0.6.1-d7a68d01`), then renamed into place — the host never held zero
  working copies
- `/usr/local/bin` was chosen over `~/.local/bin` because `~/.local/bin` is listed
  three times in this fleet shell's PATH; a binary there would make the AC-1 loop
  print three lines. `/usr/local/bin` appears exactly once in every PATH variant
  measured, and is visible to all users/services.
- Removed: `/home/kara/.local/bin/crier`. Not destroyed — both pre-fix builds are
  preserved under `/home/kara/.local/share/crier-binary-backups/release-018-20261118T000352Z/`
  with sha256 recorded.

## Acceptance criteria (verbatim readings)

AC-1 — exactly one path, both PATH environments:

    # this session's PATH (which lists ~/.local/bin three times)
    /usr/local/bin/crier
    count: 1

    # clean login shell: env -i HOME=$HOME bash --login
    /usr/local/bin/crier

AC-2 — release-identifiable build metadata:

    /usr/local/bin/crier
    	path	github.com/crier-dev/crier/cmd/server
    	mod	github.com/crier-dev/crier	v0.6.1
    	build	vcs.revision=d7a68d01e85e86aa947b9a7c7a3fa2ad3c04e8f9
    	build	vcs.time=2026-11-11T21:16:33Z
    	build	vcs.modified=false

AC-3 — version invocation (`crier --help` documents `-version`):

    crier v0.6.1-d7a68d01

## Supervised unit untouched

`crier-fleet.service` is a **user** unit (`systemctl --user`), not a system unit —
`systemctl status crier-fleet` as a system command reports "could not be found".
Its `ExecStart` is `/home/kara/crier/bin/crier -pidfile /home/kara/crier-fleet/crier.pid`,
which was not modified, not restarted, and not replaced.

| | pre-fix | post-fix |
|---|---|---|
| ActiveState | active (running) | active (running) |
| InvocationID | 1bbabecf8d5242e0952d39fe30748af8 | 1bbabecf8d5242e0952d39fe30748af8 |
| MainPID | 15357 | 15357 |
| ExecMainStartTimestamp | Tue 2026-11-17 15:54:09 -05 | Tue 2026-11-17 15:54:09 -05 |
| NRestarts | 2 | 2 |
| ExecStart binary sha256 | af60174f...e87427 | af60174f...e87427 |

Live bus still serving on the unit's port 8767: `http=200 {"status":"ok"}`, with
`ss` attributing the listener to `pid=15357`.

## Notes / scope observations

- Three Docker containers (`crierdog-crier-1`, `qacrier1625-crier-1`,
  `qa-crier-repro-crier-1`) run `/usr/local/bin/crier` **inside their own images**
  (shas 830f8c6b…, 12910f7e…, d844df9d…, versions v0.6.1-28-g1774dcd7-dirty,
  v0.3.0-71-g465df7b4, v0.2.0-457-gb5b66f1c). `docker inspect .Mounts` is `[]` for
  all three, so the host path is not bind-mounted into them; replacing it does not
  affect those containers or any pinned QA fixture.
- A second crier listens on :18767 (Docker-published, distinct from the unit's
  :8767). Unrelated to this row; noted so a future reader does not confuse the two.
- `~/.local/bin` being listed three times in the fleet shell's PATH is an
  environment wart, not a crier defect; the fix was made PATH-shape-independent.

## Files

- `before-state.txt` — pre-fix defect proven from preserved backups
- `after-acceptance.txt` — AC-1/2/3 in this session's PATH + installed identity
- `after-acceptance-clean-login.txt` — AC-1 in a profile-derived login shell
- `release-asset.txt` — asset manifest verification + build stamps + tag match
- `service-state.txt` — unit definition, pre/post-identical state, live health
