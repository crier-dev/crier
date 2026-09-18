#!/usr/bin/env python3
"""Bounded, self-cleaning CPU load generator — the sanctioned way to put synthetic
load on a box (DF-CRIER-254).

WHY THIS EXISTS
---------------
A "fails 4/24 runs ON A LOADED BOX" reproduction on 2026-09-18 was driven with an
ad-hoc shell loop that backgrounded one burn unit per iteration.  Nothing OWNED
those units: when the spawner exited they reparented to the user systemd manager
and outlived every caller, so 73 -> 278 concurrent burn units accumulated (each
one a shell no-op loop burning ~13% CPU), loadavg reached 220/346/237 with ~2000
processes, and the box — shared with the Hermes gateway, the cron scheduler and
DuckBrain — was unusable for hours.  Every judge run that re-read the criterion
respawned them.

This module makes that shape impossible:

* bounded WORKERS (hard cap 8) and bounded LIFETIME (hard cap 300 s).  An
  out-of-cap request is REFUSED, naming the cap (exit 2) — never clamped
  silently, so nobody can believe they asked for 64 units and got 8;
* a LOAD-AVERAGE GATE: with the measured 1-minute load average above
  ``--load-threshold`` (default 8.0) the run is SKIPPED with a one-line recorded
  reason (exit 3) instead of piling more load onto a busy host;
* every child is a daemonic ``multiprocessing`` process that arms Linux
  ``prctl(PR_SET_PDEATHSIG, SIGKILL)``, so a ``SIGKILL`` of THIS process — the
  case no ``finally``/``atexit`` can cover — still kills the children in the
  kernel, and a child that finds its parent already gone exits instead of
  spinning;
* a hard teardown in a ``finally``: SIGTERM -> ``join(5)`` -> SIGKILL -> VERIFY,
  and any pid still alive is printed and makes the run exit 1;
* no ``setsid``, no detached process, no shell busy-wait loop, and no
  ``pkill``/``pgrep -f`` anywhere in the file.

What is NOT here on purpose: the shared-host *process marker* refusal from the
sibling implementation in another repo.  A load run on a shared box is refused
here by MEASURED LOAD (the threshold above), which is the property the criterion
asks for; callers that want a synthetic value pass ``--loadavg-file``.

USAGE
-----
    python3 scripts/loadgen.py --workers 4 --seconds 60 --cpus 0-3
    python3 scripts/loadgen.py --workers 2 --seconds 5 --json
    python3 scripts/loadgen.py --loadavg-file /tmp/synthetic.loadavg --workers 1 --seconds 1

OUTPUT CONTRACT
---------------
stdout carries the VERDICT — one JSON object with ``--json``, human ``loadgen:
key=value`` lines without it.  stderr carries DIAGNOSTICS — most importantly one
machine-readable start line, emitted only once every child is running AND has
reported whether it armed the parent-death signal:

    loadgen: started workers=2 pids=1234,1235

A harness that must kill this process to prove the children cannot outlive it
reads its pids from that line.  ``SKIPPED: ...`` (the load gate) is printed on
stdout as a plain line in both output modes — the exit code is the first thing a
caller should read (0 ok, 1 survivors, 2 refusal/misuse, 3 load skip).

EXIT CODES
----------
    0  the run completed and every child is gone
    1  at least one child SURVIVED teardown (also wins over 128+signum)
    2  out-of-cap workers/seconds, a bad ``--cpus`` spec, or an unreadable /
       unparseable ``--loadavg-file`` (fail closed: an unreadable load figure
       must never be read as "the host is idle")
    3  the load gate skipped the run (above ``--load-threshold``)
    128+signum  interrupted by that signal; children were still torn down
"""

from __future__ import annotations

import argparse
import ctypes
import json
import multiprocessing
import os
import signal
import sys
import time

# ── caps and defaults (the numbers the doc and AGENTS.md quote) ───────────────
MAX_WORKERS = 8
DEFAULT_WORKERS = 4
MAX_SECONDS = 300.0
DEFAULT_SECONDS = 30.0
DEFAULT_LOAD_THRESHOLD = 8.0
DEFAULT_LOADAVG_FILE = "/proc/loadavg"
PR_SET_PDEATHSIG = 1

EXIT_OK = 0
EXIT_SURVIVORS = 1
EXIT_REFUSED = 2
EXIT_LOAD_SKIP = 3

