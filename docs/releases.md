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
4. **Update the README Quick Start pin to this version.** `README.md` names the
   release three times in its Quick Start install section, and all three move
   together: the heading (`### Install prebuilt binaries (vX.Y.Z release
   assets)`), the sentence `Release \`vX.Y.Z\` publishes 8 assets, the complete
   set: ...`, and the pinned install example (`sh -s -- --version vX.Y.Z`). The
   unpinned example needs no edit — the installer resolves
   `/releases/latest` — but the PIN is hand-maintained prose, so it drifts the
   moment a tag is cut. It did: after the v0.3.0 cut the front door still
   pointed at v0.2.0 (RELEASE-013), which sends a reader who wants a fixed
   version to the previous release. This step is a CUT-TIME duty rather than a
   gate: `make docs-check` cannot assert "README pin == newest tag" on a real
   clone — the CI checkout is shallow and fetches no tags, so the assertion
   would either pass vacuously or fail on every run — and a doc claim anchored
   on the version string alone would just have to be edited in the same commit.
   Moving it with the changelog entry is the lock: one commit carries both, and
   the tag point can never contain one without the other.
5. Commit that (with whatever else the release contains) — `make release`
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
`gh release list`.

**The tag push publishes by itself.** Pushing the tag triggers
`.github/workflows/release.yml`, which builds the asset set from the tagged
commit and attaches it to the Release object — the publish step is no longer
a hand duty that can be forgotten (that is exactly how v0.1.0-rc2 shipped with
`assets: []`). The command below is the SAME publisher run by hand: use it to
publish before pushing the tag, to repair a half-failed publish, or to
re-verify what landed. Order no longer matters: whichever of the two
publishers runs first creates the Release with its assets, and the second one
converges against it — an identical set is a no-op, and a rebuild of the same
commit is ADOPTED by build identity rather than red-ing on byte drift (see
the reconciliation notes at the end of this section).

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
link against the previous tag — or reconciles against the one that already
exists: identical content is skipped, a same-commit rebuild is adopted by
build identity, absent names are uploaded, and genuinely different content is
refused. It never pushes or moves a git tag: §3 is still a hand command.

`SHA256SUMS` is uploaded **with** the set but is never listed **inside** it (a
manifest that hashed itself could never be verified), so the set/manifest
comparison every publish path runs allows exactly one asset the manifest does not
name — the manifest itself — while still refusing any *other* unknown asset and
any manifest asset that is not attached. `make release-upload-selftest` proves
that rule on both implementations (this script and the `release.yml` verification
step), with a neuter proof that removing the exemption reproduces the v0.2.0 red
(`RELEASE-010`).

Two things it will not decide for you:

- **`--prerelease` (`PRERELEASE=1`)**: a prerelease is EXCLUDED from
  `/releases/latest`, and that is the URL `scripts/install.sh` resolves for a
  default (unpinned) install. Mark an rc prerelease only if you mean to close
  that default path — a tester then has to pass `--version <tag>` — and note
  that the rc's cut so far are published as ordinary releases.
- **overwriting an asset**: a re-publish RECONCILES with what the Release already
  carries (`RELEASE-012`, extended by INT-CI-033). An asset already attached with
  **identical** content is skipped, and a set that is entirely identical exits 0
  reporting that it converged — so re-running a failed publish is safe. A
  differing BINARY whose bytes carry the same build identity as this build (the
  same embedded VERSION token and the same single 40-hex commit candidate, read
  from the artifact bytes, with no `vcs.modified=true` on the attached side) and
  whose byte divergence stays within the metadata bound (below) is
  **adopted**: the published set is kept exactly as it is, nothing is uploaded,
  and the difference is reported as the BUILD_TIME metadata a rebuild always
  stamps in — never as a conflict. `SHA256SUMS` is adopted only as the manifest
  of a fully-adopted set; `install.sh` carries no stamp and must converge
  byte-identically. Anything else — a different commit, a dirty-tree build, an
  unreadable or ambiguous stamp, a differing non-binary, a body diverging
  beyond the metadata bound — is refused unless `CLOBBER=1`, because a binary
  somebody may already have downloaded is never swapped out silently (and
  adoption never swaps anything at all). Names that
  are absent are uploaded, so a publish that half-failed is repaired by
  re-running it rather than re-done from scratch.

