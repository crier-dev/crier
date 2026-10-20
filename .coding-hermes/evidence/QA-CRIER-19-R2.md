# QA-CRIER-19-R2 — ui-probe cell: detect served text/html surfaces (tick 2026-10-20)

Deliverable: `~/.hermes/scripts/bunker-qa.sh` (tracked in the home meta-repo, the live
copy every QA lane's battery is rendered from) — commit **01c753f4f** on `main`
(`fix(harness): ui-probe cell detects served text/html surfaces — QA-CRIER-19-R2.`),
+93/−15 in one file. No crier source touched (AC-5): the diff is one hunk in the
generated battery's `ui-probe` cell plus the detection helpers it now calls.

## Defect — a gate that could not be satisfied, reported as a product fact

The battery renders its remote script from a heredoc; the `ui-probe` cell's Go arm is
gated by

    elif [ -f go.mod ] && grep -qRE '<ERE>' . --include='*.go' 2>/dev/null; then

QA-BUNKER-B21 (2f2c0fea8, 2026-10-19) broadened that ERE and shipped the term
`\.Serve(` — an **unterminated group**. Measured on the emitted `qa-run.sh` at
crier HEAD 2f68709:

    $ grep -qRE 'http\.ListenAndServe|...|\.Serve(' . --include='*.go'
    grep: Unmatched ( or \(        (rc=2)

rc=2 is a *false condition* in an `&&` chain exactly like rc=1 ("no match"), so the
arm never ran and control fell through to the final
`else cell ui-probe N/A "no web UI detected"`. That is the verdict the 2026-10-20
battery recorded (evidence `/tmp/qa-crier-battery-20261020T1020Z.jsonl`, cell
`ui-probe | N/A | no web UI detected`, ts 10:30:14.299 — 48 ms, i.e. no probe ever
ran) against a product that serves /chat at 87,584 B and /docs at 11,732 B of
text/html. This is the recurrence of QA-CRIER-19, which had *worked* between
2026-09-21 and 2026-10-18; B21 silently un-fixed it.

Class: **harness coverage gap, not a product defect** — the product's UI answered
200 text/html in the battery cycle's own probe.

## Fix

1. **The detection gate can no longer fail silent.** The ERE is *compiled* before it
   is trusted (`ui_pat_compiles`: a valid pattern returns 0/1, an invalid one 2). If
   grep rejects the pattern the scan degrades to fixed-string terms and the cell says
   so; a degraded scan that finds nothing grades **HARNESS-DEGRADED**, never N/A.
   A harness fault is never reported as a product fact.
2. **Probe the documented surface list, not two hardcoded paths.** `/chat /docs / /ui
   /dashboard /app /index.html` (override `BUNKER_QA_UI_PATHS`), and the OK cell now
   names **URL + status + content-type + size** for every text/html answer, so the
   finding stays readable cycles later.
3. **Listening detection is separate from HTML detection.** Any non-000 status proves
   the port answers, so a server whose landing page is JSON/404 (crier's `/` is
   404 application/json) is awaited properly instead of being called dead.
4. **Honest N/A names what was scanned** ("go.mod repo with no HTTP-serving source in
   *.go, scanned: net/http, http.Server, …") — "looked, found none" is now
   distinguishable from "never looked", the defect class this row exists for.
5. Teardown `fuser -k` now touches only the port this arm attributed; the shared-box
   guard (ports answering before the start are excluded) is unchanged.

## Verification — the emitted block, against real repos (not a rewrite)

The block was extracted verbatim from the rendered `qa-run.sh`
(`ui_pat_compiles` … the ui-probe chain) and sourced with a `cell` stub, so every leg
below exercises the exact code the agent runs. Fixtures: crier tree at the battery's
tested HEAD `2f68709` (`go build ./cmd/server` + a `go run ./cmd/server` leg), a
go.mod repo with no HTTP source, and a Go server answering JSON only. Ports 41811/41813
were free (`ss -ltn`).

| leg | harness | fixture | recorded cell |
| --- | --- | --- | --- |
| **R (red)** | **pre-fix** | crier @2f68709 | `N/A \| no web UI detected` — defect reproduced |
| A | fixed | crier, prebuilt binary | `OK \| UI serves text/html: http://127.0.0.1:41811/chat 200 text/html; charset=utf-8 87584B http://127.0.0.1:41811/docs 200 text/html; charset=utf-8 11732B (start: …/crier), other paths: /=[404 application/json] /ui=[404 …] /dashboard=[404 …] /app=[404 …] /index.html=[404 …]` |
| A2 | fixed | crier, `go run ./cmd/server` | same OK on :41813 (real BIN shape + process-group teardown) |
| B | fixed | go.mod, no HTTP source | `N/A \| no web UI detected: go.mod repo with no HTTP-serving source in *.go (scanned: net/http, http.Server, ListenAndServe, ServeMux, HandleFunc, chi/mux/gin/echo/fiber routers, .Serve() ) and no package.json dev/start script` |
| C | fixed | server answers JSON only | `INFO \| go-native server answers on :41812 but NO 2xx text/html surface on any probed path [/chat /docs / /ui /dashboard /app /index.html] — exact answers: /chat=[200 application/json 21B] /docs=[404 application/json] …` |
| D | fixed | no-HTTP repo + **the shipped broken ERE** | `INFO \| HARNESS-DEGRADED: … the HTTP-detection scan was UNTRUSTWORTHY … a harness fault, not a no-UI verdict` (never N/A) |
| E | fixed | crier + **the shipped broken ERE** | `OK \| …/chat 200 text/html; charset=utf-8 87584B … (HARNESS-DEGRADED: the detection ERE was rejected by grep — the arm ran on fixed-string terms)` |

AC map: **1** → A/A2 (OK names URL, status, content-type); **2** → A/A2 record
`/chat 200 text/html … 87584B`; **3** → B (honest N/A, reason named); **4** → A/A2/E
(HTML served ⇒ never UI-less) and D (unknown ⇒ HARNESS-DEGRADED, not a verdict);
**5** → diff is `bunker-qa.sh` only, zero crier Go changes.

Gates: `bash -n bunker-qa.sh` PASS; rendered body `bash -n` PASS; render stderr empty
(the comment-backtick trap was caught in a first render — `` `grep -qRE` `` and
`` `&&` `` executed on the controller, exactly the QA-ASCE-8 fence — and removed).
Teardown: no listener left on 41811/41812/41813/41814/41815 after all legs; the
foreign :5173 server (canopy-vite) still answers 200, i.e. the shared-box guard works.
Commit gate: guard PASS (DEGRADED PASS — no staged Go/lint scope; secrets clean,
tests full) on the home meta-repo commit.

## Not covered / honest limits

- The battery was not re-run end-to-end on a fresh bunker agent for this row: the
  proof is the rendered block executed against the same crier HEAD the failed battery
  tested (2f68709), with the same server and the same port attribution logic. A full
  `bunker-qa.sh launch` re-run is the next QA lane's confirmation.
- The npm arms (`package.json` / `frontend/package.json`) are untouched; the
  redirect-first `-L` probe of 9router and the ENV-BLOCKED gate of canopy are as they
  were.
- `BUNKER_QA_UI_PATHS` is a new knob; a repo whose panel lives outside the default
  list still needs the override (the INFO line names the paths it probed, so that is
  visible rather than silent).
