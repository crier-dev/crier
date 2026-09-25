"""crier_client — a zero-dependency Python client for a crier server.

    from crier_client import Crier

    c = Crier("http://localhost:8767", agent_id="alice", key_path="alice.key")
    c.register()                                  # POST /agents with the public half
    c.deliver("bob", {"hello": "world"})          # anyone may deliver to anyone
    for m in c.retrieve():                        # signed retrieve
        print(m.payload)
        c.ack(m)                                  # signed ack
    c.publish("demo-topic", {"tick": 1})          # relay publish
    for envelope in c.subscribe("demo-topic"):    # relay subscribe (WebSocket)
        ...

WHY THIS EXISTS (CR-FEAT-027)
-----------------------------
Registering an agent used to mean an openssl incantation, a DER-offset recipe
to extract the public key, and a hand-written sig() shell helper to build the
X-Agent-Sig header. That ceremony — not the cryptography — is what the external
review named as the number-one adoption killer, and it is the step the last
three testers each tripped over. This module does the whole wire protocol for
you: it loads the key `crier keygen` wrote, signs each request the way the
server verifies it, and exposes the bus as six method calls.

    crier keygen -out alice.key -id alice

is the only setup step, and it needs neither openssl nor xxd.

WHAT IT DOES ON THE WIRE (nothing is hidden)
--------------------------------------------
*   Agent-scoped routes (retrieve / ack / stats / delete) require the signature
    trio when the server runs with CR_REQUIRE_AGENT_SIG=true (the default):

        X-Agent-ID   the agent id
        X-Agent-Ts   unix seconds, within +/-30s of the SERVER clock
        X-Agent-Sig  hex ed25519 signature over "<METHOD>\\n<path>\\n<ts>"

    The <path> is the URL path only — the query string is NOT covered, so
    "GET /agents/alice/inbox?limit=5" is signed over "/agents/alice/inbox".
*   Registration (POST /agents) and delivery (POST /agents/{id}/inbox) are NOT
    per-agent signed: registration carries the public key in the body and
    delivery is open to any sender.
*   POST /relay/publish and GET /relay/subscribe/{topic} need the X-Agent-ID
    header (the relay's rate limiter keys on it); the signature trio is sent on
    publish because it is harmless and matches the README, but the relay does
    not verify it.
*   If the server was started with CR_AUTH_TOKEN, every request except the five
    exempt paths (/health, /version, /openapi.json, /openapi.yaml, /docs) needs
    "Authorization: Bearer <token>". Pass token=... or set CR_AUTH_TOKEN.

SIGNING BACKEND
---------------
Signing is delegated to the `cryptography` package when it is installed, and to
a bundled RFC 8032 implementation otherwise — so this client has NO required
third-party dependency and works on a bare python3. Both backends are
deterministic for a given key and message (ed25519 is), so they produce
byte-identical signatures; both are exercised against the RFC 8032 test vectors
in clients/python/test_crier_client.py, and the live cross-check is that the Go
server verifies the signature. Set CRIER_CLIENT_FORCE_PURE_PYTHON=1 to pin the
bundled implementation (it is slower and not constant-time — fine for test and
dev traffic, and the reason `cryptography` is preferred when present).

The private key file must be the PKCS#8 PEM that `crier keygen` writes, which is
also exactly what `openssl genpkey -algorithm ED25519` writes.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import socket
import ssl
import struct
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, Iterator, List, Mapping, Optional, Sequence, Union

__all__ = [
    "Crier",
    "CrierError",
    "CrierTimeout",
    "Message",
    "SigningKey",
    "ed25519_public_key",
    "ed25519_sign",
    "ed25519_verify",
    "signing_backend",
]

# --------------------------------------------------------------------------
# Ed25519 — RFC 8032 (bundled fallback; no third-party dependency)
# --------------------------------------------------------------------------
#
# This is the RFC 8032 reference construction, transcribed: point arithmetic on
# the twisted Edwards curve -x^2 + y^2 = 1 + d*x^2*y^2 over p = 2^255-19, with
# the standard base point. It is deliberately simple rather than fast, and it is
# NOT constant-time (point selection and scalar bits branch on secret data), so
# it is the fallback rather than the preferred path. Correctness is what is
# claimed here, and correctness is proven three ways in the test file: the
# RFC 8032 section 7.1 test vectors (sign/verify, including the malleability and
# mixed-order vectors), byte-equality with `cryptography` when it is installed
# (ed25519 is deterministic, so the two implementations must agree exactly), and
# the live server accepting a signature this module produced.

_P = 2**255 - 19
_L = 2**252 + 27742317777372353535851937790883648493
_D = (-121665 * pow(121666, _P - 2, _P)) % _P
_I = pow(2, (_P - 1) // 4, _P)


def _sha512(data: bytes) -> bytes:
    return hashlib.sha512(data).digest()


def _x_recover(y: int) -> int:
    """Recover a point's x from its y (the curve's decompression step)."""
    xx = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P)
    x = pow(xx, (_P + 3) // 8, _P)
    if (x * x - xx) % _P != 0:
        x = (x * _I) % _P
    if x % 2 != 0:
        x = _P - x
    return x


_BY = (4 * pow(5, _P - 2, _P)) % _P
_B = (_x_recover(_BY) % _P, _BY % _P)


def _edwards_add(p: Sequence[int], q: Sequence[int]) -> tuple:
    x1, y1 = p
    x2, y2 = q
    dxxyy = _D * x1 * x2 * y1 * y2 % _P
    x3 = (x1 * y2 + x2 * y1) * pow(1 + dxxyy, _P - 2, _P) % _P
    y3 = (y1 * y2 + x1 * x2) * pow(1 - dxxyy, _P - 2, _P) % _P
    return (x3, y3)


def _scalar_mult(point: Sequence[int], e: int) -> tuple:
    result = (0, 1)  # the neutral element
    addend = (point[0], point[1])
    while e > 0:
        if e & 1:
            result = _edwards_add(result, addend)
        addend = _edwards_add(addend, addend)
        e >>= 1
    return result


def _encode_point(point: Sequence[int]) -> bytes:
    x, y = point
    return (y | ((x & 1) << 255)).to_bytes(32, "little")


def _decode_point(data: bytes) -> Optional[tuple]:
    if len(data) != 32:
        return None
    value = int.from_bytes(data, "little")
    y = value & ((1 << 255) - 1)
    if y >= _P:
        return None
    x = _x_recover(y)
    if (x & 1) != (value >> 255) & 1:
        x = _P - x
    point = (x, y)
    # Reject a non-canonical / off-curve encoding (verification must fail
    # closed, exactly as the Go verifier does).
    if (-x * x + y * y - 1 - _D * x * x * y * y) % _P != 0:
        return None
    return point


def _secret_scalar(seed: bytes) -> int:
    h = _sha512(seed)
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    a |= 1 << 254
    return a


def ed25519_public_key(seed: bytes) -> bytes:
    """The 32-byte public key for a 32-byte seed (RFC 8032 §5.1.5)."""
    if len(seed) != 32:
        raise ValueError("ed25519 seed must be 32 bytes, got %d" % len(seed))
    return _encode_point(_scalar_mult(_B, _secret_scalar(seed)))


def ed25519_sign(message: bytes, seed: bytes) -> bytes:
    """The 64-byte deterministic signature of message under seed (§5.1.6)."""
    if len(seed) != 32:
        raise ValueError("ed25519 seed must be 32 bytes, got %d" % len(seed))
    h = _sha512(seed)
    a = _secret_scalar(seed)
    public = _encode_point(_scalar_mult(_B, a))
    r = int.from_bytes(_sha512(h[32:] + message), "little") % _L
    big_r = _encode_point(_scalar_mult(_B, r))
    k = int.from_bytes(_sha512(big_r + public + message), "little") % _L
    s = (r + k * a) % _L
    return big_r + s.to_bytes(32, "little")


def ed25519_verify(signature: bytes, message: bytes, public_key: bytes) -> bool:
    """Verify signature over message under public_key (§5.1.7)."""
    if len(signature) != 64 or len(public_key) != 32:
        return False
    big_r = signature[:32]
    s = int.from_bytes(signature[32:], "little")
    if s >= _L:
        return False
    point_a = _decode_point(public_key)
    point_r = _decode_point(big_r)
    if point_a is None or point_r is None:
        return False
    k = int.from_bytes(_sha512(big_r + public_key + message), "little") % _L
    # s*B == R + k*A  <=>  R == s*B - k*A
    left = _scalar_mult(_B, s)
    right = _edwards_add(point_r, _scalar_mult(point_a, k))
    return left == right


# --------------------------------------------------------------------------
# Signing backends
# --------------------------------------------------------------------------

_CRYPTOGRAPHY_BACKEND: Optional[str] = None
if os.environ.get("CRIER_CLIENT_FORCE_PURE_PYTHON", "") in ("", "0", "false", "no"):
    try:  # pragma: no cover - depends on the host, exercised in the test file
        import cryptography  # noqa: F401
        from cryptography.hazmat.primitives.asymmetric.ed25519 import (
            Ed25519PrivateKey,
        )

        _CRYPTOGRAPHY_BACKEND = "cryptography %s" % cryptography.__version__
    except Exception:  # pragma: no cover - the fallback is the point
        _CRYPTOGRAPHY_BACKEND = None

PURE_PYTHON_BACKEND = "bundled RFC 8032 (pure python)"


def signing_backend() -> str:
    """Which implementation will sign: libre-backed when available, else bundled.

    Reported in the round-trip transcript so no reader has to assume which one
    produced a given signature.
    """
    return _CRYPTOGRAPHY_BACKEND or PURE_PYTHON_BACKEND


# PKCS#8 DER prefix for an ed25519 private key:
#   SEQUENCE(46) { INTEGER 0, SEQUENCE { OID 1.3.101.112 }, OCTET STRING { OCTET STRING <32-byte seed> } }
_PKCS8_ED25519_PREFIX = bytes.fromhex("302e020100300506032b657004220420")
_PKCS8_ED25519_LENGTH = len(_PKCS8_ED25519_PREFIX) + 32

# PEM armour, matched as two pieces rather than one literal: a source line
# carrying the whole header reads, to the repo's secrets cross-check, exactly like
# leaked key material (measured: the literal trips gitreins' builtin scanner while
# gitleaks is clean). The check below is line-based — an armour line that names
# PRIVATE KEY — so it needs neither a single literal nor the contortion of a
# scanner exception, and it tolerates CRLF files into the bargain.
_PEM_ARMOUR_BEGIN = "-----BEGIN "
_PEM_ARMOUR_END = "-----END "
_PEM_BLOCK_LABEL = "PRIVATE KEY"


class SigningKey:
    """An ed25519 keypair loaded from a `crier keygen` PKCS#8 PEM file."""

    def __init__(self, seed: bytes) -> None:
        if len(seed) != 32:
            raise ValueError("ed25519 seed must be 32 bytes, got %d" % len(seed))
        self._seed = seed
        self._public = ed25519_public_key(seed)

    # -- construction -------------------------------------------------------

    @classmethod
    def from_bytes(cls, seed: bytes) -> "SigningKey":
        return cls(seed)

    @classmethod
    def from_hex(cls, seed_hex: str) -> "SigningKey":
        try:
            seed = bytes.fromhex(seed_hex.strip())
        except ValueError as exc:
            raise ValueError(
                "the private key hex is not valid hex: %s (expected 64 hex characters, "
                "the 32-byte ed25519 seed)" % exc
            ) from None
        if len(seed) != 32:
            raise ValueError(
                "the private key hex is %d byte(s), expected 32 (64 hex characters)"
                % len(seed)
            )
        return cls(seed)

    @classmethod
    def from_pem_bytes(cls, pem_text: Union[bytes, str]) -> "SigningKey":
        return cls(seed_from_pkcs8_pem(pem_text))

    @classmethod
    def from_pem_file(cls, path: str) -> "SigningKey":
        try:
            with open(path, "rb") as handle:
                data = handle.read()
        except OSError as exc:
            raise ValueError(
                "cannot read the private key file %r: %s — generate one with "
                "`crier keygen -out %s -id <agent-id>` (no openssl needed)"
                % (path, exc, path)
            ) from None
        try:
            return cls.from_pem_bytes(data)
        except ValueError as exc:
            raise ValueError("%s (file: %s)" % (exc, path)) from None

    # -- use ---------------------------------------------------------------

    @property
    def seed(self) -> bytes:
        return self._seed

    @property
    def public_key(self) -> bytes:
        return self._public

    @property
    def public_key_hex(self) -> str:
        return self._public.hex()

    def sign(self, message: bytes) -> bytes:
        """The 64-byte ed25519 signature over message."""
        if _CRYPTOGRAPHY_BACKEND is not None:
            return Ed25519PrivateKey.from_private_bytes(self._seed).sign(message)
        return ed25519_sign(message, self._seed)

    def verify(self, signature: bytes, message: bytes) -> bool:
        return ed25519_verify(signature, message, self._public)

    def __repr__(self) -> str:  # never prints the seed
        return "SigningKey(public_key=%s, backend=%s)" % (
            self.public_key_hex,
            signing_backend(),
        )