# How long the parent waits for the children's arming reports before it stops
# waiting and announces the run anyway (a child that cannot report is a child
# this process can still kill in the `finally`).
ARM_REPORT_BUDGET_S = 3.0
# Teardown step budgets, matching the documented "terminate -> join(5) -> kill".
JOIN_TIMEOUT_S = 5.0


class Refused(Exception):
    """A request this harness will not run (exit 2)."""

    exit_code = EXIT_REFUSED


class LoadSkip(Exception):
    """The host is already above the load threshold (exit 3)."""

    exit_code = EXIT_LOAD_SKIP

    def __init__(self, loadavg: float, threshold: float) -> None:
        self.loadavg = loadavg
        self.threshold = threshold
        super().__init__(self.reason())

    def reason(self) -> str:
        return (
            f"1-minute loadavg {self.loadavg:.2f} is above the threshold "
            f"{self.threshold:.2f}"
        )


class Interrupted(Exception):
    """A teardown signal arrived; the children are torn down before exit."""

    def __init__(self, signum: int) -> None:
        self.signum = signum
        super().__init__(f"interrupted by signal {signum}")


# ── the load-average gate ─────────────────────────────────────────────────────


def read_loadavg(path: str) -> float:
    """Return the 1-minute load average from ``path``.

    ``/proc/loadavg``'s first whitespace-separated field is the documented
    source; the file is a parameter so a test can pin a synthetic value and so a
    caller on a host without /proc can still use the gate.  Anything unreadable
    or unparseable RAISES (exit 2): an unknown load figure must never be read as
    "the host is idle".
    """
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as handle:
            raw = handle.read()
    except FileNotFoundError:
        raise Refused(f"load-average file not found: {path}") from None
    except OSError as exc:
        raise Refused(f"cannot read the load-average file {path}: {exc}") from None
    fields = raw.split()
    if not fields:
        raise Refused(f"load-average file {path} is empty (no 1-minute value)")
    try:
        return float(fields[0])
    except ValueError:
        raise Refused(
            f"cannot parse a 1-minute load average from the first field of {path}: "
            f"{fields[0]!r}"
        ) from None


# ── children, affinity, liveness ──────────────────────────────────────────────


def pdeathsig_available() -> bool:
    """True when the kernel parent-death mechanism can be armed on this host.

    Availability is reported, never assumed: the summary carries the observed
    result (what the children actually reported) rather than this platform
    guess, and the teardown verification is the guarantee on every platform.
    """
    if os.name != "posix" or not sys.platform.startswith("linux"):
        return False
    try:
        libc = ctypes.CDLL("libc.so.6", use_errno=True)
    except (OSError, AttributeError):
        return False
    return hasattr(libc, "prctl")


def _arm_parent_death_signal(parent_pid: int) -> bool:
    """Ask the kernel to SIGKILL this process when its parent dies (Linux).

    Returns False where the mechanism is unavailable (non-Linux, no libc).  The
    parent's own teardown still applies there — the mechanism is the extra
    guarantee that covers a ``SIGKILL`` of the parent, not the only one.

    The parent-pid check closes the fork/arm race: a parent that died between
    ``fork`` and this call can never be observed again, so the child exits
    instead of spinning for the rest of its life.
    """
    armed = False
    if pdeathsig_available():
        try:
            libc = ctypes.CDLL("libc.so.6", use_errno=True)
            armed = libc.prctl(PR_SET_PDEATHSIG, signal.SIGKILL, 0, 0, 0) == 0
        except (OSError, AttributeError):  # pragma: no cover - defensive
            armed = False
    try:
        parent_gone = os.getppid() != parent_pid
    except OSError:  # pragma: no cover - defensive
        parent_gone = False
    if parent_gone:
        os._exit(0)
    return armed


def _spin(parent_pid: int, report) -> None:  # pragma: no cover - burns CPU by design
    """Burn one CPU until torn down.  Daemonic child; never detaches."""
    # A forked child inherits the parent's SIGTERM/SIGINT handlers, which raise
    # Python exceptions.  Reset them so a teardown signal kills the child
    # immediately instead of running interpreter code inside a burner.
    for signum in (signal.SIGTERM, signal.SIGINT):
        try:
            signal.signal(signum, signal.SIG_DFL)
        except (OSError, ValueError):
            pass
    armed = _arm_parent_death_signal(parent_pid)
    try:
        report.put({"pid": os.getpid(), "pdeathsig": bool(armed)})
    except Exception:
        pass
    while True:
        pass