The adoption is bounded by a metadata-only heuristic, not a proof of sameness
(INT-CI-034). After the identity grounds pass, the publisher measures the
divergence between the local asset and the attached copy — the size delta and
the number of differing byte offsets — and refuses the adoption beyond
`RELEASE_ADOPT_MAX_SIZE_DELTA` (default `0`) or
`RELEASE_ADOPT_MAX_DIFF_OFFSETS` (default `4096`), quoting the measured
numbers in the refusal; a converged (adopted) run reports its own measured
divergence on every `ADOPTED` line. The defaults derive from the measured
spread of two builds of the SAME commit differing only in `BUILD_TIME`:
sizes identical, and exactly 63 differing byte offsets on linux/amd64, 63 on
linux/arm64 and 153 on darwin/arm64 (the Mach-O ad-hoc signature covers the
stamped bytes) — the offsets bound is more than ten times the largest
measured value, and the size bound is zero because the measured rebuild is
size-identical. The bound is the discriminator that matters on the toolchain
crier builds with: go1.26.10 records no `vcs.revision` and no `vcs.modified`
at all, so the `vcs.modified=true` dirty-tree gate cannot fire there and a
rebuild from a modified tree carries the same stamps as a clean one — only
the byte bound separates the two on that toolchain. It is a heuristic, not a
proof: a body crafted to diverge by less than the bound would still be
adopted, and `CLOBBER=1` remains the only deliberate replacement path either
way.

That reconciliation is why the ORDER of the two publishers no longer matters —
and why, since INT-CI-033, neither does the rebuild the second publisher does.
Before `RELEASE-012` the order mattered outright: v0.3.0's manual publish landed
~40 s before the tag workflow, and the workflow — finding every name already
attached — died with `asset under the same name already exists` on a release
whose 8 assets were all present and correct (run `42246232196`). RELEASE-012
turned that collision into a convergence. The three historical reds on releases
that had actually shipped correctly are the evidence this section's rules close:

- run `42246232196` (v0.3.0) — the name collision above, closed by measuring
  content and skipping what is identical;
- run `39539975561` (v0.2.0) — `Release v0.2.0 carries asset(s) SHA256SUMS does
  not know:SHA256SUMS`, the set-comparison bug (the manifest cannot name
  itself), closed by the SHA256SUMS exemption (`RELEASE-010`);
- run `42422236749` (v0.4.0) — every asset "attached with DIFFERENT content"
  because the workflow's rebuild carried a later BUILD_TIME stamp than the
  manual publish's build, closed by the build-identity adoption above.

Content identity is always MEASURED, never assumed: when GitHub reports no
digest for an attached asset, the publisher downloads and hashes it, and a
size mismatch is already a refusal. Build identity is measured the same way —
from the artifact bytes, never inferred from the tag name.

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

The publish-time set check (a correct set carrying `SHA256SUMS` is accepted, an
unknown asset is still refused, and the exemption is proven load-bearing) and the
publish's idempotence (an already-attached identical set converges with zero
uploads, different content is refused by name unless `CLOBBER=1`, a half-published
Release is repaired with exactly the missing assets) are one drive:

```bash
make release-upload-selftest
```

It runs a stateful `gh` double, so no network, no keys and no published release
are needed, and each rule carries a NEUTER proof: removing the rule makes its
green arm fail again. That includes the INT-CI-034 metadata bound: a fixture
whose binaries carry a copied identity stamp over a materially different body
is refused with its measured size delta and differing-offset count, and a copy
of the publisher with the bound removed adopts the same fixture.

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
