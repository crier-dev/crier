# Operational evidence — an off-repo close must commit its proof (DF-CRIER-276)

Some tasks' deliverable does not land in this repository. The work happens on a
remote host, in another repository, or against a live service — and the closing
commit here is a board/status flip. The Tier-2 judge grades the local diff, so it
has nothing to read. This document is the rule and the artifact contract for that
class of close.

## The failure class

QA-CRIER-12 (closed on tick 359) had exactly this shape. Its deliverable was a
redeploy on the remote host `bunker-las-01`, executed from the BUNKER repository:
`bunkerd 0.1.4` (built from `198f6b4`) had to replace an 8-day-old binary. The
crier-repo close diff was a **9-line `.gitreins/tasks.yaml` status flip** — no
deploy, no host, no version anywhere a diff reader could look.

Tier-2 returned INCOMPLETE (verdict `53a6b62a`, recorded at
`.gitreins/history/2026-09-20/53a6b62a/verdict.json`: tier1 PASS, tier2
INCOMPLETE). It resolved the "deployed version" criterion by reading the BUNKER
board's `GAP-083` row — whose probe **predated the deploy** — and so read the
criterion as unproven. The judge's own probe agents on the host had already
demonstrated the opposite: agent `qa12-probe-a` freed the port range on destroy
(20:11:39 PDT) and `qa12-probe-b` allocated the recycled range `30000-30099` on
respawn (20:12:04 PDT). The foreman re-verified on the live host at `03:19Z`
(`bunkerd 0.1.4` / `198f6b4`). The verdict was a false negative about real work,
not a work defect.

The load-bearing rule it exposes: **a board row is a claim, not a measurement.**
A judge that can only see the local diff has no measurement to read, so it falls
back to whatever claim is nearby — here, a stale one.

The class is not specific to deploys. It is every tick whose work lands off-repo:
a remote deploy, a host configuration change, a change in another repository, or a
live service change. Each produces a local diff that is metadata, and metadata
cannot prove an operation.

## The rule

When a task's deliverable lands off-repo — a remote deploy, host configuration,
another repository, or a live service change — **the closing commit in THIS
repository MUST include an evidence artifact at
`.coding-hermes/evidence/<TASK-ID>.md`**, where `<TASK-ID>` is the board row id of
the task being closed (e.g. `.coding-hermes/evidence/QA-CRIER-12.md`).

It goes in the **same commit** as the board/status flip it closes. An artifact
committed in a later commit is not part of the close; it is a follow-up the judge
of that close never saw.

## The artifact contract

Every artifact carries all of the following. The template is
`.coding-hermes/evidence/TEMPLATE.md`; `README.md` in that directory states the
naming, append-only and redaction rules.

1. **Task id** — the board row this closes, matching the filename.
2. **Tick and UTC date** — when the operation was performed and when it was
   verified (`YYYY-MM-DD`, probe times in UTC).
3. **The operation performed** — what was actually done, in one or two sentences
   (redeploy, config change, host op, commit in another repo).
4. **Targets, with versions or shas** — the host(s), repo(s) and service(s)
   touched, each named concretely: `<host>`, `<repo>@<sha>`, `<service> <version>
   (built from <sha>)`. "the server" is not a target.
5. **The acceptance criteria** — the criteria as the board row states them.
6. **For EACH criterion, its live verification:**
   - the **exact command run** (verbatim, secrets redacted);
   - its **real observed output** — pasted, not summarised or paraphrased;
   - the **before/after state** the output establishes;
   - and **links to any external commit shas** the operation produced or depends
     on (`<sha>` plus its subject, and the repo it lives in).

## Why it works

The artifact rides the same commit as the board/status flip, so the judge's
local-diff view — the only view it has — now contains the operational proof:
the command, its output, and the versions on both sides of the change. The
criterion stops being resolved by proximity (the nearest claim on a board) and
starts being resolved by measurement.

The contract is deliberately unforgiving on one point: **an artifact that only
asserts, without probe output, is not evidence.** A file that says "verified:
bunkerd 0.1.4 deployed" is a claim of the same kind that already failed — it just
has a new filename. The output block is the evidence; everything else in the
artifact is there to make that output legible and to place it in time.

## Scope

- **Pure in-repo code tasks need no artifact.** Their deliverable *is* the local
  diff, and Tier 1 (secrets / build / vet / tests) plus the guard already grade it
  directly. Adding an artifact there would be duplicated judgement, not evidence.
- The convention applies to the off-repo share of the work. A task that does both
  — a code change here plus a deploy elsewhere — needs the artifact for the
  off-repo criteria and its normal tests for the code.
- An artifact never substitutes for a local test that could exist, and nothing in
  `.coding-hermes/evidence/` is a grading input for the local suite. If a criterion
  can be checked in the repo, check it in the repo.