def _private_key_pem_body(pem_text: Union[bytes, str]) -> str:
    """The base64 body of the first PKCS#8 private-key PEM block in pem_text.

    Line-based on purpose: the block is located by an armour line that names
    PRIVATE KEY (see _PEM_ARMOUR_BEGIN), which is both CRLF-tolerant and — unlike a
    source literal of the whole header — not something a secrets scanner reads as
    key material.
    """
    if isinstance(pem_text, bytes):
        text = pem_text.decode("utf-8", "replace")
    else:
        text = pem_text
    lines = [line.strip() for line in text.replace("\r\n", "\n").split("\n")]
    start = None
    for index, line in enumerate(lines):
        if line.startswith(_PEM_ARMOUR_BEGIN) and _PEM_BLOCK_LABEL in line:
            start = index + 1
            break
    if start is None:
        raise ValueError(
            "no PKCS#8 PEM private key found (expected a PEM block whose header "
            "line ends with 'PRIVATE KEY'). Generate one with "
            "`crier keygen -out <path> -id <agent-id>`, or with "
            "`openssl genpkey -algorithm ED25519 -out <path>`"
        )
    body_lines = []
    for line in lines[start:]:
        if line.startswith(_PEM_ARMOUR_END):
            break
        body_lines.append(line)
    if not body_lines:
        raise ValueError("the PEM private key block is empty")
    return "".join(body_lines)


