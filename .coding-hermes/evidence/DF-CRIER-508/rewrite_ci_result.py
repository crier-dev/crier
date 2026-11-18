#!/usr/bin/env python3
"""DF-CRIER-508 — rewrite free-form ci_result values into the structured form.

The boardctl result-value canon (internal/board/result_schema.go, SCHED-GAP-1572)
accepts a guard_result/ci_result value as EITHER a plain vocabulary string
({GREEN,RED,SKIP} for ci_result, case-tolerant) OR the structured form

    {"status": "pass|fail|skip|pending", "details": "<prose>"}

Twelve crier rows still hold legacy free-form prose (BT-007: tolerated on read,
warned by validate). This script migrates exactly those rows:

  - it operates on a COPY of the board first (default) and prints a dry-run
    diff summary; nothing is written unless --apply is passed;
  - it rewrites ONLY the ci_result value's raw bytes on the 12 named rows,
    matching each row's own compact/spaced JSON style, so every other line of
    tasks.jsonl stays byte-identical and key order inside the row is preserved;
  - `details` carries the ORIGINAL prose verbatim (no information loss) and
    `status` is classified from the prose: GREEN/pass -> "pass", RED -> "fail",
    PENDING -> "pending", BLOCKED -> "skip" (CI never ran);
  - a row whose stored value no longer equals the expected free-form string
    (already migrated, or edited since) is SKIPPED loudly, never guessed.

Exit codes: 0 = dry-run/apply consistent (12 rows matched or already done);
1 = a named row is missing or its value changed since this script was written
(the row must be re-quoted in the evidence file before migrating).
"""
import json
import os
import re
import shutil
import sys
import tempfile

BOARD = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                      "..", "..", "board", "tasks.jsonl"))

# id -> (expected current free-form value, classified status)
ROWS = {
    "INT-CI-012": ("GREEN - CI run 39338420027 on exactly d0e55ce: conclusion success, all 6 jobs success (Build & Test 1.26.9, Whole-board validation, Coverage, Integration Tests, Release acceptance matrix, OpenAPI Spec Validator); the Agent-ecosystem offline execution binding step passed with the provisioned venv (pytest==9.1.1 installed into $RUNNER_TEMP/agent-ecosystem-battery-venv, node tests 83/83).", "pass"),
    "INT-CI-013": ("GREEN - same run 39338420027 on d0e55ce: 'Whole-board validation (fail closed)' job success and Build & Test success; the host-hook leak that reded the local Tier-1 gate did not reach CI (the runner has no global core.hooksPath), and the local gate is now green too (gitreins guard --full Tier 1 PASS).", "pass"),
    "DF-CRIER-428": ("pass", "pass"),
    "DF-CRIER-466": ("GREEN (merged to main 856dc28; local gates green; verdict 3c593503)", "pass"),
    "DF-CRIER-468": ("GREEN (merged 6e71013; local gates green; verdict 9fa82068 PASS)", "pass"),
    "DF-CRIER-469": ("GREEN (merged to main 12604ce; local gates green; verdict 574cca34 FAIL-on-artifact disclosed)", "pass"),
    "DF-CRIER-473": ("RED (CI run 40045386277 on d51d4ec; cmd/server short suite failed; local focused gates pass)", "fail"),
    "DF-CRIER-474": ("GREEN (GitHub run 40109528478; sibling same-SHA runs pending at recovery close)", "pass"),
    "QA-CRIER-REVIEW-1": ("BLOCKED", "skip"),
    "QA-CRIER-51": ("PENDING", "pending"),
    "INT-CI-017": ("GREEN (exact-head CI Build & Test run 40582899662)", "pass"),
    "INT-CI-036": ("PENDING: post-closeout exact-head CI", "pending"),
}


def rewrite_line(line, tid):
    """Return the line with the ci_result value migrated, or None if not this row."""
    m = re.search(r'"ci_result"\s*:\s*', line)
    if not m:
        return None
    start = m.end()
    val, end = json.JSONDecoder().raw_decode(line, start)
    if not isinstance(val, str) or val not in (ROWS[tid][0],):
        return "SKIP-VALUE"
    structured = {"status": ROWS[tid][1], "details": val}
    spaced = line[start - 1] == " "
    if spaced:
        new = json.dumps(structured, ensure_ascii=True)
    else:
        new = json.dumps(structured, ensure_ascii=True, separators=(",", ":"))
    return line[:start] + new + line[end:]


def main():
    apply = "--apply" in sys.argv
    src = BOARD
    if not os.path.isfile(src):
        print(f"ERROR board not found: {src}", file=sys.stderr)
        return 2
    work = src
    tmp = None
    if not apply:
        tmp = tempfile.mktemp(prefix="df508-ci-dryrun-", dir=os.path.dirname(src))
        shutil.copy2(src, tmp)
        work = tmp

    seen, changed, problems = set(), 0, []
    out = []
    with open(src, "r", encoding="utf-8") as f:
        lines = f.readlines()
    for i, line in enumerate(lines):
        raw = line.rstrip("\n")
        tid = None
        m = re.match(r'\s*\{.*"id"\s*:\s*"([^"]+)"', raw)
        if m and m.group(1) in ROWS:
            tid = m.group(1)
        if tid:
            seen.add(tid)
            res = rewrite_line(raw, tid)
            if res == "SKIP-VALUE":
                problems.append(f"line {i+1} ({tid}): ci_result value is not the expected free-form string — NOT touched")
            elif res is None:
                problems.append(f"line {i+1} ({tid}): no ci_result key found — NOT touched")
            else:
                out.append(line.replace(raw, res, 1))
                changed += 1
                continue
        out.append(line)
    missing = sorted(set(ROWS) - seen)
    for tid in missing:
        problems.append(f"row {tid}: not found in board")
    for tid in sorted(seen - set(ROWS)):
        problems.append(f"row {tid}: matched by prefix but not in the ROWS table — NOT touched")

    print(f"board={src} mode={'APPLY' if apply else 'DRY-RUN(copy)'}")
    print(f"rows matched={changed}/12 rewritten={changed} already-migrated/absent={len(missing)}")
    if changed:
        import difflib
        diff = list(difflib.unified_diff(
            [l.rstrip("\n") for l in lines], [l.rstrip("\n") for l in out],
            lineterm="", n=0))
        for ln in diff[2:]:
            print(("  " + ln[:160]))
    if problems:
        print("PROBLEMS:")
        for p in problems:
            print("  " + p)
    if apply and not problems:
        with open(src, "w", encoding="utf-8") as f:
            f.writelines(out)
        print(f"APPLIED {changed} row(s) to {src}")
    elif apply and problems:
        print("APPLY REFUSED — fix the problems above first")
    if tmp:
        os.unlink(tmp)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
