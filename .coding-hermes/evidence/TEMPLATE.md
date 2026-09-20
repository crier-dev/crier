# <TASK-ID> — <one-line summary of the operation>

<!--
Fill-in template for an operational evidence artifact (docs/ops-evidence.md).
Copy to .coding-hermes/evidence/<TASK-ID>.md using the board row id, replace every
<...> placeholder, delete these comment lines, and commit the artifact in the SAME
commit as the board/status flip it proves. Append-only: corrections are new dated
sections, never rewrites. Redact every secret as <redacted> — in commands and in
pasted output.
-->

- **Task:** `<TASK-ID>` — <board row title, verbatim>
- **Tick / date:** tick `<N>` — `<YYYY-MM-DD>` (UTC; probe times as `<HH:MM:SS>Z`)
- **Operation:** <what was actually done, 1-2 sentences: redeploy / config change /
  host op / commit in another repo>
- **Targets:**
  - host(s): `<host>` (<how it was reached, e.g. `ssh <alias>`)
  - repo(s): `<repo>@<sha>` (<what changed there, if anything>)
  - service(s): `<service> <version>` (built from `<sha>`)
- **External commits:** `<sha>` <subject> in `<repo>`  — or: none (no repository
  change off-repo)

## Acceptance criteria

### AC-1 — <criterion exactly as the board row states it>

- **Command run:**
  ```bash
  <exact command, verbatim, secrets redacted>
  ```
- **Observed output:**
  ```text
  <real output of the command above, pasted verbatim — not summarised>
  ```
- **Before:** <state before the operation>
- **After:** <state after the operation, as the output above establishes it>

### AC-2 — <next criterion>

- **Command run:**
  ```bash
  <exact command, verbatim, secrets redacted>
  ```
- **Observed output:**
  ```text
  <real output, pasted verbatim>
  ```
- **Before:** <state before>
- **After:** <state after>

## External commits

- `<sha>` <subject> — `<repo>` (<what it delivered, e.g. the fix that was deployed>)
- <any other sha the operation depends on or produced; "none" is a valid entry>