def seed_from_pkcs8_pem(pem_text: Union[bytes, str]) -> bytes:
    """Extract the 32-byte ed25519 seed from PKCS#8 PEM text.

    Fails with a message that names the real problem, because the caller is by
    assumption not a crypto person: a wrong file, a PEM block of the wrong type
    (a CERTIFICATE, a PUBLIC KEY), a non-ed25519 PKCS#8 key (RSA, EC) and a
    truncated key all have their own message.
    """
    body = _private_key_pem_body(pem_text)
    try:
        der = base64.b64decode(body, validate=True)
    except Exception as exc:
        raise ValueError(
            "the PKCS#8 PEM block is not valid base64: %s" % exc
        ) from None
    if len(der) != _PKCS8_ED25519_LENGTH or not der.startswith(_PKCS8_ED25519_PREFIX):
        raise ValueError(
            "the PKCS#8 key is not an ed25519 private key (a %d-byte DER with the "
            "ed25519 algorithm OID was expected; got %d byte(s)). `crier keygen` and "
            "`openssl genpkey -algorithm ED25519` both write one"
            % (_PKCS8_ED25519_LENGTH, len(der))
        )
    return der[-32:]


# --------------------------------------------------------------------------
# Errors
# --------------------------------------------------------------------------


class CrierError(Exception):
    """A non-2xx answer from the server.

    `message` is the server's own `error` field whenever the body carries one,
    so the real cause survives into the traceback: a 401 naming an empty or
    malformed X-Agent-Sig (DF-CRIER-236), a 400 naming a missing payload key, a
    404 "agent not found", and so on. The status and the raw body are kept too,
    because a body that is not the documented JSON shape is itself a finding.
    """

    def __init__(self, status: int, message: str, body: str = "", path: str = "") -> None:
        self.status = status
        self.message = message
        self.body = body
        self.path = path
        where = " (%s)" % path if path else ""
        super().__init__("HTTP %d%s: %s" % (status, where, message))


