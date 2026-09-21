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
5. creates the annotated tag `VERSION` with the message `crier VERSION`,
6. prints the exact `git push` command for both remotes — `origin`, plus
   `gitlab` when that remote is configured.

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

## 4. Create the GitHub Release object

The pushed tag is not the whole release surface: GitHub renders the Releases
page from Release objects, not tags. A tag with no Release object is invisible
on the Releases page, has no release notes, and does not appear in
`gh release list`. `make release` creates only the tag, so this half of the
publish step is also done by hand, right after the tag push (v0.1.0-rc2 shipped
tag-only until this step was noticed missing):

```bash
gh release create v0.1.0-rc2 --repo crier-dev/crier \
  --title "v0.1.0-rc2" \
  --notes-file /tmp/release-notes-v0.1.0-rc2.md
```

The notes file is the version's own CHANGELOG section, plus a compare link —
rc2's is
`https://github.com/crier-dev/crier/compare/v0.1.0-rc1...v0.1.0-rc2`.
Verify the Release exists and names the tag:

```bash
gh release view v0.1.0-rc2 --repo crier-dev/crier
```

## Gate requirements

The release target enforces the minimum battery on the commit being tagged:

- `make build` — compiles `bin/crier` with the version stamped into the build
  identity, so the tag and `./bin/crier -version` can never disagree.
- `make lint` — `go vet ./...`.
- `make test-short` — the unit suite (`go test -short ./...`, no Docker).

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