def _process_context():
    """The ``fork`` context, so a child inherits this module without re-importing.

    ``fork`` is also what makes ``PR_SET_PDEATHSIG`` meaningful here: the child's
    parent is this process.  Where fork does not exist the default context is
    used and the run reports the degraded guarantee.
    """
    try:
        return multiprocessing.get_context("fork")
    except ValueError:  # pragma: no cover - non-POSIX
        return multiprocessing.get_context()


def pid_state(pid: int) -> str:
    """The process state letter from ``/proc/<pid>/stat``, or '' when unknown.

    The field is read after the LAST ')' because the comm field (field 2) is
    parenthesised and may itself contain spaces and parentheses.
    """
    try:
        with open(f"/proc/{pid}/stat", "rb") as handle:
            raw = handle.read().decode("utf-8", "replace")
    except OSError:
        return ""
    cut = raw.rfind(")")
    if cut == -1:
        return ""
    rest = raw[cut + 1 :].split()
    return rest[0] if rest else ""


def pid_alive(pid: int) -> bool:
    """True when ``pid`` is a LIVE process.

    A zombie is DEAD here: an unreaped corpse keeps ``/proc/<pid>`` visible but
    it is not running, holds no CPU, and must not be reported as a survivor.
    """
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 1:
        return False
    state = pid_state(pid)
    if state:
        return state not in ("Z", "z", "X", "x")
    if not os.path.isdir("/proc"):
        # No procfs: fall back to the signal probe (it cannot tell a zombie from
        # a live process, which is exactly why procfs is preferred).
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
        except OSError:
            return False
        return True
    # procfs exists but this pid has no stat entry: it is gone (or was never
    # ours — either way there is no survivor to report).
    return False


def parse_cpus(spec: str, allowed) -> set:
    """Parse ``0-3,7`` into a CPU set that must fit inside ``allowed``.

    An out-of-range request is REFUSED (exit 2) rather than intersected: a run
    silently restricted to other CPUs is not the run that was asked for.
    """
    chosen = set()
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            if "-" in part:
                start_s, _, end_s = part.partition("-")
                start, end = int(start_s), int(end_s)
                if end < start:
                    raise ValueError(f"reversed range {part!r}")
                chosen.update(range(start, end + 1))
            else:
                chosen.add(int(part))
        except ValueError as exc:
            raise Refused(f"cannot parse --cpus {spec!r}: {exc}") from None
    if not chosen:
        raise Refused(f"--cpus {spec!r} selected no CPU")
    outside = sorted(c for c in chosen if c not in allowed)
    if outside:
        raise Refused(
            f"--cpus {spec!r} names CPU(s) {outside} which this host does not allow "
            f"(allowed: {sorted(allowed)})"
        )
    return chosen


# ── the run ───────────────────────────────────────────────────────────────────


def handle_teardown_signal(signum, _frame):  # noqa: ANN001 - signal handler
    raise Interrupted(signum)


def _drain(report, expected: int, budget_s: float) -> list:
    """Collect up to ``expected`` child reports, bounded by ``budget_s``.

    A timeout is not a failure: a child may simply not have been scheduled yet on
    a loaded box, so the loop keeps waiting until the budget is spent (an earlier
    version broke out on the first timeout and silently reported that no child
    had armed the parent-death signal).
    """
    reports = []
    deadline = time.monotonic() + budget_s
    while len(reports) < expected:
        if time.monotonic() >= deadline:
            break
        # SimpleQueue.get() takes no timeout (only Queue.get does), so poll
        # `empty()` and sleep a little; the deadline above is the bound.
        if report.empty():
            time.sleep(0.02)
            continue
        try:
            reports.append(report.get())
        except Exception:  # pragma: no cover - defensive (queue closed)
            break
    return reports


def _teardown(children: list) -> None:
    """terminate -> join(timeout) -> kill -> join(timeout). Idempotent."""
    for child in children:
        if child.is_alive():
            child.terminate()
    for child in children:
        child.join(timeout=JOIN_TIMEOUT_S)
    for child in children:
        if child.is_alive():
            child.kill()
            child.join(timeout=JOIN_TIMEOUT_S)