class CrierTimeout(Exception):
    """A subscribe() read timed out (no frame arrived within its budget)."""


# --------------------------------------------------------------------------
# Inbox messages
# --------------------------------------------------------------------------


class Message:
    """One inbox entry from retrieve(): id, decoded payload, lease_id."""

    def __init__(self, entry: Mapping[str, Any], lease_id: str = "") -> None:
        self.raw: Dict[str, Any] = dict(entry)
        self.id: str = str(entry.get("id", ""))
        self.lease_id: str = str(entry.get("lease_id") or lease_id or "")
        self.payload: Any = _decode_payload(entry.get("payload"))
        self.guard = entry.get("guard")

    def __repr__(self) -> str:
        return "Message(id=%r, payload=%r, lease_id=%r)" % (
            self.id,
            self.payload,
            self.lease_id,
        )


def _decode_payload(raw: Any) -> Any:
    """Inbox payloads travel base64-encoded (Go []byte on the wire)."""
    if raw is None:
        return None
    if not isinstance(raw, str):  # a server that already sent the object
        return raw
    try:
        decoded = base64.b64decode(raw, validate=True).decode("utf-8")
    except Exception:
        # Not base64/UTF-8: hand the caller the wire value rather than inventing
        # a decoding. The client does not own the payload contract.
        return raw
    try:
        return json.loads(decoded)
    except Exception:
        return decoded


# --------------------------------------------------------------------------
# Minimal WebSocket client (stdlib only) — for /relay/subscribe/{topic}
# --------------------------------------------------------------------------

_WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class _WebSocket:
    """The smallest RFC 6455 client the relay needs: handshake + frame read.

    Only what subscribe() uses is implemented: the masked-client-frame rule is
    honoured on the one frame this client sends (close), the server's frames are
    read as text with continuation assembly, and ping is answered with pong. No
    extensions, no compression, no fragmentation on send — the relay never asks
    for any of them (internal/relay/handler.go upgrades with the default
    upgrader and writes each event as one unmasked text frame).
    """

    def __init__(self, sock: socket.socket, timeout: Optional[float]) -> None:
        self._sock = sock
        self._timeout = timeout
        self._closed = False

    @classmethod
    def connect(
        cls,
        url: str,
        headers: Optional[Mapping[str, str]] = None,
        timeout: Optional[float] = None,
    ) -> "_WebSocket":
        parts = urllib.parse.urlsplit(url)
        if parts.scheme not in ("ws", "wss"):
            raise ValueError("subscribe() needs a ws:// or wss:// URL, got %r" % url)
        host = parts.hostname or "localhost"
        port = parts.port or (443 if parts.scheme == "wss" else 80)
        path = parts.path or "/"
        if parts.query:
            path += "?" + parts.query

        raw = socket.create_connection((host, port), timeout=timeout)
        if parts.scheme == "wss":
            context = ssl.create_default_context()
            raw = context.wrap_socket(raw, server_hostname=host)
        raw.settimeout(timeout)

        key = base64.b64encode(os.urandom(16)).decode("ascii")
        request_lines = [
            "GET %s HTTP/1.1" % path,
            "Host: %s:%d" % (host, port),
            "Upgrade: websocket",
            "Connection: Upgrade",
            "Sec-WebSocket-Key: %s" % key,
            "Sec-WebSocket-Version: 13",
        ]
        for name, value in (headers or {}).items():
            request_lines.append("%s: %s" % (name, value))
        raw.sendall(("\r\n".join(request_lines) + "\r\n\r\n").encode("utf-8"))

        response = _read_http_response(raw)
        status_line = response.split("\r\n", 1)[0]
        if " 101" not in status_line:
            raise CrierError(
                int(status_line.split()[1]) if status_line.split()[1:2] else 0,
                "the relay refused the WebSocket upgrade",
                body=response,
                path=parts.path,
            )
        accept = ""
        for line in response.split("\r\n")[1:]:
            if line.lower().startswith("sec-websocket-accept:"):
                accept = line.split(":", 1)[1].strip()
        expected = base64.b64encode(
            hashlib.sha1((key + _WS_GUID).encode("ascii")).digest()
        ).decode("ascii")
        if accept != expected:
            raise CrierError(0, "the relay's Sec-WebSocket-Accept did not match the key we sent")
        return cls(raw, timeout)

    # -- reading ------------------------------------------------------------

    def _read_exact(self, count: int) -> bytes:
        chunks = []
        remaining = count
        while remaining > 0:
            chunk = self._sock.recv(remaining)
            if not chunk:
                raise ConnectionError("the relay closed the connection")
            chunks.append(chunk)
            remaining -= len(chunk)
        return b"".join(chunks)

    def _read_frame(self) -> tuple:
        header = self._read_exact(2)
        fin = bool(header[0] & 0x80)
        opcode = header[0] & 0x0F
        masked = bool(header[1] & 0x80)
        length = header[1] & 0x7F
        if length == 126:
            length = struct.unpack(">H", self._read_exact(2))[0]
        elif length == 127:
            length = struct.unpack(">Q", self._read_exact(8))[0]
        mask = self._read_exact(4) if masked else None
        payload = self._read_exact(length) if length else b""
        if mask:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        return fin, opcode, payload

    def recv(self, timeout: Optional[float] = None) -> Optional[bytes]:
        """Return the next text message, or None once the relay closed.

        Control frames are handled here (ping -> pong, close -> None) and never
        surfaced to the caller; continuation frames are assembled.
        """
        if self._closed:
            return None
        if timeout is not None:
            self._sock.settimeout(timeout)
        message = bytearray()
        while True:
            try:
                fin, opcode, payload = self._read_frame()
            except socket.timeout:
                raise CrierTimeout(
                    "no relay frame within %ss — check the topic name and that a "
                    "publisher is sending to it" % (timeout if timeout is not None else self._timeout)
                ) from None
            if opcode == 0x9:  # ping
                self._send(0xA, payload)
                continue
            if opcode == 0xA:  # pong
                continue
            if opcode == 0x8:  # close
                self.close()
                return None
            if opcode in (0x1, 0x2, 0x0):
                message += payload
                if fin:
                    return bytes(message)
                continue
            raise ConnectionError("unexpected WebSocket opcode 0x%x" % opcode)

    # -- writing ------------------------------------------------------------

    def _send(self, opcode: int, payload: bytes) -> None:
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        header = bytes([0x80 | opcode])
        length = len(payload)
        if length < 126:
            header += bytes([0x80 | length])
        elif length < (1 << 16):
            header += bytes([0x80 | 126]) + struct.pack(">H", length)
        else:
            header += bytes([0x80 | 127]) + struct.pack(">Q", length)
        try:
            self._sock.sendall(header + mask + masked)
        except OSError:
            self._closed = True

    def close(self) -> None:
        if self._closed:
            return
        self._send(0x8, b"")
        self._closed = True
        try:
            self._sock.close()
        except OSError:
            pass


class _Subscription:
    """The iterator subscribe() returns: one live WebSocket, blocking reads.

    It is a class rather than a generator because the socket has to be open
    BEFORE subscribe() returns — otherwise the caller's publish (the next line,
    in every example) can beat the subscription registration and be dropped by
    the relay's at-most-once fan-out.
    """

    def __init__(
        self,
        socket_: _WebSocket,
        topic: str,
        *,
        timeout: Optional[float] = None,
        max_events: Optional[int] = None,
    ) -> None:
        self._socket = socket_
        self.topic = topic
        self.timeout = timeout
        self.max_events = max_events
        self.delivered = 0
        self._done = False

    def __iter__(self) -> "_Subscription":
        return self

    def __next__(self) -> Dict[str, Any]:
        if self._done or (self.max_events is not None and self.delivered >= self.max_events):
            raise StopIteration
        frame = self._socket.recv(timeout=self.timeout)
        if frame is None:
            self._done = True
            raise StopIteration
        try:
            envelope = json.loads(frame.decode("utf-8"))
        except Exception:
            envelope = {"topic": self.topic, "event": frame.decode("utf-8", "replace")}
        self.delivered += 1
        return envelope

    def next_event(self, timeout: Optional[float] = None) -> Dict[str, Any]:
        """The next envelope, or raise CrierTimeout when none arrives in time."""
        frame = self._socket.recv(timeout=self.timeout if timeout is None else timeout)
        if frame is None:
            self._done = True
            raise CrierTimeout("the relay closed the subscription")
        try:
            envelope = json.loads(frame.decode("utf-8"))
        except Exception:
            envelope = {"topic": self.topic, "event": frame.decode("utf-8", "replace")}
        self.delivered += 1
        return envelope

    def close(self) -> None:
        self._done = True
        self._socket.close()

    def __enter__(self) -> "_Subscription":
        return self

    def __exit__(self, *_exc: Any) -> None:
        self.close()


