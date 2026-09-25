# Cutting a crier release

A crier release is one annotated tag on a commit of `main` plus its GitHub
Release object. The tag is created
LOCALLY by `make release`, and published by hand once CI is green on the commit
it names. Nothing in this repo pushes a tag for you.

The changelog lives in [CHANGELOG.md](../CHANGELOG.md) at the repo root
([Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format).

## 1. Write the changelog entry first

The tag point is the commit that contains the entry — a tag cut first can never
contain its own changelog entry. So, before tagging:

1. Move everything under `## [Unreleased]` in `CHANGELOG.md` into a new heading
   for the version being cut, dated in `YYYY-MM-DD`:
   `## [0.1.0-rc2] - 2026-09-21`.
2. Leave a fresh, empty `## [Unreleased]` heading at the top.
3. Update the link references at the bottom of the file.
4. Commit that (with whatever else the release contains) — `make release`
   refuses a dirty tree, and this is the reason it does.

## 2. Cut the tag

```bash
make release VERSION=v0.1.0-rc2
```

`VERSION` must be passed explicitly. The target refuses the default
`git describe` value, anything that is not a `vX.Y.Z` (optionally suffixed like
`-rc2`) tag, a dirty working tree, any branch other than `main`, and a version
whose tag already exists. So the version can only ever come from the caller.

The target then:

1. verifies the working tree is clean (`git status --porcelain` empty),
2. verifies the current branch is `main`,
3. verifies the tag does not exist yet,
4. runs the gates — `make build && make lint && make test-short` — echoing each
   one before it runs, and stopping before any tag if one fails,
5. builds the release asset set for this exact commit — `make release-artifacts
   VERSION=<tag>` (CR-FEAT-028): `crier` and `crier-mcp` cross-compiled for
   linux/amd64, linux/arm64 and darwin/arm64 into `dist/`, the installer
   (`scripts/install.sh`) copied in, and `SHA256SUMS` written and re-verified.
   The assets are built HERE, from the commit being tagged, so a published
   binary can always be tied to the tag that names it — and `dist/` is
   gitignored build output, so it never dirties the tree the next release
   checks,
6. creates the annotated tag `VERSION` with the message `crier VERSION`,
7. prints the exact `git push` command for both remotes — `origin`, plus
   `gitlab` when that remote is configured — and the `make release-upload`
   command that attaches the assets (step 4 below).

It never pushes. That is deliberate: the tag must only be published after CI is
green on `main`.

## 3. Publish the tag

Once CI on the tagged commit is green, push it by hand:

```bash
git push origin v0.1.0-rc2
git push gitlab v0.1.0-rc2   # only if the gitlab remote is configured
```

A pushed tag is effectively immutable for anyone consuming it, so agree on the
version before publishing. If the commit is not on `origin/main` yet, push
`main` and let CI go green on it first — `make release` prints a warning when
the commit it tagged is not an ancestor of `origin/main` (as of the last
`git fetch`).

## 4. Publish the Release object — and its assets

The pushed tag is not the whole release surface: GitHub renders the Releases
page from Release objects, not tags. A tag with no Release object is invisible
on the Releases page, has no release notes, and does not appear in
`gh release list`. `make release` creates only the tag, so this half of the
publish step is done by hand, right after the tag push.

**Attach the asset set in the same command (CR-FEAT-028).** This is what puts
the binaries on the page. v0.1.0-rc2 shipped with `assets: []` — the Release
object existed and carried notes only, so the only way in was `git clone` plus a
Go toolchain plus `make build`, which is the tester-funnel problem the external
hands-on review filed (`DISPATCH · CRI-001`). `make release` built the asset set
for the tag it cut in step 5 above; this publishes it:

```bash
make release-upload VERSION=v0.1.0-rc3
```

`DRY_RUN=1` prints the exact `gh` commands and publishes nothing:

```bash
make release-upload VERSION=v0.1.0-rc3 DRY_RUN=1
```

`release-upload` (`scripts/release-upload.sh`) verifies `dist/SHA256SUMS`
first, refuses a set that is missing any release target or the installer,
refuses assets built from a commit other than the tag's (pin `ALLOW_TAG_DRIFT=1`
only when you know why they differ), and then either CREATES the Release object
— title, release notes from the version's own `CHANGELOG.md` section, compare
link against the previous tag — or uploads onto the one that already exists. It
never pushes or moves a git tag: §3 is still a hand command.

Two things it will not decide for you:

- **`--prerelease` (`PRERELEASE=1`)**: a prerelease is EXCLUDED from
  `/releases/latest`, and that is the URL `scripts/install.sh` resolves for a
  default (unpinned) install. Mark an rc prerelease only if you mean to close
  that default path — a tester then has to pass `--version <tag>` — and note
  that the rc's cut so far are published as ordinary releases.
- **overwriting an asset**: re-running against an existing Release object
  without `CLOBBER=1` fails on the name collision rather than silently
  replacing a binary somebody may already have downloaded.

The hand-written form below still works if the notes must be something other
than the changelog section (pass `--notes-file <path>` to `release-upload` for
the same effect, with the assets):

```bash
gh release create v0.1.0-rc3 --repo crier-dev/crier \
  --title "v0.1.0-rc3" \
  --notes-file /tmp/release-notes-v0.1.0-rc3.md
```

Verify that the Release exists, names the tag, **and carries the assets** — a
release with the object and no assets is the defect this step closes, so the
asset list is part of the verification, not an extra:

```bash
gh release view v0.1.0-rc3 --repo crier-dev/crier --json assets --jq '.assets[].name'
```

The acceptance drive for the whole front door — the asset set builds, installs
from a served release tree into a clean box with a failing `go` shim on `PATH`,
the installed binary runs, and both unverified-download shapes are refused — is:

```bash
make install-path-selftest
```

It needs no network, no docker and no keys; the publish command itself is driven
against a `gh` PATH shim there, so the real upload is the one step this
repo cannot test without publishing something.

## Gate requirements

The release target enforces the minimum battery on the commit being tagged:

- `make build` — compiles `bin/crier` with the version stamped into the build
  identity, so the tag and `./bin/crier -version` can never disagree.
- `make lint` — `go vet ./...`.
- `make test-short` — the unit suite (`go test -short ./...`, no Docker).
- `make release-artifacts` — the release asset set, built for the tag being cut
  from the commit being tagged (CR-FEAT-028). `make install-path-selftest` is
  its acceptance drive: the set is built, served as a release tree, installed
  into a clean box that has a FAILING `go` shim on `PATH`, and the installed
  binary is run — plus the tampered-artifact and missing-manifest refusals.

Before the tag is published, the full CI on the same commit should be green as
well: `.github/workflows/ci.yml` runs the build, vet, the short suite, the
coverage gate (`make coverage-check`), the PostgreSQL-backed registry
integration tests, the OpenAPI/docs-claims checks, and the guard arms the
Tier-1 battery does not cover (gofmt, shell/YAML, Makefile/Dockerfile, MCP
launcher stdout). The committed E2E battery (`scripts/e2e-battery.sh`) is the
widest local gate.

## What not to do

- **Do not re-tag a published version.** Two commits under one version number
  is unrecoverable for consumers — cut a new `-rc` (or patch) instead.
- **Do not force-push a tag**, and do not move an existing tag to a new commit.
- **Do not cut a tag from a branch commit or a dirty tree** — the gates and the
  changelog entry are exactly what makes the tag point meaningful.
- **Do not push the tag before CI is green** on the commit it names.