def generate(
    workers: int = DEFAULT_WORKERS,
    seconds: float = DEFAULT_SECONDS,
    *,
    cpus: str | None = None,
    load_threshold: float = DEFAULT_LOAD_THRESHOLD,
    loadavg_file: str = DEFAULT_LOADAVG_FILE,
    on_start=None,
) -> dict:
    """Run ``workers`` burners for ``seconds`` and return the run summary.

    Raises ``Refused`` (exit 2) for an out-of-cap request or an unusable load
    figure, and ``LoadSkip`` (exit 3) when the host is above the threshold.
    Interruption is NOT raised: the summary comes back with ``interrupted`` set
    to the signal number, so the caller can still report the teardown result.
    """
    if not isinstance(workers, int) or isinstance(workers, bool) or workers < 1:
        raise Refused(f"--workers must be a positive integer, got {workers!r}")
    if workers > MAX_WORKERS:
        raise Refused(
            f"--workers {workers} is above the hard cap of {MAX_WORKERS} "
            "(this harness is bounded; DF-CRIER-254) — reduce the count or run "
            "several bounded passes instead"
        )
    if not isinstance(seconds, (int, float)) or isinstance(seconds, bool) or seconds <= 0:
        raise Refused(f"--seconds must be positive, got {seconds!r}")
    if seconds > MAX_SECONDS:
        raise Refused(
            f"--seconds {seconds:g} is above the hard cap of {MAX_SECONDS:g}s "
            "(this harness is bounded; DF-CRIER-254)"
        )
    if not isinstance(load_threshold, (int, float)) or isinstance(load_threshold, bool):
        raise Refused(f"--load-threshold must be a number, got {load_threshold!r}")
    seconds = float(seconds)
    load_threshold = float(load_threshold)

    loadavg = read_loadavg(loadavg_file)
    if loadavg > load_threshold:
        raise LoadSkip(loadavg, load_threshold)

    affinity_before = None
    if cpus:
        if not hasattr(os, "sched_setaffinity"):  # pragma: no cover - non-Linux
            raise Refused("--cpus is only supported where os.sched_setaffinity exists")
        try:
            allowed = os.sched_getaffinity(0)
            affinity_before = sorted(allowed)
            os.sched_setaffinity(0, parse_cpus(cpus, allowed))
        except OSError as exc:
            raise Refused(f"cannot restrict the CPU set to {cpus!r}: {exc}") from None

    ctx = _process_context()
    parent_pid = os.getpid()
    report = ctx.SimpleQueue()
    children: list = []
    started = time.monotonic()
    summary = {
        "workers": workers,
        "seconds": seconds,
        "cpus": cpus,
        "load_threshold": load_threshold,
        "loadavg_file": loadavg_file,
        "loadavg": loadavg,
        "pids": [],
        "survivors": [],
        "armed": 0,
        "clean": True,
        "pdeathsig": False,
        "pdeathsig_available": pdeathsig_available(),
        "interrupted": None,
        "elapsed": 0.0,
    }

    previous = {}
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        try:
            previous[signum] = signal.signal(signum, handle_teardown_signal)
        except (OSError, ValueError):  # pragma: no cover - non-main thread/OS
            pass
    try:
        try:
            for _ in range(workers):
                child = ctx.Process(
                    target=_spin, args=(parent_pid, report), daemon=True
                )
                children.append(child)
                child.start()
            summary["pids"] = [child.pid for child in children]
            reports = _drain(report, workers, ARM_REPORT_BUDGET_S)
            summary["pdeathsig"] = bool(reports) and len(reports) == workers and all(
                r.get("pdeathsig") for r in reports
            )
            summary["armed"] = len(reports)
            if on_start is not None:
                on_start(list(summary["pids"]))
            deadline = started + seconds
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                time.sleep(min(0.25, remaining))
        except Interrupted as exc:
            summary["interrupted"] = exc.signum
    finally:
        _teardown(children)
        try:
            report.close()
        except Exception:  # pragma: no cover - defensive
            pass
        if affinity_before is not None:
            try:
                os.sched_setaffinity(0, set(affinity_before))
            except OSError:  # pragma: no cover - defensive
                pass
        survivors = [pid for pid in summary["pids"] if pid and pid_alive(pid)]
        summary["survivors"] = survivors
        summary["clean"] = not survivors
        summary["elapsed"] = round(time.monotonic() - started, 3)
        for signum, handler in previous.items():
            try:
                signal.signal(signum, handler)
            except (OSError, ValueError):  # pragma: no cover - defensive
                pass
    return summary


# ── CLI ───────────────────────────────────────────────────────────────────────


