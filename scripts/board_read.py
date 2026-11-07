#!/usr/bin/env python3
"""board_read.py — read the authoritative board without truncation.

Why this exists: `.coding-hermes/board/tasks.jsonl` is 2.3 MB and 870+ rows.
`read_file` truncates at a 100 000-char budget, which lands inside the first ~53
lines — all of them completed rows — while every pending row lives near the tail.
A tick that reads the board that way sees "nothing to do" and stands down. That
exact failure produced 12 of 22 dry ticks on 2026-11-07.

Usage:
    python3 scripts/board_read.py                 # open rows, newest first
    python3 scripts/board_read.py --next 10       # first 10 open rows
    python3 scripts/board_read.py --id CRIER-HARNESS-8
    python3 scripts/board_read.py --all           # status census only

Output is small by construction: only rows that are not complete, one line each,
plus a census header naming the source file and its sizes. Never prints the whole
board.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys

BOARD_RELPATH = ".coding-hermes/board/tasks.jsonl"
OPEN_STATUSES = ("pending", "in_progress", "blocked", "rework")


def repo_root() -> str:
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            capture_output=True, text=True, timeout=15,
        )
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return os.getcwd()


def load(path: str) -> tuple[dict[str, dict], int, int]:
    rows: dict[str, dict] = {}
    lines = bad = 0
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            lines += 1
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                bad += 1
                continue
            rows[str(row.get("id"))] = row  # last write wins, same as boardctl
    return rows, lines, bad


def status_of(row: dict) -> str:
    return str(row.get("status") or "").strip().lower()


def gate_of(row: dict) -> str:
    """Return a one-word gate label, or '' when the row is genuinely ready.

    The board encodes gates three ways and a reader must honour all three:
      * the `owner_gate` field,
      * a `blocked_reason` string (may be a partial gate — print it, do not hide it),
      * an `OWNER-GATED` marker in the title or acceptance text.
    block_drive.py honoured only the first two, which is why CRIER-HARNESS-8 read
    ready=1 on every tick for a day while its own title said OWNER-GATED.
    """
    if row.get("owner_gate"):
        return "owner-flag"
    text = f"{row.get('title') or ''} {row.get('acceptance_criteria') or ''}".upper()
    if "OWNER-GATED" in text or "BLOCKED ON OWNER" in text:
        return "owner-text"
    if row.get("blocked_reason"):
        return "blocked-reason"
    return ""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--next", type=int, default=0, help="show at most N open rows")
    ap.add_argument("--id", default="", help="show one row by id")
    ap.add_argument("--all", action="store_true", help="census only")
    ap.add_argument("--json", action="store_true", help="emit open rows as JSON")
    ap.add_argument("--repo", default="", help="repo root (default: git toplevel)")
    args = ap.parse_args()

    root = args.repo or repo_root()
    path = os.path.join(root, BOARD_RELPATH)
    if not os.path.exists(path):
        print(f"board_read: no board at {path}", file=sys.stderr)
        return 2

    rows, lines, bad = load(path)
    size = os.path.getsize(path)
    census: dict[str, int] = {}
    for row in rows.values():
        census[status_of(row) or "?"] = census.get(status_of(row) or "?", 0) + 1
    open_rows = [
        row for row in rows.values()
        if status_of(row) in OPEN_STATUSES or status_of(row) not in ("complete",)
    ]
    # rows with an unknown/blank status are surfaced, never silently dropped
    open_rows.sort(key=lambda r: (str(r.get("priority") or "P9"), str(r.get("id"))))

    print(f"board source: {path}")
    print(f"  {lines} lines, {len(rows)} unique ids, {bad} unparseable, {size} bytes")
    print("  status census: " + ", ".join(f"{k}={v}" for k, v in sorted(census.items())))
    print(f"  open (not complete): {len(open_rows)}")

    if args.id:
        row = rows.get(args.id)
        print(json.dumps(row, indent=1) if row else f"  {args.id}: NOT FOUND")
        return 0 if row else 1
    if args.all:
        return 0
    if args.json:
        print(json.dumps(open_rows, indent=1))
        return 0

    shown = open_rows[: args.next] if args.next else open_rows
    for row in shown:
        gate = gate_of(row)
        marks = []
        if gate:
            marks.append(gate)
        if row.get("depends_on"):
            marks.append("deps=" + ",".join(str(d) for d in row["depends_on"]) if isinstance(row["depends_on"], list) else f"deps={row['depends_on']}")
        if row.get("worker_status"):
            marks.append(f"worker={row['worker_status']}")
        suffix = ("  [" + " ".join(marks) + "]") if marks else ""
        print(f"  {str(row.get('id')):24} {str(row.get('priority') or '?')} "
              f"{status_of(row):11} {str(row.get('title') or '')[:78]}{suffix}")
        if gate == "blocked-reason":
            print(f"      reason: {str(row.get('blocked_reason'))[:200]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
