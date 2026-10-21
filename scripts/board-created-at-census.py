#!/usr/bin/env python3
"""DF-CRIER-451 — the board report may not silently measure a fraction of the board.

Reads .coding-hermes/board/tasks.jsonl and asserts three things:

 1. every row carries created_at, or carries created_at=null WITH a non-empty
    created_at_source explaining why the date is not known (a null must carry a reason);
 2. every row's created_at parses, and a created_at later than the row's own
    updated_at is allowed only when the row also carries created_at_note explaining it
    (an imported row's first tracked appearance is legitimately later than its own
    timestamps);
 3. the census is printed with counts, so a report can quote the real coverage instead
    of an assumed one.

Exit codes: 0 = every row accounted for; 1 = a row is unaccounted for (named);
2 = the board file is missing or has no rows (never a vacuous PASS).

--selftest runs the same checks against fixtures under $TMPDIR and proves the checker
REJECTS a row with a missing date and no reason, and a row whose ordering anomaly has no
note, while ACCEPTING an accounted-for board — plus a NEUTER copy that accepts the bad
fixture, so a green selftest cannot be vacuous.
"""
import json
import os
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

try:
    from zoneinfo import ZoneInfo
    LOCAL = ZoneInfo("America/Bogota")
except Exception:  # pragma: no cover - zoneinfo is stdlib since 3.9
    LOCAL = timezone.utc


def parse_ts(value):
    """Parse the board's mixed timestamp shapes.

    The board stores three shapes in the same field family — ``...Z``, ``...+00:00`` and a
    naive ``YYYY-MM-DD HH:MM:SS`` — so comparing the raw strings is meaningless (an
    ISO-with-T value sorts above a space-separated one regardless of the instant). A naive
    value is read as fleet-local time, which is what every writer on this host used.
    """
    if not value:
        return None
    s = str(value).strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(s)
    except Exception:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=LOCAL)
    return parsed.astimezone(timezone.utc)

BOARD = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".coding-hermes", "board", "tasks.jsonl")
BOARD = os.path.normpath(BOARD)


def load(path):
    rows = []
    with open(path, encoding="utf-8") as f:
        for i, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                rows.append((i, json.loads(line)))
            except Exception as exc:
                raise SystemExit(f"FAIL board line {i}: not JSON ({exc})")
    return rows


def check(path):
    """Return (failures, stats) — pure, so the selftest can drive it on fixtures."""
    rows = load(path)
    if not rows:
        return ["board has no rows — refusing to report a coverage of nothing"], {}
    failures = []
    have = null_reason = annotated = 0
    for lineno, r in rows:
        rid = r.get("id") or f"<line {lineno}>"
        c, u = r.get("created_at"), r.get("updated_at")
        if c:
            have += 1
        else:
            src = (r.get("created_at_source") or "").strip()
            if not src:
                failures.append(f"{rid}: created_at is null with no created_at_source (a null must carry a reason)")
            else:
                null_reason += 1
        if c and u and parse_ts(c) and parse_ts(u) and parse_ts(c) > parse_ts(u):
            if not (r.get("created_at_note") or "").strip():
                failures.append(f"{rid}: created_at ({c}) is later than updated_at ({u}) with no created_at_note")
            else:
                annotated += 1
    stats = {"rows": len(rows), "with_created_at": have, "null_with_reason": null_reason,
             "ordering_anomaly_with_note": annotated,
             "coverage_pct": round(100.0 * (have + null_reason) / len(rows), 1)}
    return failures, stats


def selftest():
    tmp = tempfile.mkdtemp(prefix="c451-")
    good = os.path.join(tmp, "good.jsonl")
    bad_missing = os.path.join(tmp, "bad_missing.jsonl")
    bad_order = os.path.join(tmp, "bad_order.jsonl")
    with open(good, "w", encoding="utf-8") as f:
        f.write(json.dumps({"id": "OK-1", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}) + "\n")
        f.write(json.dumps({"id": "OK-2", "created_at": None, "created_at_source": "unknown: pre-tracking"}) + "\n")
        f.write(json.dumps({"id": "OK-3", "created_at": "2026-01-03", "updated_at": "2026-01-01",
                            "created_at_note": "imported"}) + "\n")
    with open(bad_missing, "w", encoding="utf-8") as f:
        f.write(json.dumps({"id": "BAD-1", "created_at": None}) + "\n")
    with open(bad_order, "w", encoding="utf-8") as f:
        f.write(json.dumps({"id": "BAD-2", "created_at": "2026-05-05", "updated_at": "2026-01-01"}) + "\n")
    empty = os.path.join(tmp, "empty.jsonl")
    open(empty, "w").close()

    passed = failed = 0
    def expect(name, cond):
        nonlocal passed, failed
        if cond:
            passed += 1
            print(f"PASS {name}")
        else:
            failed += 1
            print(f"FAIL {name}")

    f_good, s_good = check(good)
    expect("accepts an accounted-for board", not f_good)
    expect("counts the reasoned null", s_good.get("null_with_reason") == 1)
    expect("counts the annotated anomaly", s_good.get("ordering_anomaly_with_note") == 1)
    f_missing, _ = check(bad_missing)
    expect("rejects a null with no reason", any("no created_at_source" in x for x in f_missing))
    f_order, _ = check(bad_order)
    expect("rejects an unannotated ordering anomaly", any("no created_at_note" in x for x in f_order))
    f_empty, _ = check(empty)
    expect("refuses an empty board instead of a vacuous PASS", bool(f_empty))

    # NEUTER: a copy that ignores the reason requirement must ACCEPT the bad fixture.
    src = open(os.path.abspath(__file__), encoding="utf-8").read()
    neuter = os.path.join(tmp, "neuter.py")
    with open(neuter, "w", encoding="utf-8") as f:
        f.write(src.replace('failures.append(f"{rid}: created_at is null with no created_at_source (a null must carry a reason)")',
                            "pass"))
    out = subprocess.run([sys.executable, neuter, "--check", bad_missing], capture_output=True, text=True)
    expect("NEUTER copy accepts what the real checker rejects", out.returncode == 0)

    print(f"RESULT {passed}/{passed+failed} passed")
    return 0 if failed == 0 else 1


def main():
    args = sys.argv[1:]
    if "--selftest" in args:
        return selftest()
    path = BOARD
    if "--board" in args:
        path = args[args.index("--board") + 1]
    else:
        positional = [a for a in args if not a.startswith("--")]
        if positional:
            path = positional[0]
    failures, stats = check(path)
    if "--check" in args:
        return 0 if not failures else 1
    print(f"board: {path}")
    print(f"rows: {stats.get('rows')} | created_at: {stats.get('with_created_at')} | "
          f"null-with-reason: {stats.get('null_with_reason')} | ordering-anomaly-with-note: "
          f"{stats.get('ordering_anomaly_with_note')} | accounted-for: {stats.get('coverage_pct')}%")
    if failures:
        print(f"FAIL {len(failures)} row(s) unaccounted for:")
        for x in failures[:20]:
            print("  -", x)
        return 1
    print("PASS every row is accounted for")
    return 0


if __name__ == "__main__":
    sys.exit(main())