def _announce_started(pids: list) -> None:
    """The one machine-readable start line (stderr), flushed immediately.

    It is emitted only after every child is running AND has reported whether it
    armed the parent-death signal, so a harness that kills this process on
    seeing it is proving the mechanism and not racing it.
    """
    print(
        "loadgen: started workers=%d pids=%s" % (len(pids), ",".join(str(p) for p in pids)),
        file=sys.stderr,
        flush=True,
    )


def _emit_summary(summary: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(summary, sort_keys=True))
        return
    pids = ",".join(str(p) for p in summary["pids"]) or "-"
    survivors = ",".join(str(p) for p in summary["survivors"]) or "-"
    print(
        "loadgen: workers=%d seconds=%g pids=%s cpus=%s"
        % (summary["workers"], summary["seconds"], pids, summary["cpus"] or "-")
    )
    print(
        "loadgen: loadavg_file=%s loadavg=%.2f threshold=%.2f"
        % (summary["loadavg_file"], summary["loadavg"], summary["load_threshold"])
    )
    print(
        "loadgen: pdeathsig=%s clean=%s survivors=%s elapsed=%s"
        % (
            "true" if summary["pdeathsig"] else "false",
            "true" if summary["clean"] else "false",
            survivors,
            summary["elapsed"],
        )
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="loadgen.py",
        description="Bounded, self-cleaning CPU load generator (DF-CRIER-254).",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Exit codes: 0 ok, 1 a child survived teardown, 2 refused "
            "(out-of-cap / bad --cpus / unreadable --loadavg-file), 3 the load "
            "gate skipped the run, 128+signum interrupted.\n"
            "The load gate compares the measured 1-minute load average against "
            "--load-threshold and skips when it is STRICTLY ABOVE it."
        ),
    )
    parser.add_argument(
        "--workers",
        type=int,
        default=DEFAULT_WORKERS,
        help=f"burner processes to start (default {DEFAULT_WORKERS}, hard cap {MAX_WORKERS})",
    )
    parser.add_argument(
        "--seconds",
        type=float,
        default=DEFAULT_SECONDS,
        help=f"run lifetime in seconds (default {DEFAULT_SECONDS:g}, hard cap {MAX_SECONDS:g})",
    )
    parser.add_argument(
        "--cpus",
        default=None,
        metavar="SPEC",
        help="CPU set for the run, e.g. 0-3,7 (must fit inside what the host allows)",
    )
    parser.add_argument(
        "--load-threshold",
        type=float,
        default=DEFAULT_LOAD_THRESHOLD,
        help=f"skip the run when the 1-minute loadavg is above this (default {DEFAULT_LOAD_THRESHOLD:g})",
    )
    parser.add_argument(
        "--loadavg-file",
        default=DEFAULT_LOADAVG_FILE,
        metavar="PATH",
        help=f"where to read the load average from (default {DEFAULT_LOADAVG_FILE}); "
        "override to pin a synthetic value",
    )
    parser.add_argument(
        "--json",
        action="store_true",
        help="print the run summary as one JSON object on stdout",
    )
    return parser


def main(argv: list | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        summary = generate(
            args.workers,
            args.seconds,
            cpus=args.cpus,
            load_threshold=args.load_threshold,
            loadavg_file=args.loadavg_file,
            on_start=_announce_started,
        )
    except LoadSkip as exc:
        print(
            f"SKIPPED: {exc.reason()} — refusing to add load to a host that is already "
            "loaded. Raise --load-threshold only when the box can take it.",
            flush=True,
        )
        return EXIT_LOAD_SKIP
    except Refused as exc:
        print(f"loadgen: ERROR: {exc}", file=sys.stderr)
        return EXIT_REFUSED

    _emit_summary(summary, args.json)
    if summary["survivors"]:
        print(
            "loadgen: ERROR: %d child process(es) SURVIVED teardown: %s"
            % (len(summary["survivors"]), ",".join(str(p) for p in summary["survivors"])),
            file=sys.stderr,
        )
        return EXIT_SURVIVORS
    if summary["interrupted"]:
        print(
            "loadgen: interrupted by signal %d — every child was torn down before exit"
            % summary["interrupted"],
            file=sys.stderr,
        )
        return 128 + int(summary["interrupted"])
    return EXIT_OK


if __name__ == "__main__":  # pragma: no cover - CLI entry
    sys.exit(main())
