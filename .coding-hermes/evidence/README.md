# `.coding-hermes/evidence/` — committed proof for off-repo closes

This directory holds the operational evidence artifacts described in
[`docs/ops-evidence.md`](../../docs/ops-evidence.md). An artifact exists so that a
Tier-2 judge — which grades the local diff — can read the measurement for work
whose deliverable landed off-repo (a remote deploy, host configuration, another
repository, a live service change).

## Rules

- **One file per task id, named `<TASK-ID>.md`** — the board row id, verbatim
  (e.g. `QA-CRIER-12.md`). One task, one file; the filename and the artifact's
  `Task:` field must agree.
- **Committed with the close it proves.** The artifact lands in the SAME commit as
  the board/status flip for that task. A later commit is not part of that close.
- **Append-only.** Never rewrite a committed artifact. A correction or a
  re-verification is a new dated section appended to the existing file — the
  date-stamped history is the point, and editing it destroys the record.
- **Redaction is mandatory.** No tokens, API keys, passwords, private keys or
  session cookies. Redact as `<redacted>` in commands AND in pasted output. The
  artifact is committed to a public-facing repo: an unredacted secret is an
  incident, not an inconvenience.
- **Evidence, not assertion.** Every acceptance criterion carries the exact
  command run and its real observed output, pasted verbatim. A file that states a
  version was deployed, without the output that showed it, is a claim — not
  evidence.
- **In-repo code tasks do not need an artifact.** Their deliverable is the diff,
  and the tests grade it.

Start from [`TEMPLATE.md`](TEMPLATE.md) and fill every placeholder.