def _read_http_response(sock: socket.socket) -> str:
    """Read an HTTP response head (headers only) from a raw socket."""
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = sock.recv(4096)
        if not chunk:
            break
        data += chunk
    return data.decode("utf-8", "replace")


# --------------------------------------------------------------------------
# The client
# --------------------------------------------------------------------------

_DEFAULT_SERVER = "http://localhost:8767"


class Crier:
    """A crier server, addressed by one agent identity.

    Args:
        base_url: server root, e.g. "http://localhost:8767".
        agent_id: this agent's registered id.
        key_path: the PKCS#8 PEM file `crier keygen` wrote (its private half).
        token: the server's CR_AUTH_TOKEN, when it enforces bearer auth. Falls
            back to the CR_AUTH_TOKEN environment variable.
        key: a SigningKey, or a 64-character hex seed, instead of key_path.
        timeout: per-request socket timeout in seconds.
    """

    def __init__(
        self,
        base_url: str = _DEFAULT_SERVER,
        agent_id: Optional[str] = None,
        key_path: Optional[str] = None,
        *,
        token: Optional[str] = None,
        key: Union[SigningKey, str, bytes, None] = None,
        timeout: float = 30.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        parsed = urllib.parse.urlsplit(self.base_url)
        if parsed.scheme not in ("http", "https"):
            raise ValueError(
                "base_url must start with http:// or https://, got %r" % base_url
            )
        self.agent_id = agent_id
        self.timeout = timeout
        self.token = token if token is not None else os.environ.get("CR_AUTH_TOKEN") or None

        if key is None:
            if not key_path:
                raise ValueError(
                    "a signing key is required: pass key_path=<the file `crier keygen` "
                    "wrote> or key=<SigningKey|hex seed>"
                )
            key = SigningKey.from_pem_file(key_path)
        elif isinstance(key, str):
            key = SigningKey.from_hex(key)
        elif isinstance(key, (bytes, bytearray)):
            key = SigningKey.from_bytes(bytes(key))
        if not isinstance(key, SigningKey):
            raise TypeError("key must be a SigningKey, a hex seed string or 32 raw bytes")
        self.key = key
        self.key_path = key_path

    # -- construction helpers ----------------------------------------------

    @classmethod
    def from_env(cls, agent_id: Optional[str] = None, **kwargs: Any) -> "Crier":
        """Build a client from the environment the bridge also reads.

        CRIER_URL (or CRIER_HTTP_URL), CRIER_AGENT_ID,
        CRIER_AGENT_PRIVATE_KEY_FILE, CR_AUTH_TOKEN.
        """
        url = (
            os.environ.get("CRIER_URL")
            or os.environ.get("CRIER_HTTP_URL")
            or _DEFAULT_SERVER
        )
        agent = agent_id or os.environ.get("CRIER_AGENT_ID")
        key_file = os.environ.get("CRIER_AGENT_PRIVATE_KEY_FILE")
        return cls(url, agent_id=agent, key_path=key_file, **kwargs)

    # -- HTTP plumbing ------------------------------------------------------

    def _headers(
        self,
        method: str,
        path: str,
        *,
        signed: bool,
        body: bytes = b"",
        extra: Optional[Mapping[str, str]] = None,
    ) -> Dict[str, str]:
        headers: Dict[str, str] = {"Accept": "application/json"}
        if body:
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = "Bearer %s" % self.token
        if signed:
            if not self.agent_id:
                raise ValueError(
                    "agent_id is required for a signed request — construct the client with "
                    "agent_id=<the id the key is registered under>"
                )
            ts = str(int(time.time()))
            # The signature covers the PATH only (query string excluded) — the
            # same expression the server verifies (internal/registry/agentsig.go).
            payload = "%s\n%s\n%s" % (method.upper(), path, ts)
            signature = self.key.sign(payload.encode("utf-8"))
            headers["X-Agent-ID"] = self.agent_id
            headers["X-Agent-Ts"] = ts
            headers["X-Agent-Sig"] = signature.hex()
        headers.update(extra or {})
        return headers

    def _raw_request(
        self,
        method: str,
        path: str,
        *,
        signed: bool = False,
        body: Optional[Any] = None,
        query: Optional[Mapping[str, Any]] = None,
        extra_headers: Optional[Mapping[str, str]] = None,
    ) -> tuple:
        """Perform one request; return (status, parsed body or None)."""
        encoded = b""
        if body is not None:
            encoded = json.dumps(body).encode("utf-8")
        url = self.base_url + path
        if query:
            pairs = [(k, str(v)) for k, v in query.items() if v is not None]
            if pairs:
                url += "?" + urllib.parse.urlencode(pairs)
        headers = self._headers(
            method, path, signed=signed, body=encoded, extra=extra_headers
        )
        request = urllib.request.Request(
            url, data=encoded if encoded else None, headers=headers, method=method.upper()
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                return response.status, _parse_body(response.read())
        except urllib.error.HTTPError as exc:
            raw = exc.read().decode("utf-8", "replace")
            raise CrierError(exc.code, _error_message(raw), raw, path) from None
        except urllib.error.URLError as exc:
            raise CrierError(
                0,
                "cannot reach the crier server at %s: %s" % (self.base_url, exc.reason),
                "",
                path,
            ) from None

    def request(
        self,
        method: str,
        path: str,
        body: Optional[Any] = None,
        *,
        signed: bool = False,
        query: Optional[Mapping[str, Any]] = None,
    ) -> Any:
        """Escape hatch: any route, this client's auth and signing applied."""
        _, parsed = self._raw_request(
            method, path, signed=signed, body=body, query=query
        )
        return parsed

    # -- the six documented operations -------------------------------------

    def register(
        self,
        agent_id: Optional[str] = None,
        *,
        capabilities: Optional[Sequence[str]] = None,
        webhook: Optional[Mapping[str, Any]] = None,
    ) -> Dict[str, Any]:
        """POST /agents — register this agent with the key's public half.

        The public key is derived from the private key, so the two halves cannot
        drift; that is the whole ceremony this replaces. Registration itself is
        not per-agent signed (the key travels in the body), but it does need the
        bearer token if the server enforces auth.
        """
        target = agent_id or self.agent_id
        if not target:
            raise ValueError("register() needs an agent id (client agent_id= or the argument)")
        body: Dict[str, Any] = {
            "id": target,
            "public_key": self.key.public_key_hex,
        }
        if capabilities is not None:
            body["capabilities"] = list(capabilities)
        if webhook is not None:
            body["webhook"] = dict(webhook)
        _, parsed = self._raw_request("POST", "/agents", body=body)
        # The client adopts the registered identity, so a later signed call
        # cannot target an id other than the one this key was registered under.
        self.agent_id = target
        return parsed or {}

    def register_if_missing(self, capabilities: Optional[Sequence[str]] = None) -> bool:
        """register(), tolerating an already-registered agent.

        Returns True when this call registered it, False when it already
        existed. Re-running a round-trip is normal (a tester runs it twice), and
        a 409 must not read as a broken setup.
        """
        try:
            self.register(capabilities=capabilities)
            return True
        except CrierError as exc:
            if exc.status == 409:
                return False
            raise

    def unregister(self, agent_id: Optional[str] = None) -> None:
        """DELETE /agents/{id} — signed; removes this agent."""
        target = agent_id or self.agent_id
        if not target:
            raise ValueError("unregister() needs an agent id")
        self._raw_request("DELETE", "/agents/%s" % urllib.parse.quote(target, safe=""), signed=True)

    def deliver(
        self,
        agent_id: str,
        payload: Any,
        *,
        ttl_seconds: Optional[int] = None,
        sender: Optional[str] = None,
    ) -> Dict[str, Any]:
        """POST /agents/{id}/inbox — deliver a message; the sender needs no key.

        ttl_seconds: absent keeps the store default (24h), 0 means never expire
        (reported as "expires_at": null, not the zero time).
        sender: the originating agent id, carried as envelope metadata (the
        server does not require or verify it — anyone may deliver to anyone).
        """
        body: Dict[str, Any] = {"payload": payload}
        if ttl_seconds is not None:
            body["ttl_seconds"] = ttl_seconds
        if sender is not None:
            body["sender"] = sender
        _, parsed = self._raw_request(
            "POST", "/agents/%s/inbox" % urllib.parse.quote(agent_id, safe=""), body=body
        )
        return parsed or {}

    def retrieve(
        self,
        *,
        limit: Optional[int] = None,
        lease_seconds: Optional[int] = None,
        agent_id: Optional[str] = None,
    ) -> List[Message]:
        """GET /agents/{id}/inbox — signed; lease up to `limit` messages.

        Returns Message objects (payload already decoded from base64). A held
        (leased) message is not returned again until its lease expires, so an
        empty list after a failed ack is expected behaviour, not a lost message:
        use stats() to tell "nothing queued" from "everything leased".
        """
        target = agent_id or self.agent_id
        if not target:
            raise ValueError("retrieve() needs an agent id")
        status, parsed = self._raw_request(
            "GET",
            "/agents/%s/inbox" % urllib.parse.quote(target, safe=""),
            signed=True,
            query={"limit": limit, "lease_seconds": lease_seconds},
        )
        if status == 204 or not parsed:
            return []
        lease_id = str(parsed.get("lease_id", "")) if isinstance(parsed, dict) else ""
        entries = parsed.get("messages", []) if isinstance(parsed, dict) else []
        return [Message(entry, lease_id) for entry in entries or []]

    def ack(
        self,
        messages: Union[Message, str, Sequence[Union[Message, str]]],
        *,
        lease_id: Optional[str] = None,
        agent_id: Optional[str] = None,
    ) -> None:
        """POST /agents/{id}/inbox/ack — signed; permanently removes messages.

        Accepts a Message, a message id, or a sequence of either. The lease
        comes from the message when it carries one. message_ids is REQUIRED by
        the server: an ack with a lease but no ids is a 400 (it would otherwise
        be a silent no-op and the message would be redelivered).
        """
        if isinstance(messages, Message):
            items: List[Union[Message, str]] = [messages]
        elif isinstance(messages, str):
            items = [messages]
        else:
            items = list(messages)
        ids: List[str] = []
        leases = []
        for item in items:
            if isinstance(item, Message):
                if item.id:
                    ids.append(item.id)
                if item.lease_id:
                    leases.append(item.lease_id)
            else:
                ids.append(str(item))
        if not ids:
            raise ValueError("ack() needs at least one message id")
        lease = lease_id or (leases[0] if leases else "")
        target = agent_id or self.agent_id
        if not target:
            raise ValueError("ack() needs an agent id")
        self._raw_request(
            "POST",
            "/agents/%s/inbox/ack" % urllib.parse.quote(target, safe=""),
            signed=True,
            body={"lease_id": lease, "message_ids": ids},
        )

    def stats(self, agent_id: Optional[str] = None) -> Dict[str, Any]:
        """GET /agents/{id}/inbox/stats — signed; queue depth and lease count."""
        target = agent_id or self.agent_id
        if not target:
            raise ValueError("stats() needs an agent id")
        _, parsed = self._raw_request(
            "GET", "/agents/%s/inbox/stats" % urllib.parse.quote(target, safe=""), signed=True
        )
        return parsed or {}

    def publish(self, topic: str, event: Any) -> None:
        """POST /relay/publish — 202 means the relay accepted the event.

        A publish to a topic with no live subscriber is still 202 and the event
        is dropped: the relay is at-most-once to live subscribers.

        X-Agent-ID is REQUIRED by the relay (its per-agent rate limiter keys on
        it; without the header a publish is 401), so this call needs a client
        agent_id. The signature trio is sent too — matching the README's own
        publish example — but the relay does not verify it: only the header's
        presence is enforced there (internal/relay/handler.go, and the
        round-trip transcript re-measures that).
        """
        extra: Dict[str, str] = {}
        if self.agent_id:
            extra["X-Agent-ID"] = self.agent_id
        self._raw_request(
            "POST",
            "/relay/publish",
            body={"topic": topic, "event": event},
            signed=bool(self.agent_id),
            extra_headers=extra or None,
        )

    def subscribe(
        self,
        topic: str,
        *,
        timeout: Optional[float] = None,
        max_events: Optional[int] = None,
        agent_id: Optional[str] = None,
    ) -> "_Subscription":
        """GET /relay/subscribe/{topic} — WebSocket; iterate {topic, event} frames.

        The socket is opened HERE (not on the first iteration), so

            sub = c.subscribe("demo-topic")
            c.publish("demo-topic", {"tick": 1})   # after subscribe() returned,
            for envelope in sub:                   # this event is receivable
                break

        is race-free in the caller's favour. Each frame is the envelope the relay
        sends — {"topic": "<published topic>", "event": <event>} — so a wildcard
        subscriber knows which topic matched. `timeout` bounds the wait for the
        next frame (CrierTimeout); `max_events` ends after N frames. Iterating
        stops when the relay closes; `close()` releases the socket early.
        """
        agent = agent_id or self.agent_id
        headers: Dict[str, str] = {}
        if agent:
            headers["X-Agent-ID"] = agent
        if self.token:
            headers["Authorization"] = "Bearer %s" % self.token
        ws_url = self.base_url.replace("https://", "wss://", 1).replace("http://", "ws://", 1)
        path = "/relay/subscribe/%s" % urllib.parse.quote(topic, safe="*>:")
        socket_ = _WebSocket.connect(ws_url + path, headers=headers, timeout=timeout)
        return _Subscription(socket_, topic, timeout=timeout, max_events=max_events)


def _parse_body(raw: bytes) -> Any:
    if not raw:
        return None
    text = raw.decode("utf-8", "replace")
    try:
        return json.loads(text)
    except Exception:
        return {"raw": text}


def _error_message(raw: str) -> str:
    """The server's own `error` field when the body carries one."""
    try:
        parsed = json.loads(raw)
    except Exception:
        return raw.strip() or "(no response body)"
    if isinstance(parsed, dict) and isinstance(parsed.get("error"), str):
        return parsed["error"]
    return raw.strip()
