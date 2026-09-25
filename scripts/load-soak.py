#!/usr/bin/env python3
"""Bounded capacity soak for crier — the repeatable measurement behind
docs/capacity-ceiling.md (CR-FEAT-033).

WHY THIS EXISTS
---------------
The external hands-on review "DISPATCH · CRI-001" (Carter, 2026-09-24) measured
two real numbers on one box (register 100 agents 0.19 s; 500 inbox deliveries
0.95 s ~= 529 msgs/s) and then wrote the honest sentence: "I did not run a
1000-agent soak; treat the ceiling as unproven."  It also named the structural
bottlenecks — one relay process is the whole throughput, the mesh is a full fan
of live WebSockets per agent, there are no namespaces, and the 100/min/agent
publish cap is the ONLY backpressure — none of which was measured or published.

This harness turns that into a repeatable measurement: it starts a real
``crier`` server on a scratch port, drives register / inbox-deliver / relay
WebSocket fan-out / mesh WebSocket fan against it at 2, 10, 100 and 1000
registered agents, and prints the numbers (plus the honest gaps) as one
machine-readable object.  The published numbers in docs/capacity-ceiling.md are
anchored to the artifact this writes (``--output``), so prose cannot silently
drift from the measurement.

BOUNDED BY CONSTRUCTION — IT CANNOT BECOME A BURN LOOP
------------------------------------------------------
This repo's load rule (DF-CRIER-254, docs/load-reproduction.md) exists because an
unbounded "load the box" shell loop orphaned 278 burners and took a shared host
to loadavg 346.  A capacity soak has exactly that risk shape, so:

* every dimension is capped and an out-of-cap request is REFUSED naming the cap
  (exit 2) — never clamped: agents <= 1000, messages <= 5000, subscribers <=
  1000, events <= 100, mesh connections <= 1000, concurrency <= 8;
* the TOTAL WALL-CLOCK BUDGET is capped (``--budget-seconds``, default 120,
  hard cap 300 = ``loadgen.MAX_SECONDS``) and enforced inside every loop: when it
  expires the run STOPS, records ``truncated: true`` at the stage it reached and
  exits 4 rather than continuing;
* the load-average gate is NOT re-implemented here: it is
  ``scripts/loadgen.py``'s (imported), so the threshold, the strictness and the
  fail-closed unreadable-file behaviour have one implementation (docs/
  load-reproduction.md, "ONE IMPLEMENTATION OF THE GATE");
* the server this harness starts is OWNED: it is spawned with
  ``prctl(PR_SET_PDEATHSIG, SIGKILL)`` so even a ``SIGKILL`` of this process
  cannot leave it running, and teardown is TERM -> wait -> KILL -> VERIFY with a
  survivor check that outranks everything else (exit 1);
* no ``setsid``, no detached process, no shell busy-wait, and no
  ``pkill``/``pgrep -f`` anywhere.

USAGE
-----
    python3 scripts/load-soak.py --agents 2 --messages 500 --json
    python3 scripts/load-soak.py --agents 2,10,100,1000 --messages 500 \
        --events 20 --output docs/soak-baseline.json --load-threshold 12
    make load-soak              # CI-sized smoke (2 agents, seconds not minutes)
    make load-soak-baseline     # regenerate the published artifact (quiet box)

Each profile gets a FRESH server process (cold start, cold registry), so the
per-profile numbers are independent and the agent counts do not accumulate.

WHAT EACH STAGE MEASURES
------------------------
register        POST /agents x N        -> registrations/s, p50/p95 latency
deliver         POST /agents/{id}/inbox -> deliveries/s, p50/p95 latency,
                at least one delivery per registered agent (round-robin)
relay_fanout    S live WebSocket subscribers on one topic, E publishes
                -> frames delivered / expected, fan-out drain time, frames/s
mesh_fan        C live /mesh/connect sockets (each sending a REGISTER frame,
                each opting into the CR-FEAT-023 inbox ping)
                -> sockets/s, GET /mesh/peers count, deliver-to-ping p50/p95
                latency, server RSS growth per socket
rate_limit      R publishes from ONE X-Agent-ID against the shipped 100/min cap
                -> accepted (202) vs rejected (429) — the only backpressure the
                review found, measured instead of asserted

EXIT CODES
----------
    0    every stage completed and passed its own verification
    1    a hard failure: a server that died, a stage error, a survivor after
         teardown, or a mesh peer count that disagrees with the sockets opened
    2    refused/misuse: out-of-cap dimension, bad argument, missing binary, or
         an unreadable load-average file (fail closed)
    3    the load gate skipped the run (1-minute loadavg strictly above
         --load-threshold); the server is never started
    4    the wall-clock budget expired: the run stopped early and the summary
         records ``truncated: true`` (an incomplete measurement is not a clean
         one, and it is never reported as if it were)
    128+N  interrupted by signal N; the server was torn down before exit

DEPENDENCIES: python3 (stdlib only), bash, and a built ``bin/crier``
(``make build``).  No docker, no network beyond localhost, no root.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import http.client
import json
import os
import select
import selectors
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent
sys.path.insert(0, str(HERE))

# The bounded-load gate is NOT re-implemented: caps, threshold and the
# fail-closed unreadable-file behaviour all come from scripts/loadgen.py
# (docs/load-reproduction.md, "ONE IMPLEMENTATION OF THE GATE").
import loadgen  # noqa: E402

# ── caps and defaults (the numbers docs/capacity-ceiling.md quotes) ───────────
MAX_AGENTS = 1000
DEFAULT_AGENTS = 2
MAX_MESSAGES = 5000
DEFAULT_MESSAGES = 500
MAX_SUBSCRIBERS = 1000
MAX_EVENTS = 100
DEFAULT_EVENTS = 20
MAX_MESH_CONNECTIONS = 1000
MAX_CONCURRENCY = 8
DEFAULT_CONCURRENCY = 4
MAX_REQUEST_TIMEOUT_S = 30.0
DEFAULT_REQUEST_TIMEOUT_S = 10.0
DEFAULT_BUDGET_S = 120.0
# The wall-clock ceiling is loadgen's own hard lifetime cap: the soak may never
# outlive what the repo already allows a load run to last.
MAX_BUDGET_S = float(loadgen.MAX_SECONDS)
DEFAULT_LOAD_THRESHOLD = loadgen.DEFAULT_LOAD_THRESHOLD
DEFAULT_LOADAVG_FILE = loadgen.DEFAULT_LOADAVG_FILE
DEFAULT_BINARY = "bin/crier"

# The rate-limit stage drives the shipped default (config.RateLimitPerMinute =
# 100) with a small margin over it, so the cap is OBSERVED rather than assumed.
RATE_PROBE_PUBLISHES = 110
RATE_PROBE_CAP = 200
# How many mesh sockets get an end-to-end deliver-to-ping sample (bounded probe
# of the CR-FEAT-023 path; the fan itself is all of them).
MESH_PING_SAMPLES = 20

# Health/readiness wait for a freshly spawned server, and the port-release check
# after teardown.
SERVER_READY_BUDGET_S = 15.0

EXIT_OK = 0
EXIT_FAILED = 1
EXIT_REFUSED = 2
EXIT_LOAD_SKIP = 3
EXIT_TRUNCATED = 4

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
WS_TEXT = 0x1
WS_CLOSE = 0x8
WS_PING = 0x9
WS_PONG = 0xA


class Interrupted(Exception):
    """A teardown signal arrived; the server is stopped before exit."""

    def __init__(self, signum: int) -> None:
        self.signum = signum
        super().__init__(f"interrupted by signal {signum}")


class BudgetExhausted(Exception):
    """The wall-clock budget expired; the run stops where it is (exit 4)."""


class StageError(Exception):
    """A hard failure inside a stage (exit 1)."""


# ── WebSocket client (stdlib only) ────────────────────────────────────────────


class WSConn:
    """A minimal RFC 6455 client: upgrade, masked text frames, unmasked reads.

    Only what the soak needs is implemented (text/close/ping/pong).  The socket
    is non-blocking after the handshake so a stage can watch hundreds of them
    from one selector loop instead of one thread per socket.
    """

    def __init__(self, sock: socket.socket, leftover: bytes = b"") -> None:
        self.sock = sock
        self.buf = bytearray(leftover)
        self.closed = False
        self.eof = False

    # -- writing ------------------------------------------------------------
    def send_text(self, payload: bytes) -> None:
        self._send_frame(WS_TEXT, payload)

    def _send_frame(self, opcode: int, payload: bytes) -> None:
        header = bytearray([0x80 | opcode])
        length = len(payload)
        if length < 126:
            header.append(0x80 | length)
        elif length < 65536:
            header.append(0x80 | 126)
            header += struct.pack("!H", length)
        else:
            header.append(0x80 | 127)
            header += struct.pack("!Q", length)
        mask = os.urandom(4)
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + masked)

    # -- reading ------------------------------------------------------------
    def feed(self, data: bytes) -> list[tuple[int, bytes]]:
        """Append bytes and return every COMPLETE frame parsed so far."""
        self.buf += data
        frames: list[tuple[int, bytes]] = []
        while True:
            frame = self._try_parse()
            if frame is None:
                return frames
            frames.append(frame)

    def _try_parse(self) -> tuple[int, bytes] | None:
        buf = self.buf
        if len(buf) < 2:
            return None
        b0, b1 = buf[0], buf[1]
        opcode = b0 & 0x0F
        masked = bool(b1 & 0x80)
        length = b1 & 0x7F
        offset = 2
        if length == 126:
            if len(buf) < offset + 2:
                return None
            length = struct.unpack("!H", bytes(buf[offset : offset + 2]))[0]
            offset += 2
        elif length == 127:
            if len(buf) < offset + 8:
                return None
            length = struct.unpack("!Q", bytes(buf[offset : offset + 8]))[0]
            offset += 8
        mask_key = b""
        if masked:
            if len(buf) < offset + 4:
                return None
            mask_key = bytes(buf[offset : offset + 4])
            offset += 4
        if len(buf) < offset + length:
            return None
        payload = bytes(buf[offset : offset + length])
        if masked:
            payload = bytes(b ^ mask_key[i % 4] for i, b in enumerate(payload))
        del buf[: offset + length]
        return opcode, payload

    def drain(self, timeout: float) -> list[tuple[int, bytes]]:
        """Read whatever is readable within ``timeout``; return new frames.

        A CLOSE frame (or EOF) marks the connection closed and is reported as a
        frame so the caller can account for it honestly.
        """
        if self.eof:
            return []
        frames: list[tuple[int, bytes]] = []
        ready, _, _ = select.select([self.sock], [], [], max(0.0, timeout))
        if not ready:
            return frames
        try:
            data = self.sock.recv(65536)
        except (ConnectionResetError, BlockingIOError):
            data = b""
        if not data:
            self.eof = True
            self.closed = True
            return frames
        frames = self.feed(data)
        for opcode, _payload in frames:
            if opcode == WS_CLOSE:
                self.closed = True
                break
            if opcode == WS_PING:
                try:
                    self._send_frame(WS_PONG, b"")
                except OSError:
                    self.closed = True
        return frames

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            pass
        self.closed = True


def ws_handshake(sock: socket.socket, path: str, host_header: str, extra_headers: dict) -> bytes:
    """Perform the RFC 6455 upgrade; return any bytes past the header block."""
    key = base64.b64encode(os.urandom(16)).decode("ascii")
    lines = [
        f"GET {path} HTTP/1.1",
        f"Host: {host_header}",
        "Upgrade: websocket",
        "Connection: Upgrade",
        f"Sec-WebSocket-Key: {key}",
        "Sec-WebSocket-Version: 13",
    ]
    for name, value in extra_headers.items():
        lines.append(f"{name}: {value}")
    request = ("\r\n".join(lines) + "\r\n\r\n").encode("ascii")
    sock.sendall(request)

    raw = bytearray()
    deadline = time.monotonic() + 10.0
    while b"\r\n\r\n" not in raw:
        if time.monotonic() > deadline:
            raise StageError(f"WebSocket handshake for {path} timed out before the response headers")
        chunk = sock.recv(4096)
        if not chunk:
            raise StageError(f"WebSocket handshake for {path} closed before the response headers")
        raw += chunk
    head, _, rest = bytes(raw).partition(b"\r\n\r\n")
    status_line = head.split(b"\r\n", 1)[0].decode("latin-1")
    if "101" not in status_line.split(" ")[:2]:
        raise StageError(f"WebSocket handshake for {path} answered {status_line!r}, want 101")
    expect = base64.b64encode(hashlib.sha1((key + WS_GUID).encode("ascii")).digest()).decode("ascii")
    if f"sec-websocket-accept: {expect}".lower() not in head.decode("latin-1").lower():
        raise StageError(f"WebSocket handshake for {path} carried no matching Sec-WebSocket-Accept")
    return rest


def ws_connect(port: int, path: str, *, timeout: float, headers: dict | None = None) -> WSConn:
    """Dial a WebSocket, upgrade, then leave the socket non-blocking."""
    sock = socket.create_connection(("127.0.0.1", port), timeout=timeout)
    sock.settimeout(timeout)
    try:
        leftover = ws_handshake(sock, path, f"127.0.0.1:{port}", headers or {})
    except Exception:
        sock.close()
        raise
    sock.setblocking(False)
    return WSConn(sock, leftover)


def wait_for_frame(conn: WSConn, timeout: float) -> list[tuple[int, bytes]]:
    """Drain until at least one frame arrives or ``timeout`` expires."""
    deadline = time.monotonic() + timeout
    frames: list[tuple[int, bytes]] = []
    while time.monotonic() < deadline:
        got = conn.drain(min(0.05, max(0.0, deadline - time.monotonic())))
        if got:
            frames.extend(got)
            return frames
        if conn.eof:
            return frames
    return frames


# ── HTTP (stdlib only) ────────────────────────────────────────────────────────

_thread_local = threading.local()


def http_request(port: int, method: str, path: str, body: bytes | None, timeout: float, headers: dict | None = None) -> tuple[int, bytes]:
    """One HTTP request over a per-thread keep-alive connection.

    A stale keep-alive connection is retried ONCE on a fresh connection: this is
    the only retry in the harness and it exists so an idle connection the server
    closed cannot be reported as a failed measurement.
    """
    attempts = 2
    last: Exception | None = None
    for attempt in range(attempts):
        conn = getattr(_thread_local, "conn", None)
        if conn is None:
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
            _thread_local.conn = conn
        try:
            hdrs = {"Content-Type": "application/json"}
            if headers:
                hdrs.update(headers)
            conn.request(method, path, body=body, headers=hdrs)
            resp = conn.getresponse()
            payload = resp.read()
            return resp.status, payload
        except Exception as exc:  # noqa: BLE001 - retried once, then surfaced
            last = exc
            try:
                conn.close()
            except Exception:  # noqa: BLE001 - best effort
                pass
            _thread_local.conn = None
            if attempt + 1 >= attempts:
                break
    raise StageError(f"{method} {path} failed: {last}")


# ── the server under test ─────────────────────────────────────────────────────


def free_port() -> int:
    """Ask the kernel for a free ephemeral port on loopback.

    The port is released before the server binds it (the usual small race); the
    harness re-checks ownership right after startup, so a stolen port is caught
    rather than measured.
    """
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def port_has_listener(port: int) -> bool:
    """True when ANY process is LISTENing on ``port`` (procfs, not a bind test).

    A bind test is the wrong instrument after teardown: a connection in
    TIME_WAIT makes the port unbindable without SO_REUSEADDR even though nothing
    is listening, which would report a clean stop as a leak.  What must be gone
    is the LISTENER.
    """
    return bool(port_listen_inodes(port))


def port_listen_inodes(port: int) -> set[str]:
    """Inodes of the LISTEN sockets bound to ``port``, from procfs.

    Read from /proc/net/tcp AND /proc/net/tcp6, with no ss/lsof dependency: Go's
    ``Listen`` on ``:port`` commonly creates a DUAL-STACK socket, and the socket
    that actually answers 127.0.0.1 may be listed only in the v6 table.  Any
    local address is accepted here because the ownership check that follows is
    what makes the entry ours — a listener on this port number that we do not
    hold is exactly the hazard this check exists for.
    """
    suffix = f":{port:04X}"
    inodes: set[str] = set()
    for table in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            with open(table, "r", encoding="utf-8") as handle:
                next(handle, None)
                for line in handle:
                    fields = line.split()
                    if len(fields) < 10:
                        continue
                    local, state = fields[1], fields[3]
                    if state != "0A":  # 0A = TCP_LISTEN
                        continue
                    if local.endswith(suffix):
                        inodes.add(fields[9])
        except OSError:
            continue
    return inodes


def pid_owns_socket(pid: int, inode: str) -> bool:
    """True when ``pid`` holds the socket ``inode`` open."""
    fd_dir = f"/proc/{pid}/fd"
    try:
        for name in os.listdir(fd_dir):
            try:
                target = os.readlink(os.path.join(fd_dir, name))
            except OSError:
                continue
            if target == f"socket:[{inode}]":
                return True
    except OSError:
        return False
    return False


def pid_alive(pid: int) -> bool:
    """A zombie is dead here (same rule loadgen.py applies to its burners)."""
    return loadgen.pid_alive(pid)


def rss_kb(pid: int) -> int | None:
    try:
        with open(f"/proc/{pid}/status", "r", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1])
    except (OSError, ValueError):
        return None
    return None


def _arm_parent_death_signal() -> None:
    """preexec hook: the kernel kills this child when the harness dies.

    Without it, a ``SIGKILL`` of the harness (the case no ``finally`` can cover)
    could leave a crier server holding a port.  Best effort: where the mechanism
    is unavailable the ordinary TERM/KILL teardown still applies.
    """
    try:
        import ctypes

        libc = ctypes.CDLL("libc.so.6", use_errno=True)
        libc.prctl(loadgen.PR_SET_PDEATHSIG, 9, 0, 0, 0)  # SIGKILL
    except Exception:  # noqa: BLE001 - the teardown below is the guarantee
        pass


class ServerUnderTest:
    """A crier server this harness starts, owns and tears down."""

    def __init__(self, binary: str, log_path: str) -> None:
        self.binary = binary
        self.log_path = log_path
        self.proc: subprocess.Popen | None = None
        self.port: int | None = None
        self.version: str = ""
        self.posture: dict = {}
        self.log_handle = None

    def start(self, port: int, timeout: float) -> None:
        env = os.environ.copy()
        env["CRIER_PORT"] = str(port)
        # The measured posture is the SHIPPED DEFAULT one: no shared secret, the
        # in-memory registry, the 100/min publish cap.  Anything that would
        # change what is measured is removed from the environment rather than
        # assumed absent.
        for name in (
            "CR_AUTH_TOKEN",
            "CR_DATABASE_URL",
            "CR_RATE_LIMIT_PER_MINUTE",
            "CR_REQUIRE_MESH_AUTH",
            "CR_WS_ALLOWED_ORIGINS",
            "CR_PIDFILE",
        ):
            env.pop(name, None)
        env["CR_LOG_LEVEL"] = "warn"

        self.log_handle = open(self.log_path, "wb")
        self.proc = subprocess.Popen(
            [self.binary, "-port", str(port)],
            stdout=self.log_handle,
            stderr=subprocess.STDOUT,
            stdin=subprocess.DEVNULL,
            env=env,
            preexec_fn=_arm_parent_death_signal,
            start_new_session=False,
        )
        self.port = port

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                raise StageError(
                    f"{self.binary} exited with rc={self.proc.returncode} before answering /health; "
                    f"see {self.log_path}"
                )
            try:
                status, body = http_request(port, "GET", "/health", None, 2.0)
                if status == 200 and b"ok" in body:
                    break
            except Exception:  # noqa: BLE001 - readiness is a poll
                pass
            time.sleep(0.05)
        else:
            raise StageError(f"{self.binary} did not answer /health within {timeout:g}s; see {self.log_path}")

        # Ownership assertion (the QA-CRIER-9 rule for demo harnesses): the
        # process answering on this port must be the one we started.  A port we
        # did not bind would make every number below someone else's.
        if not pid_alive(self.proc.pid):
            raise StageError("the server process died immediately after answering /health")
        inodes = port_listen_inodes(port)
        if not inodes:
            raise StageError(f"no LISTEN socket on port {port} found in procfs after startup")
        if not any(pid_owns_socket(self.proc.pid, inode) for inode in inodes):
            raise StageError(
                f"port {port} is held by a socket pid {self.proc.pid} does not own "
                "(another process answered) — refusing to measure it"
            )

        try:
            _status, body = http_request(port, "GET", "/version", None, 5.0)
            self.version = json.loads(body).get("version", "") if body else ""
        except Exception:  # noqa: BLE001 - reported, never fatal
            self.version = ""
        try:
            _status, body = http_request(port, "GET", "/status", None, 5.0)
            self.posture = json.loads(body) if body else {}
        except Exception:  # noqa: BLE001 - reported, never fatal
            self.posture = {}

    def stop(self) -> bool:
        """TERM -> wait -> KILL -> VERIFY.  Returns True when nothing survives."""
        if self.proc is None:
            return True
        pid = self.proc.pid
        if pid_alive(pid):
            try:
                self.proc.terminate()
            except OSError:
                pass
        try:
            self.proc.wait(timeout=5.0)
        except subprocess.TimeoutExpired:
            try:
                self.proc.kill()
            except OSError:
                pass
            try:
                self.proc.wait(timeout=5.0)
            except subprocess.TimeoutExpired:
                pass
        if self.log_handle is not None:
            try:
                self.log_handle.close()
            except OSError:
                pass
            self.log_handle = None
        return not pid_alive(pid)

    def rss_kb(self) -> int | None:
        return rss_kb(self.proc.pid) if self.proc is not None else None


# ── argument validation ───────────────────────────────────────────────────────


def parse_profiles(spec: str) -> list[int]:
    profiles: list[int] = []
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            value = int(part)
        except ValueError:
            raise loadgen.Refused(f"--agents {spec!r}: {part!r} is not an integer") from None
        if value < 1:
            raise loadgen.Refused(f"--agents {spec!r}: every profile must be >= 1 agent, got {value}")
        if value > MAX_AGENTS:
            _cap_violation(
                f"--agents {value} is above the hard cap of {MAX_AGENTS} agents "
                "(this harness is bounded; CR-FEAT-033) — lower the profile, or raise "
                "MAX_AGENTS deliberately with the measured numbers to back it"
            )
        profiles.append(value)
    if not profiles:
        raise loadgen.Refused("--agents selected no profile")
    return profiles


def _cap_violation(message: str) -> None:
    """Refuse an out-of-cap request (exit 2).

    One named call for every cap, so the selftest's NEUTER proof can force THIS
    verdict to a no-op by editing one line: if the over-cap request then proceeds,
    the refusal we assert on is genuinely produced here and not by accident.
    """
    raise loadgen.Refused(message)


def _check_cap(name: str, value: int, cap: int, why: str) -> None:
    if value < 0:
        _cap_violation(f"{name} must not be negative, got {value}")
    if value > cap:
        _cap_violation(
            f"{name} {value} is above the hard cap of {cap} ({why}; this harness is "
            "bounded by construction — CR-FEAT-033)"
        )


def _parse_count(flag: str, spec: str) -> int:
    """Parse a count flag ('12' or 'auto' handled by the caller) or refuse."""
    try:
        return int(str(spec).strip())
    except ValueError:
        raise loadgen.Refused(f"{flag} {spec!r} is not an integer") from None


# ── stages ────────────────────────────────────────────────────────────────────


def _latency_summary(samples: list[float]) -> dict:
    if not samples:
        return {"p50_ms": None, "p95_ms": None, "max_ms": None}
    ordered = sorted(samples)
    return {
        "p50_ms": round(ordered[len(ordered) // 2] * 1000, 2),
        "p95_ms": round(ordered[min(len(ordered) - 1, int(len(ordered) * 0.95))] * 1000, 2),
        "max_ms": round(ordered[-1] * 1000, 2),
    }


def _run_bounded(items: list, fn, concurrency: int, deadline: float) -> tuple[list, int]:
    """Run ``fn`` over ``items`` with a bounded pool and a wall-clock deadline.

    Returns (results, unattempted).  The deadline is checked inside the worker,
    so the run stops mid-stage instead of draining the whole list after the
    budget is gone.
    """
    results: list = []
    lock = threading.Lock()
    attempted = 0

    def wrapped(item):
        nonlocal attempted
        if time.monotonic() > deadline:
            raise BudgetExhausted()
        with lock:
            attempted += 1
        return fn(item)

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = [pool.submit(wrapped, item) for item in items]
        for future in futures:
            try:
                results.append(future.result())
            except BudgetExhausted:
                for other in futures:
                    other.cancel()
                break
            except StageError:
                raise
    return results, max(0, len(items) - attempted)


def stage_register(ctx: dict, agents: int) -> dict:
    ids = [f"{ctx['prefix']}-{i:04d}" for i in range(agents)]
    key = "ab" * 32
    started = time.monotonic()
    latencies: list[float] = []

    def one(agent_id: str):
        body = json.dumps({"id": agent_id, "public_key": key, "capabilities": ["soak"]}).encode()
        t0 = time.monotonic()
        status, payload = http_request(ctx["port"], "POST", "/agents", body, ctx["timeout"])
        latencies.append(time.monotonic() - t0)
        if status != 201:
            raise StageError(f"POST /agents for {agent_id} answered {status}: {payload[:200]!r}")
        return agent_id

    results, unattempted = _run_bounded(ids, one, ctx["concurrency"], ctx["deadline"])
    elapsed = time.monotonic() - started
    return {
        "stage": "register",
        "attempted": len(results),
        "requested": agents,
        "unattempted": unattempted,
        "ok": len(results),
        "elapsed_s": round(elapsed, 3),
        "per_s": round(len(results) / elapsed, 1) if elapsed > 0 and results else 0.0,
        **_latency_summary(latencies),
        "agent_ids": ids[: len(results)],
    }


def stage_deliver(ctx: dict, agent_ids: list[str], messages: int, concurrency: int | None = None) -> dict:
    """Inbox deliveries, round-robin, at least one per registered agent."""
    if not agent_ids:
        return {"stage": "deliver", "requested": 0, "attempted": 0, "ok": 0, "skipped": "no registered agents"}
    targets = [agent_ids[i % len(agent_ids)] for i in range(messages)]
    payload = json.dumps({"payload": {"soak": True}, "sender": "soak-sender"}).encode()
    started = time.monotonic()
    latencies: list[float] = []
    workers = concurrency if concurrency is not None else ctx["concurrency"]

    def one(agent_id: str):
        t0 = time.monotonic()
        status, body = http_request(ctx["port"], "POST", f"/agents/{agent_id}/inbox", payload, ctx["timeout"])
        latencies.append(time.monotonic() - t0)
        if status != 201:
            raise StageError(f"POST /agents/{agent_id}/inbox answered {status}: {body[:200]!r}")
        return agent_id

    results, unattempted = _run_bounded(targets, one, workers, ctx["deadline"])
    elapsed = time.monotonic() - started
    return {
        "stage": "deliver",
        "concurrency": workers,
        "requested": messages,
        "attempted": len(results),
        "unattempted": unattempted,
        "ok": len(results),
        "elapsed_s": round(elapsed, 3),
        "per_s": round(len(results) / elapsed, 1) if elapsed > 0 and results else 0.0,
        **_latency_summary(latencies),
    }


def stage_relay_fanout(ctx: dict, agent_ids: list[str], subscribers: int, events: int) -> dict:
    """S subscribers on one topic, E publishes, every frame counted."""
    topic = f"{ctx['prefix']}-fanout"
    subs: list[WSConn] = []
    connect_started = time.monotonic()
    connect_errors = 0
    for i in range(subscribers):
        if time.monotonic() > ctx["deadline"]:
            break
        subscriber_id = f"{agent_ids[i % len(agent_ids)]}-sub{i:04d}" if agent_ids else f"soak-sub{i:04d}"
        try:
            subs.append(
                ws_connect(
                    ctx["port"],
                    f"/relay/subscribe/{topic}",
                    timeout=ctx["timeout"],
                    headers={"X-Agent-ID": subscriber_id},
                )
            )
        except Exception:  # noqa: BLE001 - counted, reported, never silent
            connect_errors += 1
    connect_elapsed = time.monotonic() - connect_started

    frames_per_socket = [0] * len(subs)
    completed_at: list[float | None] = [None] * len(subs)
    drain_started = None
    publish_latencies: list[float] = []
    published = 0
    publisher_ids = [f"{ctx['prefix']}-pub{i:02d}" for i in range(max(1, min(len(agent_ids) or 1, events)))]

    if subs:
        selector = selectors.DefaultSelector()
        for index, conn in enumerate(subs):
            selector.register(conn.sock, selectors.EVENT_READ, index)
        try:
            for seq in range(events):
                if time.monotonic() > ctx["deadline"]:
                    break
                body = json.dumps({"topic": topic, "event": {"seq": seq, "soak": True}}).encode()
                publisher = publisher_ids[seq % len(publisher_ids)]
                t0 = time.monotonic()
                status, payload = http_request(
                    ctx["port"], "POST", "/relay/publish", body, ctx["timeout"],
                    headers={"X-Agent-ID": publisher},
                )
                publish_latencies.append(time.monotonic() - t0)
                if status != 202:
                    raise StageError(f"POST /relay/publish answered {status}: {payload[:200]!r}")
                published += 1
                if drain_started is None:
                    drain_started = time.monotonic()

            # Drain the fan: every subscriber should see every published event.
            target = published
            drain_deadline = min(ctx["deadline"], time.monotonic() + max(5.0, ctx["timeout"]))
            while time.monotonic() < drain_deadline:
                if all(count >= target for count in frames_per_socket):
                    break
                for key, _events in selector.select(timeout=0.05):
                    index = key.data
                    conn = subs[index]
                    for opcode, _payload in conn.drain(0.0):
                        if opcode == WS_TEXT:
                            frames_per_socket[index] += 1
                            if completed_at[index] is None and frames_per_socket[index] >= target:
                                completed_at[index] = time.monotonic()
        finally:
            selector.close()
            for conn in subs:
                conn.close()

    drain_elapsed = None
    if drain_started is not None and any(t is not None for t in completed_at):
        last_complete = max(t for t in completed_at if t is not None)
        drain_elapsed = last_complete - drain_started
    frames_received = sum(frames_per_socket)
    sockets_complete = sum(1 for count in frames_per_socket if count >= published and published > 0)
    return {
        "stage": "relay_fanout",
        "topic": topic,
        "subscribers_requested": subscribers,
        "subscribers_connected": len(subs),
        "connect_errors": connect_errors,
        "connect_s": round(connect_elapsed, 3),
        "events_published": published,
        "frames_expected": published * len(subs),
        "frames_received": frames_received,
        "sockets_complete": sockets_complete,
        "all_sockets_complete": bool(subs) and sockets_complete == len(subs),
        "drain_s": round(drain_elapsed, 3) if drain_elapsed is not None else None,
        "frames_per_s": round(frames_received / drain_elapsed, 1) if drain_elapsed and drain_elapsed > 0 else None,
        **_latency_summary(publish_latencies),
    }


def stage_mesh_fan(ctx: dict, agent_ids: list[str], connections: int) -> dict:
    """Live mesh WebSockets per agent + the CR-FEAT-023 deliver-to-ping latency."""
    # Each socket is paired with the agent id it connected AS: a failed connect
    # must not shift the pairing, or a ping would be aimed at the wrong socket.
    conns: list[tuple[WSConn, str]] = []
    started = time.monotonic()
    connect_errors = 0
    for i in range(connections):
        if time.monotonic() > ctx["deadline"]:
            break
        agent_id = agent_ids[i % len(agent_ids)] if agent_ids else f"{ctx['prefix']}-mesh{i:04d}"
        try:
            conn = ws_connect(
                ctx["port"],
                f"/mesh/connect/{agent_id}?inbox_notify=1",
                timeout=ctx["timeout"],
            )
            envelope = {
                "type": "REGISTER",
                "version": 1,
                "message_id": f"soak-{i:04d}",
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "agent_id": agent_id,
                "lease_id": f"soak-lease-{i:04d}",
                "lease_ttl_ms": 60000,
                "capabilities": {"version": "1", "topics": [], "max_concurrent_sessions": 1},
            }
            conn.send_text((json.dumps(envelope) + "\n").encode())
            conns.append((conn, agent_id))
        except Exception:  # noqa: BLE001 - counted, reported, never silent
            connect_errors += 1
    connect_elapsed = time.monotonic() - started

    status, body = http_request(ctx["port"], "GET", "/mesh/peers", None, ctx["timeout"])
    peers_immediate = json.loads(body).get("count", 0) if status == 200 and body else 0
    # The connect handler writes the 101 upgrade BEFORE it admits the peer, so a
    # client that reads the handshake can be a moment ahead of the peer table —
    # a scheduling race that widens on a loaded box (measured: 99/100 at
    # loadavg 27).  Poll until the table has caught up, and report both the
    # immediate reading and how long the fan took to settle: that settle time is
    # itself a property of the fan under test.
    settle_started = time.monotonic()
    peers = peers_immediate
    peers_settle_deadline = settle_started + max(2.0, ctx["timeout"])
    while peers < len(conns) and time.monotonic() < peers_settle_deadline:
        time.sleep(0.02)
        status, body = http_request(ctx["port"], "GET", "/mesh/peers", None, ctx["timeout"])
        peers = json.loads(body).get("count", 0) if status == 200 and body else peers
    peers_settle_s = round(time.monotonic() - settle_started, 3) if peers > peers_immediate else None

    # The ping samples go to the first N mesh sockets, each of which opted into
    # its own agent's inbox ping: the delivery is HTTP, the ping is the frame the
    # server writes back on the live socket (CR-FEAT-023).
    samples: list[float] = []
    ping_errors = 0
    for index in range(min(MESH_PING_SAMPLES, len(conns))):
        if time.monotonic() > ctx["deadline"]:
            break
        conn, agent_id = conns[index]
        payload = json.dumps({"payload": {"soak_ping": index}, "sender": "soak-sender"}).encode()
        t0 = time.monotonic()
        status, _body = http_request(ctx["port"], "POST", f"/agents/{agent_id}/inbox", payload, ctx["timeout"])
        if status != 201:
            ping_errors += 1
            continue
        got_ping = False
        while time.monotonic() - t0 < ctx["timeout"]:
            for opcode, frame in conn.drain(0.02):
                if opcode == WS_TEXT and b"INBOX_NOTIFY" in frame:
                    samples.append(time.monotonic() - t0)
                    got_ping = True
                    break
            if got_ping:
                break
        if not got_ping:
            ping_errors += 1

    for conn, _agent_id in conns:
        conn.close()

    return {
        "stage": "mesh_fan",
        "connections_requested": connections,
        "connections_ok": len(conns),
        "connect_errors": connect_errors,
        "connect_s": round(connect_elapsed, 3),
        "connections_per_s": round(len(conns) / connect_elapsed, 1) if connect_elapsed > 0 and conns else None,
        "peers_reported": peers,
        "peers_immediate": peers_immediate,
        "peers_settle_s": peers_settle_s,
        "ping_samples": len(samples),
        "ping_errors": ping_errors,
        "ping_p50_ms": round(sorted(samples)[len(samples) // 2] * 1000, 2) if samples else None,
        "ping_p95_ms": round(sorted(samples)[min(len(samples) - 1, int(len(samples) * 0.95))] * 1000, 2) if samples else None,
    }


def stage_rate_limit(ctx: dict, publishes: int) -> dict:
    """The shipped 100/min per-agent publish cap, measured instead of assumed."""
    topic = f"{ctx['prefix']}-rate-probe"
    probe_id = f"{ctx['prefix']}-rl-probe"
    accepted = 0
    rejected = 0
    other = 0
    started = time.monotonic()
    for seq in range(publishes):
        if time.monotonic() > ctx["deadline"]:
            break
        body = json.dumps({"topic": topic, "event": {"seq": seq, "soak": True}}).encode()
        status, _payload = http_request(
            ctx["port"], "POST", "/relay/publish", body, ctx["timeout"],
            headers={"X-Agent-ID": probe_id},
        )
        if status == 202:
            accepted += 1
        elif status == 429:
            rejected += 1
        else:
            other += 1
    elapsed = time.monotonic() - started
    return {
        "stage": "rate_limit",
        "probe_agent_id": probe_id,
        "publishes": publishes,
        "accepted": accepted,
        "rejected_429": rejected,
        "other_status": other,
        "elapsed_s": round(elapsed, 3),
        "cap_observed": accepted > 0 and rejected > 0,
        "rate_limit_per_minute": ctx["posture"].get("rate_limit_per_minute"),
    }


# ── one profile ───────────────────────────────────────────────────────────────


def run_profile(
    *,
    agents: int,
    messages: int,
    subscribers: int,
    events: int,
    mesh_connections: int,
    binary: str,
    workdir: Path,
    timeout: float,
    concurrency: int,
    budget_s: float,
    profile_deadline: float,
    sequential_probe: bool = False,
) -> dict:
    port = free_port()
    log_path = str(workdir / f"crier-soak-{agents}.log")
    server = ServerUnderTest(binary, log_path)
    run: dict = {
        "agents": agents,
        "port": port,
        "server_pid": None,
        "truncated": False,
        "truncated_at": None,
        "stages": {},
    }
    started = time.monotonic()
    deadline = min(profile_deadline, started + budget_s)
    try:
        server.start(port, SERVER_READY_BUDGET_S)
        run["binary_version"] = server.version
        run["server_pid"] = server.proc.pid if server.proc is not None else None
        run["posture"] = {
            "auth": "disabled" if not server.posture.get("auth_enabled", False) else "enabled",
            "auth_required": server.posture.get("require_agent_sig"),
            "registry_backend": server.posture.get("registry_backend"),
            "rate_limit_per_minute": server.posture.get("rate_limit_per_minute"),
        }
        run["rss_kb_after_start"] = server.rss_kb()

        ctx = {
            "port": port,
            "timeout": timeout,
            "concurrency": concurrency,
            "deadline": deadline,
            "prefix": "soak",
            "posture": run["posture"],
        }

        register = stage_register(ctx, agents)
        run["stage_order"] = ["register"]
        run["stages"]["register"] = register
        run["rss_kb_after_register"] = server.rss_kb()
        agent_ids = register.get("agent_ids", [])
        if not agent_ids:
            if register.get("attempted", 0) == 0:
                # The budget expired before a single registration: that is a
                # BOUNDED STOP (exit 4), not a broken server (exit 1).
                run["truncated"] = True
                run["truncated_at"] = "register"
            else:
                raise StageError("register stage produced no agents — nothing to measure")
        if register.get("unattempted") and not run["truncated"]:
            # A register stage cut short by the budget measured a DIFFERENT
            # population than the profile claims, so the profile stops here
            # instead of publishing numbers for an agent count it never reached.
            run["truncated"] = True
            run["truncated_at"] = "register"

        # Stages run in order and each one is abandonable: a stage that runs out
        # of wall-clock budget stops where it is and the run is marked truncated
        # rather than allowed to continue (the bound is the point).
        plan = [
            ("deliver", lambda: stage_deliver(ctx, agent_ids, max(messages, agents))),
            ("relay_fanout", lambda: stage_relay_fanout(ctx, agent_ids, subscribers, events)),
            ("mesh_fan", lambda: stage_mesh_fan(ctx, agent_ids, mesh_connections)),
            ("rate_limit", lambda: stage_rate_limit(ctx, RATE_PROBE_PUBLISHES)),
        ]
        for name, runner in plan:
            if run["truncated"]:
                break
            try:
                run["stages"][name] = runner()
                run["stage_order"].append(name)
            except BudgetExhausted:
                run["truncated"] = True
                run["truncated_at"] = name
                break
            if name == "mesh_fan":
                run["rss_kb_after_mesh"] = server.rss_kb()
            if run["stages"][name].get("unattempted"):
                run["truncated"] = True
                run["truncated_at"] = name
                break
            if time.monotonic() >= deadline:
                # A stage that spent the whole wall-clock budget is a BOUNDED
                # STOP even when it returned a clean-looking dict: whatever
                # follows would be measured on a different (time-shifted)
                # schedule, so the profile is marked incomplete and stops here.
                run["truncated"] = True
                run["truncated_at"] = name
                break

        # The reviewer's method, replayed: the SAME deliveries from ONE worker on
        # one keep-alive connection.  It exists so the published numbers state
        # their own method — a concurrent figure and a sequential figure for the
        # same endpoint differ by an order of magnitude on localhost.
        if sequential_probe and not run["truncated"]:
            try:
                run["stages"]["deliver_sequential"] = stage_deliver(
                    ctx, agent_ids, max(messages, agents), concurrency=1
                )
                run["stage_order"].append("deliver_sequential")
            except BudgetExhausted:
                run["truncated"] = True
                run["truncated_at"] = "deliver_sequential"

        run["rss_kb_at_end"] = server.rss_kb()

        # Verification beyond "it returned 200": the peer table must name every
        # socket we opened, and every fan-out subscriber we counted must have
        # been connected.
        mesh = run["stages"].get("mesh_fan")
        if mesh and mesh["connections_ok"] and mesh["peers_reported"] < mesh["connections_ok"]:
            raise StageError(
                f"mesh peer count {mesh['peers_reported']} is below the {mesh['connections_ok']} sockets opened"
            )
        fan = run["stages"].get("relay_fanout")
        if fan and fan["subscribers_requested"] and fan["subscribers_connected"] == 0:
            raise StageError("no relay subscriber could connect — the fan-out number would be meaningless")
    finally:
        clean = server.stop()
        run["server_stopped_clean"] = clean
        run["port_listener_gone"] = not port_has_listener(port)
        run["elapsed_s"] = round(time.monotonic() - started, 3)
    return run


# ── claims the doc anchors on (the harness OWNS the strings it publishes) ─────


def claims_for(run: dict, prefix: str = "COUNT-SOAK") -> dict:
    """The exact display strings docs/capacity-ceiling.md prints, keyed by claim id.

    The doc must carry these strings verbatim and docs/claims.yaml pins each
    claim's `expect` to the matching `value` below, so a doc edit that changes a
    number without re-measuring fails `make docs-check` (CR-GAP-065 precedent).
    ``prefix`` keeps a second artifact (the under-load run) from colliding with
    the primary one — its claim ids are its own.
    """
    agents = run["agents"]
    stages = run.get("stages", {})
    claims: dict[str, dict] = {}
    register = stages.get("register")
    if register and register.get("ok"):
        value = f"{register['elapsed_s']:.3f}"
        claims[f"{prefix}-REGISTER-{agents}"] = {
            "value": value,
            "quote": f"{agents} agents registered in {value} s",
        }
    deliver = stages.get("deliver")
    if deliver and deliver.get("ok"):
        value = f"{deliver['per_s']:.0f}"
        claims[f"{prefix}-DELIVER-{agents}"] = {
            "value": value,
            "quote": f"{value} inbox deliveries/s",
        }
    sequential = stages.get("deliver_sequential")
    if sequential and sequential.get("ok"):
        value = f"{sequential['per_s']:.0f}"
        claims[f"{prefix}-DELIVER-SEQ-{agents}"] = {
            "value": value,
            "quote": f"{value} inbox deliveries/s from one sequential client",
        }
    fan = stages.get("relay_fanout")
    if fan and fan.get("frames_per_s"):
        value = f"{fan['frames_per_s']:.0f}"
        claims[f"{prefix}-FANOUT-{agents}"] = {
            "value": value,
            "quote": f"{value} relay fan-out frames/s",
        }
    mesh = stages.get("mesh_fan")
    if mesh and mesh.get("connections_ok"):
        claims[f"{prefix}-MESH-{agents}"] = {
            "value": str(mesh["connections_ok"]),
            "quote": f"{mesh['connections_ok']} live mesh sockets",
        }
    if mesh and mesh.get("ping_p95_ms") is not None:
        value = f"{mesh['ping_p95_ms']:.2f}"
        claims[f"{prefix}-PING-{agents}"] = {
            "value": value,
            "quote": f"inbox ping p95 {value} ms",
        }
    rate = stages.get("rate_limit")
    if rate and rate.get("cap_observed"):
        claims[f"{prefix}-RATE-{agents}"] = {
            "value": str(rate["accepted"]),
            "quote": f"{rate['accepted']} relay publishes accepted",
        }
    return claims


# ── reporting ─────────────────────────────────────────────────────────────────


def human_report(summary: dict) -> None:
    for run in summary["runs"]:
        print(f"soak: agents={run['agents']} port={run['port']} elapsed={run['elapsed_s']}s "
              f"truncated={run['truncated']}")
        for name in run.get("stage_order", []):
            stage = run["stages"][name]
            if name == "register":
                print(f"soak:   register   {stage['ok']}/{stage['requested']} in {stage['elapsed_s']}s "
                      f"= {stage['per_s']}/s (p50 {stage['p50_ms']}ms p95 {stage['p95_ms']}ms)")
            elif name == "deliver":
                print(f"soak:   deliver    {stage['ok']}/{stage['requested']} in {stage['elapsed_s']}s "
                      f"= {stage['per_s']}/s (p50 {stage['p50_ms']}ms p95 {stage['p95_ms']}ms) "
                      f"with {stage.get('concurrency')} workers")
            elif name == "deliver_sequential":
                print(f"soak:   deliver-1w {stage['ok']}/{stage['requested']} in {stage['elapsed_s']}s "
                      f"= {stage['per_s']}/s (single connection, the reviewer's method)")
            elif name == "relay_fanout":
                print(f"soak:   fanout     {stage['subscribers_connected']} subscribers, "
                      f"{stage['events_published']} events -> {stage['frames_received']}/"
                      f"{stage['frames_expected']} frames in {stage['drain_s']}s "
                      f"= {stage['frames_per_s']}/s (complete sockets {stage['sockets_complete']}"
                      f"/{stage['subscribers_connected']})")
            elif name == "mesh_fan":
                print(f"soak:   mesh       {stage['connections_ok']} sockets in {stage['connect_s']}s, "
                      f"peers={stage['peers_reported']}, ping p50 {stage['ping_p50_ms']}ms "
                      f"p95 {stage['ping_p95_ms']}ms, errors={stage['ping_errors']}")
            elif name == "rate_limit":
                print(f"soak:   rate cap   {stage['accepted']} accepted / {stage['rejected_429']} rejected "
                      f"of {stage['publishes']} publishes from one agent id "
                      f"(cap {stage['rate_limit_per_minute']}/min)")
        print(f"soak:   rss start={run.get('rss_kb_after_start')}kb "
              f"after_register={run.get('rss_kb_after_register')}kb "
              f"after_mesh={run.get('rss_kb_after_mesh')}kb end={run.get('rss_kb_at_end')}kb "
              f"stopped_clean={run.get('server_stopped_clean')} "
              f"listener_gone={run.get('port_listener_gone')}")
    print("soak: claim strings (docs/capacity-ceiling.md prints these verbatim):")
    for claim_id, claim in sorted(summary.get("claims", {}).items()):
        print(f"soak:   {claim_id}  value={claim['value']}  quote={claim['quote']!r}")
    if summary.get("warnings"):
        for warning in summary["warnings"]:
            print(f"soak: WARNING: {warning}", file=sys.stderr)


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="load-soak.py",
        description="Bounded crier capacity soak (CR-FEAT-033): register / deliver / WS fan-out at 2..1000 agents.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Exit codes: 0 ok, 1 a hard failure (dead server / stage error / survivor / peer-count "
            "mismatch), 2 refused (out-of-cap, bad argument, missing binary, unreadable loadavg "
            "file), 3 the load gate skipped the run, 4 the wall-clock budget expired (truncated), "
            "128+signum interrupted.\nThe load gate is scripts/loadgen.py's: the run is skipped when "
            "the measured 1-minute load average is STRICTLY ABOVE --load-threshold."
        ),
    )
    parser.add_argument("--agents", default=str(DEFAULT_AGENTS),
                        help=f"comma-separated profiles, each 1..{MAX_AGENTS} (default {DEFAULT_AGENTS})")
    parser.add_argument("--messages", type=int, default=DEFAULT_MESSAGES,
                        help=f"inbox deliveries per profile, at least one per agent (default {DEFAULT_MESSAGES}, cap {MAX_MESSAGES})")
    parser.add_argument("--subscribers", default="auto",
                        help=f"relay WebSocket subscribers per profile (default: one per agent, cap {MAX_SUBSCRIBERS})")
    parser.add_argument("--events", type=int, default=DEFAULT_EVENTS,
                        help=f"relay publishes per profile (default {DEFAULT_EVENTS}, cap {MAX_EVENTS})")
    parser.add_argument("--mesh-connections", default="auto",
                        help=f"mesh WebSockets per profile (default: one per agent, cap {MAX_MESH_CONNECTIONS})")
    parser.add_argument("--concurrency", type=int, default=DEFAULT_CONCURRENCY,
                        help=f"HTTP workers (default {DEFAULT_CONCURRENCY}, cap {MAX_CONCURRENCY})")
    parser.add_argument("--budget-seconds", type=float, default=DEFAULT_BUDGET_S,
                        help=f"wall-clock budget for EACH profile (default {DEFAULT_BUDGET_S:g}, cap {MAX_BUDGET_S:g})")
    parser.add_argument("--timeout", type=float, default=DEFAULT_REQUEST_TIMEOUT_S,
                        help=f"per-request timeout (default {DEFAULT_REQUEST_TIMEOUT_S:g}s, cap {MAX_REQUEST_TIMEOUT_S:g}s)")
    parser.add_argument("--load-threshold", type=float, default=DEFAULT_LOAD_THRESHOLD,
                        help=f"skip above this 1-minute loadavg (default {DEFAULT_LOAD_THRESHOLD:g})")
    parser.add_argument("--loadavg-file", default=DEFAULT_LOADAVG_FILE,
                        help="load figure source (override only to pin a synthetic value)")
    parser.add_argument("--binary", default=str(REPO_ROOT / DEFAULT_BINARY), help="crier server binary")
    parser.add_argument("--sequential-probe", action=argparse.BooleanOptionalAction, default=True,
                        help="on the LAST profile, also replay the reviewer's method (one worker, one "
                             "connection) so the published numbers state their own method (default on)")
    parser.add_argument("--claim-prefix", default="COUNT-SOAK",
                        help="prefix for the claim ids this run publishes (default COUNT-SOAK; the "
                             "under-load artifact uses its own so the two cannot collide)")
    parser.add_argument("--output", default=None,
                        help="write the machine-readable summary to this path (the artifact "
                             "docs/claims.yaml anchors the published numbers to)")
    parser.add_argument("--json", action="store_true", help="print the summary as one JSON object on stdout")
    args = parser.parse_args(argv)

    started_wall = time.time()
    summary: dict = {
        "schema": "crier-capacity-soak/1",
        "tool": "scripts/load-soak.py",
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(started_wall)),
        "measured_at_unix": int(started_wall),
        "host": {"cpus": os.cpu_count()},
        "runs": [],
        "claims": {},
        "warnings": [],
    }

    interrupted: int | None = None

    def _on_signal(signum, _frame):
        raise Interrupted(signum)

    previous = {}
    for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        try:
            previous[signum] = signal.signal(signum, _on_signal)
        except Exception:  # noqa: BLE001 - non-main thread
            pass

    workdir = Path(os.environ.get("TMPDIR", "/tmp"))
    exit_code = EXIT_OK
    try:
        profiles = parse_profiles(args.agents)
        _check_cap("--messages", args.messages, MAX_MESSAGES, "per-profile inbox deliveries")
        _check_cap("--events", args.events, MAX_EVENTS, "per-profile relay publishes")
        _check_cap("--concurrency", args.concurrency, MAX_CONCURRENCY,
                   "HTTP workers — the repo's load rule (DF-CRIER-254)")
        if args.concurrency < 1:
            raise loadgen.Refused("--concurrency must be >= 1")
        if args.timeout <= 0 or args.timeout > MAX_REQUEST_TIMEOUT_S:
            raise loadgen.Refused(
                f"--timeout {args.timeout:g} is outside (0, {MAX_REQUEST_TIMEOUT_S:g}] "
                "(a hang must fail a measurement, not extend it)"
            )
        if args.budget_seconds <= 0 or args.budget_seconds > MAX_BUDGET_S:
            raise loadgen.Refused(
                f"--budget-seconds {args.budget_seconds:g} is outside (0, {MAX_BUDGET_S:g}] "
                "(the same hard lifetime cap scripts/loadgen.py enforces; this harness is bounded)"
            )
        if not args.claim_prefix or not all(c.isalnum() or c in "-_" for c in args.claim_prefix):
            raise loadgen.Refused(
                f"--claim-prefix {args.claim_prefix!r} must be a non-empty id-safe string "
                "(letters, digits, '-' or '_')"
            )
        subscribers_spec = None if args.subscribers == "auto" else args.subscribers
        mesh_spec = None if args.mesh_connections == "auto" else args.mesh_connections
        if subscribers_spec is not None:
            subscribers_value = _parse_count("--subscribers", subscribers_spec)
            _check_cap("--subscribers", subscribers_value, MAX_SUBSCRIBERS, "relay WebSocket subscribers")
            subscribers_spec = str(subscribers_value)
        if mesh_spec is not None:
            mesh_value = _parse_count("--mesh-connections", mesh_spec)
            _check_cap("--mesh-connections", mesh_value, MAX_MESH_CONNECTIONS, "mesh WebSockets")
            mesh_spec = str(mesh_value)

        binary = Path(args.binary)
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise loadgen.Refused(
                f"the server binary is missing or not executable: {binary} — run `make build` first "
                "(the soak never builds for you: what it measures must be the artifact you built)"
            )

        # ONE implementation of the gate: loadgen's.  This runs BEFORE any spawn,
        # so a skipped run starts no server at all.
        loadavg = loadgen.read_loadavg(args.loadavg_file)
        if loadavg > args.load_threshold:
            print(
                f"SKIPPED: 1-minute loadavg {loadavg:.2f} is above the threshold "
                f"{args.load_threshold:.2f} — refusing to run a capacity soak on a loaded host. "
                "Raise --load-threshold only when the box can take it (and publish the loadavg "
                "you measured at).",
                flush=True,
            )
            return EXIT_LOAD_SKIP

        summary["host"].update({"loadavg_1m": round(loadavg, 2), "loadavg_threshold": args.load_threshold,
                                "loadavg_source": args.loadavg_file})
        summary["binary"] = {"path": str(binary), "sha256": _file_sha256(binary)}
        summary["config"] = {
            "profiles": profiles,
            "messages": args.messages,
            "subscribers": subscribers_spec or "auto (one per agent)",
            "events": args.events,
            "mesh_connections": mesh_spec or "auto (one per agent)",
            "concurrency": args.concurrency,
            "budget_seconds": args.budget_seconds,
            "request_timeout_s": args.timeout,
            "rate_probe_publishes": RATE_PROBE_PUBLISHES,
            "sequential_probe": args.sequential_probe,
            "claim_prefix": args.claim_prefix,
        }
        try:
            summary["git_head"] = subprocess.run(
                ["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"],
                capture_output=True, text=True, timeout=10, check=False,
            ).stdout.strip()
        except Exception:  # noqa: BLE001 - reported as unknown, never fatal
            summary["git_head"] = ""

        for index, agents in enumerate(profiles):
            subscribers = int(subscribers_spec) if subscribers_spec is not None else min(agents, MAX_SUBSCRIBERS)
            mesh_connections = int(mesh_spec) if mesh_spec is not None else min(agents, MAX_MESH_CONNECTIONS)
            # The budget is per profile, so a long profile list cannot extend the
            # run without bound either.
            profile_deadline = time.monotonic() + args.budget_seconds
            run = run_profile(
                agents=agents,
                messages=args.messages,
                subscribers=subscribers,
                events=args.events,
                mesh_connections=mesh_connections,
                binary=str(binary),
                workdir=workdir,
                timeout=args.timeout,
                concurrency=args.concurrency,
                budget_s=args.budget_seconds,
                profile_deadline=profile_deadline,
                sequential_probe=args.sequential_probe and index == len(profiles) - 1,
            )
            summary["runs"].append(run)
            summary["claims"].update(claims_for(run, args.claim_prefix))
            if run["truncated"]:
                summary["warnings"].append(
                    f"profile {agents}: budget of {args.budget_seconds:g}s expired at stage "
                    f"{run['truncated_at']} — the numbers for this profile are INCOMPLETE"
                )
                exit_code = EXIT_TRUNCATED
            if not run.get("server_stopped_clean", True) or not run.get("port_listener_gone", True):
                summary["warnings"].append(
                    f"profile {agents}: the server did not stop cleanly "
                    f"(stopped_clean={run.get('server_stopped_clean')} "
                    f"listener_gone={run.get('port_listener_gone')})"
                )
                exit_code = EXIT_FAILED
            mesh = run["stages"].get("mesh_fan")
            fan = run["stages"].get("relay_fanout")
            if fan and fan["subscribers_connected"] and not fan["all_sockets_complete"]:
                summary["warnings"].append(
                    f"profile {agents}: relay fan-out was PARTIAL — {fan['sockets_complete']} of "
                    f"{fan['subscribers_connected']} sockets received all {fan['events_published']} events "
                    "(published honestly, not smoothed over)"
                )
    except Interrupted as exc:
        interrupted = exc.signum
        exit_code = 128 + exc.signum
        summary["warnings"].append(f"interrupted by signal {exc.signum}; the server was torn down")
    except loadgen.LoadSkip as exc:
        print(f"SKIPPED: {exc.reason()}", flush=True)
        return EXIT_LOAD_SKIP
    except loadgen.Refused as exc:
        print(f"load-soak: ERROR: {exc}", file=sys.stderr)
        return EXIT_REFUSED
    except StageError as exc:
        summary["warnings"].append(f"hard failure: {exc}")
        print(f"load-soak: ERROR: {exc}", file=sys.stderr)
        exit_code = EXIT_FAILED
    finally:
        for signum, handler in previous.items():
            try:
                signal.signal(signum, handler)
            except Exception:  # noqa: BLE001 - defensive
                pass

    summary["exit_code"] = exit_code
    summary["interrupted"] = interrupted

    if args.output:
        out = Path(args.output)
        out.parent.mkdir(parents=True, exist_ok=True)
        tmp = out.with_suffix(out.suffix + ".tmp")
        tmp.write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        tmp.replace(out)
        print(f"load-soak: wrote {out}", file=sys.stderr)

    if args.json:
        print(json.dumps(summary, sort_keys=True))
    else:
        human_report(summary)
    return exit_code


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with open(path, "rb") as handle:
            for chunk in iter(lambda: handle.read(65536), b""):
                digest.update(chunk)
    except OSError:
        return ""
    return digest.hexdigest()


if __name__ == "__main__":  # pragma: no cover - CLI entry
    sys.exit(main())
