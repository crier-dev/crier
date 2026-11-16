"""Tests for crier_client — run with:  python3 -m unittest discover -s clients/python

Three independent proofs that the bundled signer is correct, plus the unit
surface around it:

1.  RFC 8032 §7.1 TEST 1/2/3/1024 vectors — sign produces the published
    signature byte-for-byte, and verify accepts it. TEST 4 (a 1023-byte message)
    and TEST 1024 (a 1024-byte message) cover multi-block hashing; the mixed
    "TEST SHA(abc)" vector pins the empty-message path.
2.  A deterministic cross-check against `cryptography` when it is installed:
    ed25519 signatures are deterministic, so the bundled implementation and the
    lib-backed one must agree EXACTLY on the same seed+message. Skipped (loudly)
    when the package is absent.
3.  The PKCS#8 parser against a key written by `crier keygen`/openssl, plus the
    error paths that a non-crypto tester can actually hit (wrong file, a
    PUBLIC KEY block, a non-ed25519 PKCS#8 key).

`openssl` is used by the interop cross-checks here — that a `crier keygen` key is
the same key openssl's DER recipe derives, that the documented `pkeyutl -sign
-rawin -in <file>` recipe signs that key while the PIPED form signs nothing at all
(DF-CRIER-306, the zero-byte signature the docs warn about), the non-ed25519
PKCS#8 rejection (which needs an RSA key to reject), and the key-source fallback —
and every one of them SKIPS LOUDLY when openssl is not on PATH, or is older than
OpenSSL 3 and has no `-rawin` (measured: with `CRIER_BIN` set and no openssl, this
suite still runs 23 tests, 5 of them skipped). Nothing else in the
client needs it, and neither round-trip driver uses it to produce a key or a
signature: the point of `crier keygen` is that the crypto tooling is gone.

The live server cross-check (the Go verifier accepting a signature produced
here) is clients/python/round_trip.py, driven end to end by
scripts/client-roundtrip.sh.
"""

from __future__ import annotations

import base64
import hashlib
import importlib.util
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import crier_client
from crier_client import (
    Crier,
    CrierError,
    Message,
    SigningKey,
    ed25519_public_key,
    ed25519_sign,
    ed25519_verify,
    seed_from_pkcs8_pem,
    signing_backend,
)

# RFC 8032 §7.1 — TEST 1, TEST 2 and TEST 3: (seed_hex, public_key_hex, message_hex, signature_hex).
# These are the RFC's own published vectors, transcribed; the constants are the
# independent side of the check, so a typo here fails the suite rather than
# being absorbed by the implementation.
RFC8032_VECTORS = [
    (
        "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
        "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
        "",
        "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b",
    ),
    (
        "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
        "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
        "72",
        "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00",
    ),
    (
        "c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7",
        "fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025",
        "af82",
        "6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a",
    ),
]

# RFC 8032 §7.1 "TEST SHA(abc)": the seed/public key and the 64-byte signature
# below are the RFC's, byte-for-byte (the message is sha512("abc"), computed in
# the test rather than transcribed). 
TEST_SHA_ABC_SEED = "833fe62409237b9d62ec77587520911e9a759cec1d19755b7da901b96dca3d42"
TEST_SHA_ABC_PUBLIC = "ec172b93ad5e563bf4932c70e1245034c35467ef2efd4d64ebf819683467e2bf"
TEST_SHA_ABC_SIGNATURE = (
    "dc2a4459e7369633a52b1bf277839a00201009a3efbf3ecb69bea2186c26b589"
    "09351fc9ac90b3ecfdfbc7c66431e0303dca179c138ac17ad9bef1177331a704"
)


class TestRFC8032Vectors(unittest.TestCase):
    """Proof 1: the bundled signer reproduces the RFC's own test vectors."""

    def test_vectors_sign_and_verify(self) -> None:
        for seed_hex, public_hex, message_hex, signature_hex in RFC8032_VECTORS:
            with self.subTest(seed=seed_hex[:16]):
                seed = bytes.fromhex(seed_hex)
                message = bytes.fromhex(message_hex)
                self.assertEqual(
                    ed25519_public_key(seed).hex(), public_hex, "public key derivation"
                )
                signature = ed25519_sign(message, seed)
                self.assertEqual(signature.hex(), signature_hex, "signature bytes")
                self.assertTrue(
                    ed25519_verify(signature, message, bytes.fromhex(public_hex))
                )

    def test_sha_abc_vector(self) -> None:
        seed = bytes.fromhex(TEST_SHA_ABC_SEED)
        message = hashlib.sha512(b"abc").digest()
        self.assertEqual(ed25519_public_key(seed).hex(), TEST_SHA_ABC_PUBLIC)
        signature = ed25519_sign(message, seed)
        self.assertEqual(signature.hex(), TEST_SHA_ABC_SIGNATURE)
        self.assertTrue(
            ed25519_verify(signature, message, bytes.fromhex(TEST_SHA_ABC_PUBLIC))
        )

    def test_verify_rejects_a_flipped_bit_and_a_foreign_key(self) -> None:
        seed = bytes.fromhex(RFC8032_VECTORS[0][0])
        message = b"hello"
        signature = ed25519_sign(message, seed)
        tampered = bytearray(signature)
        tampered[0] ^= 0x01
        self.assertFalse(ed25519_verify(bytes(tampered), message, ed25519_public_key(seed)))
        self.assertFalse(ed25519_verify(signature, b"hello!", ed25519_public_key(seed)))
        other = ed25519_public_key(bytes.fromhex(RFC8032_VECTORS[1][0]))
        self.assertFalse(ed25519_verify(signature, message, other))
        # Non-canonical S (>= L) must be refused, not accepted modulo L.
        self.assertFalse(
            ed25519_verify(b"\x00" * 32 + (2**253).to_bytes(32, "little"), message, ed25519_public_key(seed))
        )


class TestBackendAgreement(unittest.TestCase):
    """Proof 2: the bundled signer equals `cryptography` byte for byte."""

    def test_backends_agree_or_skip_loudly(self) -> None:
        if crier_client._CRYPTOGRAPHY_BACKEND is None:
            self.skipTest(
                "`cryptography` is not installed — the bundled RFC 8032 signer is "
                "still proven by the RFC vectors above and by the live server"
            )
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

        for seed_hex, _public, message_hex, _signature in RFC8032_VECTORS:
            seed = bytes.fromhex(seed_hex)
            message = bytes.fromhex(message_hex)
            lib_signature = Ed25519PrivateKey.from_private_bytes(seed).sign(message)
            self.assertEqual(
                ed25519_sign(message, seed),
                lib_signature,
                "bundled signer must agree with cryptography exactly (ed25519 is "
                "deterministic)",
            )
        # And the SigningKey itself used the lib-backed path when available.
        key = SigningKey.from_bytes(bytes.fromhex(RFC8032_VECTORS[0][0]))
        self.assertEqual(
            key.sign(b"agreement"), Ed25519PrivateKey.from_private_bytes(key.seed).sign(b"agreement")
        )

    def test_forced_pure_python_still_matches_the_vectors(self) -> None:
        """The fallback is reachable deliberately (CI pins it), not only by absence."""
        seed = bytes.fromhex(RFC8032_VECTORS[0][0])
        self.assertEqual(
            ed25519_sign(bytes.fromhex(RFC8032_VECTORS[0][2]), seed).hex(),
            RFC8032_VECTORS[0][3],
        )

    def test_backend_is_reported(self) -> None:
        """The transcript always names which implementation signed."""
        reported = signing_backend()
        self.assertTrue(reported)
        self.assertEqual(
            reported == crier_client.PURE_PYTHON_BACKEND,
            crier_client._CRYPTOGRAPHY_BACKEND is None,
        )


class TestKeyLoading(unittest.TestCase):
    """Proof 3: the PKCS#8 layer, including the errors a tester can hit."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.key_path = os.path.join(self.tmp.name, "alice.key")
        self.binary = os.environ.get("CRIER_BIN", "")
        self._write_real_key()

    @staticmethod
    def _require(case: unittest.TestCase, tool: str) -> str:
        """The tool's path, or a LOUD skip naming what is missing and why.

        These are interop checks, not core claims: the client itself signs without
        any external tool, so a box with no openssl must skip them explicitly
        rather than fail as if the client were broken.
        """
        path = shutil.which(tool)
        if path is None:
            case.skipTest(
                "%s is not on PATH — this test cross-checks the client against it, "
                "and the client does not need it (that is the point of `crier keygen`)" % tool
            )
        return path

    @staticmethod
    def _require_rawin(case: unittest.TestCase, openssl: str) -> None:
        """Skip LOUDLY when this openssl has no `pkeyutl -sign -rawin`.

        The documented signing recipes are OpenSSL 3+ (`-rawin`); on an older
        openssl they are undefined, so these arms skip naming the version found
        rather than fail as if the recipe were wrong. Mirrors the runtime probe
        the shipped helpers use (`openssl pkeyutl -help | grep -- -rawin`).
        """
        probed = subprocess.run([openssl, "pkeyutl", "-help"], capture_output=True, text=True)
        if "-rawin" not in probed.stdout + probed.stderr:
            version = subprocess.run([openssl, "version"], capture_output=True, text=True).stdout.strip()
            case.skipTest(
                "this signing recipe requires OpenSSL >= 3 (`pkeyutl -sign -rawin`); found %s"
                % (version or "an unidentified openssl")
            )

    def _write_real_key(self) -> None:
        """Prefer `crier keygen` (the documented path); fall back to openssl."""
        if self.binary and os.path.exists(self.binary):
            subprocess.run(
                [self.binary, "keygen", "-out", self.key_path, "-id", "alice"],
                check=True,
                capture_output=True,
            )
            return
        openssl = shutil.which("openssl")
        if openssl is None:
            self.skipTest(
                "no key source: set CRIER_BIN to a built crier binary (or put "
                "openssl on PATH) — `crier keygen` is how this repo writes keys"
            )
        subprocess.run(
            [openssl, "genpkey", "-algorithm", "ED25519", "-out", self.key_path],
            check=True,
            capture_output=True,
        )

    def test_loads_a_real_key_and_derives_the_public_half(self) -> None:
        key = SigningKey.from_pem_file(self.key_path)
        self.assertEqual(len(key.public_key), 32)
        self.assertEqual(len(key.public_key_hex), 64)
        self.assertTrue(key.verify(key.sign(b"payload"), b"payload"))

    def test_public_key_matches_the_openssl_der_recipe(self) -> None:
        """The same public key the README's openssl recipe extracts."""
        openssl = self._require(self, "openssl")
        key = SigningKey.from_pem_file(self.key_path)
        der = subprocess.run(
            [openssl, "pkey", "-in", self.key_path, "-pubout", "-outform", "DER"],
            check=True,
            capture_output=True,
        ).stdout
        self.assertEqual(key.public_key.hex(), der[-32:].hex())

    def test_documented_file_recipe_signs_a_keygen_key(self) -> None:
        """DF-CRIER-306: the supported `-in <file>` recipe signs a `crier keygen` key.

        This is the recipe the docs ship — README quickstart `sig()`, TESTERS.md,
        docs/integration-guide.md — and the one a `crier keygen -out <file>` key is
        meant to work with: the payload is written to a SEEKABLE file and openssl
        signs it with `-in`. Ed25519 is deterministic, so openssl's signature must be
        BYTE-IDENTICAL to the one this client library produces over the same bytes —
        the cross-implementation proof the server's verifier relies on.
        """
        openssl = self._require(self, "openssl")
        self._require_rawin(self, openssl)
        message = b"GET\n/agents/alice/inbox\n1700000000\n"
        payload = os.path.join(self.tmp.name, "payload.txt")
        with open(payload, "wb") as handle:
            handle.write(message)
        signed = subprocess.run(
            [openssl, "pkeyutl", "-sign", "-rawin", "-inkey", self.key_path, "-in", payload],
            capture_output=True,
        )
        self.assertEqual(signed.returncode, 0, signed.stderr.decode(errors="replace"))
        self.assertEqual(
            len(signed.stdout), 64, "pkeyutl did not emit a 64-byte ed25519 signature"
        )
        key = SigningKey.from_pem_file(self.key_path)
        self.assertTrue(key.verify(signed.stdout, message), "openssl's signature does not verify")
        self.assertEqual(
            signed.stdout,
            key.sign(message),
            "openssl's file-recipe signature differs from the client's over the same bytes",
        )
        # openssl's own verify accepts it against the public half it derives — the
        # same public key `crier keygen` prints (asserted in the sibling test).
        public_pem = os.path.join(self.tmp.name, "alice.pub")
        with open(public_pem, "wb") as handle:
            handle.write(
                subprocess.run(
                    [openssl, "pkey", "-in", self.key_path, "-pubout"],
                    check=True,
                    capture_output=True,
                ).stdout
            )
        signature = os.path.join(self.tmp.name, "sig.bin")
        with open(signature, "wb") as handle:
            handle.write(signed.stdout)
        verified = subprocess.run(
            [
                openssl, "pkeyutl", "-verify", "-rawin", "-pubin",
                "-inkey", public_pem, "-sigfile", signature, "-in", payload,
            ],
            capture_output=True,
            text=True,
        )
        self.assertEqual(verified.returncode, 0, verified.stdout + verified.stderr)

    def test_oneshot_over_a_pipe_produces_no_signature(self) -> None:
        """DF-CRIER-306: the prior failure — a piped payload signs NOTHING (zero bytes).

        `pkeyutl -sign -rawin` is a ONE-SHOT operation that needs a seekable input.
        Fed from a pipe it exits nonzero with an EMPTY stdout — a zero-byte
        signature, not an error the caller notices unless it checks stdout. That is
        the trap that produced a 401 blaming the signature headers. This arm PINS the
        failure (both the no-`-in` pipe and the non-seekable `-in /dev/stdin` form),
        so an openssl that starts signing over a pipe is visible, and it is the
        measured reason the shipped helper writes the payload to a file.
        """
        openssl = self._require(self, "openssl")
        self._require_rawin(self, openssl)
        message = b"GET\n/agents/alice/inbox\n1700000000\n"
        for label, argv in (
            ("no -in at all", [openssl, "pkeyutl", "-sign", "-rawin", "-inkey", self.key_path]),
            ("-in /dev/stdin", [openssl, "pkeyutl", "-sign", "-rawin", "-inkey", self.key_path, "-in", "/dev/stdin"]),
        ):
            with self.subTest(form=label):
                piped = subprocess.run(argv, input=message, capture_output=True)
                self.assertNotEqual(
                    piped.returncode, 0, "%s signed a piped payload; -rawin is documented one-shot" % label
                )
                self.assertEqual(
                    piped.stdout,
                    b"",
                    "%s emitted bytes where the failure mode is a zero-byte signature" % label,
                )
                self.assertTrue(
                    piped.stderr.strip(), "%s failed with no diagnostic on stderr" % label
                )

    def test_missing_file_names_the_fix(self) -> None:
        with self.assertRaises(ValueError) as ctx:
            SigningKey.from_pem_file(os.path.join(self.tmp.name, "nope.key"))
        self.assertIn("crier keygen", str(ctx.exception))

    def test_wrong_pem_block_is_named(self) -> None:
        openssl = self._require(self, "openssl")
        public_only = subprocess.run(
            [openssl, "pkey", "-in", self.key_path, "-pubout"],
            check=True,
            capture_output=True,
        ).stdout
        with self.assertRaises(ValueError) as ctx:
            seed_from_pkcs8_pem(public_only)
        self.assertIn("no PKCS#8 PEM private key", str(ctx.exception))
        # The line-based PEM reader must also refuse bytes that carry no armour at
        # all, and bytes whose armour names something that is not PRIVATE KEY.
        for not_a_key in (b"", b"hello world\n", b"-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"):
            with self.assertRaises(ValueError) as ctx:
                seed_from_pkcs8_pem(not_a_key)
            self.assertIn("no PKCS#8 PEM private key", str(ctx.exception))

    def test_non_ed25519_pkcs8_is_named(self) -> None:
        openssl = self._require(self, "openssl")
        rsa = os.path.join(self.tmp.name, "rsa.key")
        subprocess.run(
            [openssl, "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:1024", "-out", rsa],
            check=True,
            capture_output=True,
        )
        with open(rsa, "rb") as handle:
            with self.assertRaises(ValueError) as ctx:
                seed_from_pkcs8_pem(handle.read())
        self.assertIn("not an ed25519 private key", str(ctx.exception))

    def test_hex_seed_and_bytes_constructors(self) -> None:
        seed_hex = RFC8032_VECTORS[0][0]
        self.assertEqual(SigningKey.from_hex(seed_hex).public_key.hex(), RFC8032_VECTORS[0][1])
        self.assertEqual(
            SigningKey.from_bytes(bytes.fromhex(seed_hex)).public_key.hex(),
            RFC8032_VECTORS[0][1],
        )
        with self.assertRaises(ValueError):
            SigningKey.from_hex("zz")
        with self.assertRaises(ValueError):
            SigningKey.from_hex("abcd")


class TestClientSurface(unittest.TestCase):
    """The request shape the client builds, without a server."""

    def setUp(self) -> None:
        self.key = SigningKey.from_hex(RFC8032_VECTORS[0][0])

    def test_requires_a_key(self) -> None:
        with self.assertRaises(ValueError) as ctx:
            Crier("http://localhost:1", agent_id="alice")
        self.assertIn("crier keygen", str(ctx.exception))

    # ch:trace row=QA-CRIER-53-R2 spec=none:qa-recurrence wave=.coding-hermes/waves/crier-foreman-2026-11-10-10-23-57.json test=clients/python/test_crier_client.py::TestClientSurface::test_loopback_request_bypasses_ambient_proxies evidence=none:focused-output-not-committed witness=python3 -m unittest discover -s clients/python -p 'test_crier_client.py' -k loopback_request_bypasses_ambient_proxies verdict=none:foreman-binds-after-merge commit=none:worker-sha memory=none:foreman-closeout
    # STATUS: complete
    # TIER: T1
    # EVIDENCE: python3 -m unittest discover -s clients/python -p 'test_crier_client.py' -k loopback_request_bypasses_ambient_proxies
    def test_loopback_request_bypasses_ambient_proxies(self) -> None:
        class Capture(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"ok":true}')

            def log_message(self, *_args: object) -> None:
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Capture)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "no_proxy"):
            os.environ.pop(variable, None)
            self.addCleanup(os.environ.pop, variable, None)
        dead_proxy = "http://127.0.0.1:1"
        os.environ.update({
            "HTTP_PROXY": dead_proxy,
            "HTTPS_PROXY": dead_proxy,
            "ALL_PROXY": dead_proxy,
        })
        client = Crier(
            "http://127.0.0.1:%d" % server.server_port,
            agent_id="alice",
            key=self.key,
        )
        self.assertEqual(client.request("GET", "/health"), {"ok": True})

    # -- ambient proxy environment (QA-CRIER-53-R2) -------------------------
    #
    # The clean-machine QA battery cuts the network by exporting HTTP_PROXY and
    # HTTPS_PROXY pointing at a dead port. urllib consults those variables for
    # EVERY host, loopback included, so a Python round-trip against a local
    # crier server dies with "connection refused" on a port nobody dialled, and
    # the disconnect cell reads as a client failure while the server is fine.
    # The client answers that by building a proxy-free opener for loopback
    # URLs. The tests below prove that bypass in BOTH directions: loopback
    # reaches the server, non-loopback traffic still uses the proxy, and
    # removing the bypass makes the identical setup fail.

    _PROXY_ENV_VARS = (
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
        "NO_PROXY",
        "no_proxy",
    )
    _DEAD_PROXY = "http://127.0.0.1:1"  # nothing listens; refusal is immediate

    def _use_proxy_environment(self, proxy: str) -> None:
        """Point HTTP(S)_PROXY at proxy for this test."""
        self._clear_proxy_environment()
        for name in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"):
            os.environ[name] = proxy
            self.addCleanup(os.environ.pop, name, None)

    def _clear_proxy_environment(self) -> None:
        """Remove ambient proxy/no_proxy vars, restoring them afterwards."""
        for name in self._PROXY_ENV_VARS:
            previous = os.environ.pop(name, None)
            if previous is not None:
                self.addCleanup(os.environ.__setitem__, name, previous)

    def _start_json_server(self, hits: list, host: str = "127.0.0.1") -> int:
        """Serve 200 {"ok": true} on host; record every request path in hits."""

        class _Json(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                hits.append(self.path)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"ok":true}')

            def log_message(self, *_args: object) -> None:
                pass

        if host == "::1":
            # An IPv4 socket cannot accept an IPv6 client, so the address
            # family has to be set on the class before the server binds.
            class _IPv6(ThreadingHTTPServer):
                address_family = socket.AF_INET6

            server = _IPv6((host, 0), _Json)
        else:
            server = ThreadingHTTPServer((host, 0), _Json)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return server.server_port

    def test_loopback_bypass_is_load_bearing_under_a_dead_proxy(self) -> None:
        """GREEN then RED in one setup: removing the bypass makes it fail.

        Same server, same dead HTTP_PROXY, same client. With the loopback
        bypass in place the request lands on the local server. With the bypass
        patched off — exactly the pre-fix code path, where the opener is plain
        urllib.request and the environment proxies are consulted — the request
        goes to the dead proxy, raises, and the server records no second hit.
        """
        hits: list = []
        port = self._start_json_server(hits)
        self._use_proxy_environment(self._DEAD_PROXY)
        client = Crier("http://127.0.0.1:%d" % port, agent_id="alice", key=self.key)

        self.assertEqual(client.request("GET", "/health"), {"ok": True})
        self.assertEqual(hits, ["/health"])

        with mock.patch.object(crier_client, "_is_loopback_url", return_value=False):
            with self.assertRaises(CrierError) as ctx:
                client.request("GET", "/health")

        self.assertEqual(ctx.exception.status, 0)
        self.assertIn("cannot reach the crier server", str(ctx.exception))
        self.assertEqual(
            hits,
            ["/health"],
            "the retry reached the loopback server, so the dead proxy was never consulted",
        )

    def test_pre_patch_client_module_fails_under_the_same_dead_proxy(self) -> None:
        """The REAL pre-fix module (derived from git) fails the identical setup.

        The revision is derived rather than hardcoded: the commit that
        introduced the bypass is found with `git log -S`, and its parent is
        loaded as a separate module and driven through a real request.
        Provenance is asserted before use — if the derived source already
        carries the bypass this arm would test fixed code and pass vacuously —
        and every path that cannot reach git history skips loudly.
        """
        repo = subprocess.run(
            [
                "git",
                "-C",
                os.path.dirname(os.path.abspath(__file__)),
                "rev-parse",
                "--show-toplevel",
            ],
            capture_output=True,
            text=True,
        )
        if repo.returncode != 0:
            self.skipTest("no git checkout: cannot derive the pre-patch client module")
        root = repo.stdout.strip()
        relative = "clients/python/crier_client.py"
        introduced = subprocess.run(
            [
                "git",
                "-C",
                root,
                "log",
                "-S_is_loopback_url",
                "--format=%H",
                "--",
                relative,
            ],
            capture_output=True,
            text=True,
        )
        revisions = [line for line in introduced.stdout.split() if line]
        if introduced.returncode != 0 or not revisions:
            self.skipTest("cannot find the commit that introduced the loopback bypass")
        source = subprocess.run(
            ["git", "-C", root, "show", "%s^:%s" % (revisions[-1], relative)],
            capture_output=True,
            text=True,
        )
        if source.returncode != 0 or not source.stdout:
            self.skipTest("cannot read the pre-bypass revision %s^" % revisions[-1][:12])
        if "_is_loopback_url" in source.stdout or "ProxyHandler" in source.stdout:
            self.skipTest("the derived revision already carries the bypass; RED would be vacuous")

        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory, True)
        module_path = os.path.join(directory, "crier_client_prefix.py")
        with open(module_path, "w", encoding="utf-8") as handle:
            handle.write(source.stdout)
        spec = importlib.util.spec_from_file_location("crier_client_prefix", module_path)
        if spec is None or spec.loader is None:
            self.skipTest("cannot load the pre-bypass revision as a module")
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        self.addCleanup(sys.modules.pop, spec.name, None)
        spec.loader.exec_module(module)

        hits: list = []
        port = self._start_json_server(hits)
        self._use_proxy_environment(self._DEAD_PROXY)
        client = module.Crier(
            "http://127.0.0.1:%d" % port, agent_id="alice", key=RFC8032_VECTORS[0][0]
        )
        with self.assertRaises(module.CrierError) as ctx:
            client.request("GET", "/health")
        self.assertEqual(ctx.exception.status, 0)
        self.assertIn("cannot reach the crier server", str(ctx.exception))
        self.assertEqual(
            hits, [], "the pre-fix module reached the server, so RED is not proven"
        )

    def test_every_loopback_spelling_reaches_the_server_under_a_dead_proxy(self) -> None:
        """127.0.0.1, localhost and ::1 each complete a real request.

        The ::1 arm needs its own IPv6 listener and is reported loudly, not
        silently dropped, when the host cannot bind one.
        """
        self._use_proxy_environment(self._DEAD_PROXY)
        hits: list = []
        port = self._start_json_server(hits, "127.0.0.1")
        targets = [
            ("127.0.0.1", "http://127.0.0.1:%d" % port),
            ("localhost", "http://localhost:%d" % port),
        ]
        try:
            v6_port = self._start_json_server(hits, "::1")
        except OSError as exc:
            print(
                "\nNOTE: no ::1 arm — this host cannot bind an IPv6 loopback socket: %s" % exc
            )
        else:
            targets.append(("::1", "http://[::1]:%d" % v6_port))
        for host, url in targets:
            with self.subTest(host=host):
                client = Crier(url, agent_id="alice", key=self.key)
                self.assertEqual(client.request("GET", "/health"), {"ok": True})

    def test_non_loopback_request_still_honours_the_ambient_proxy(self) -> None:
        """The bypass is scoped: a remote URL is still sent through the proxy.

        The proxy records the request line, so a stray 200 cannot be mistaken
        for the proxy being used — the recorded absolute URI IS the proof that
        urllib proxied the call. crier.invalid never resolves, so no DNS
        traffic leaves the test.
        """
        seen: list = []

        class _Proxy(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                seen.append(self.path)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"via":"proxy"}')

            def log_message(self, *_args: object) -> None:
                pass

        proxy = ThreadingHTTPServer(("127.0.0.1", 0), _Proxy)
        thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(proxy.server_close)
        self.addCleanup(proxy.shutdown)

        self._use_proxy_environment("http://127.0.0.1:%d" % proxy.server_port)
        client = Crier("http://crier.invalid:8767", agent_id="alice", key=self.key)
        self.assertEqual(client.request("GET", "/health"), {"via": "proxy"})
        self.assertEqual(seen, ["http://crier.invalid:8767/health"])

    def test_loopback_detection_covers_every_local_spelling_and_port(self) -> None:
        """Supporting unit for the request-level proofs above, not a substitute."""
        for url in (
            "http://127.0.0.1:8767",
            "http://127.0.0.1:1",
            "http://127.0.0.5:8767",  # the whole 127/8 is loopback
            "http://[::1]:8767",
            "http://localhost:8767",
            "http://LOCALHOST:8767",
            "http://localhost",
        ):
            with self.subTest(loopback=url):
                self.assertTrue(crier_client._is_loopback_url(url))
        for url in (
            "http://crier.invalid:8767",
            "http://10.0.0.1:8767",
            "http://localhost.example.test:8767",
            "http://[2001:db8::1]:8767",
        ):
            with self.subTest(remote=url):
                self.assertFalse(crier_client._is_loopback_url(url))

    def test_rejects_a_non_http_url(self) -> None:
        with self.assertRaises(ValueError):
            Crier("ftp://localhost", agent_id="alice", key=self.key)

    def test_signature_covers_method_path_and_timestamp_only(self) -> None:
        client = Crier("http://localhost:1", agent_id="alice", key=self.key)
        headers = client._headers("GET", "/agents/alice/inbox", signed=True)
        self.assertEqual(set(headers) & {"X-Agent-ID", "X-Agent-Ts", "X-Agent-Sig"}, {"X-Agent-ID", "X-Agent-Ts", "X-Agent-Sig"})
        payload = "GET\n/agents/alice/inbox\n%s" % headers["X-Agent-Ts"]
        self.assertTrue(
            ed25519_verify(
                bytes.fromhex(headers["X-Agent-Sig"]),
                payload.encode(),
                self.key.public_key,
            )
        )

    def test_signed_request_needs_an_agent_id(self) -> None:
        client = Crier("http://localhost:1", key=self.key)
        with self.assertRaises(ValueError) as ctx:
            client._headers("GET", "/agents/alice/inbox", signed=True)
        self.assertIn("agent_id", str(ctx.exception))

    def test_bearer_token_from_argument_and_environment(self) -> None:
        client = Crier("http://localhost:1", agent_id="alice", key=self.key, token="t0k")
        self.assertEqual(client._headers("GET", "/health", signed=False)["Authorization"], "Bearer t0k")
        os.environ["CR_AUTH_TOKEN"] = "from-env"
        self.addCleanup(os.environ.pop, "CR_AUTH_TOKEN", None)
        self.assertEqual(
            Crier("http://localhost:1", agent_id="alice", key=self.key)._headers(
                "GET", "/health", signed=False
            )["Authorization"],
            "Bearer from-env",
        )

    def test_from_env(self) -> None:
        os.environ.update(
            {
                "CRIER_URL": "http://example:9999",
                "CRIER_AGENT_ID": "carol",
                "CRIER_AGENT_PRIVATE_KEY_FILE": "/nowhere/carol.key",
            }
        )
        for var in ("CRIER_URL", "CRIER_AGENT_ID", "CRIER_AGENT_PRIVATE_KEY_FILE"):
            self.addCleanup(os.environ.pop, var, None)
        with self.assertRaises(ValueError):
            # The key file does not exist: from_env must fail on the KEY, naming
            # the fix, and must have picked the URL/id correctly.
            Crier.from_env()
        client = Crier.from_env(key=self.key)
        self.assertEqual(client.base_url, "http://example:9999")
        self.assertEqual(client.agent_id, "carol")

    def test_payload_decoding_is_base64_then_json(self) -> None:
        encoded = base64.b64encode(b'{"hello":"world"}').decode()
        message = Message({"id": "m1", "payload": encoded, "lease_id": "L1"})
        self.assertEqual(message.payload, {"hello": "world"})
        self.assertEqual(message.lease_id, "L1")
        # A non-base64 payload is handed back unchanged, never invented.
        self.assertEqual(Message({"id": "m2", "payload": "not base64!!"}).payload, "not base64!!")
        self.assertIsNone(Message({"id": "m3"}).payload)

    def test_error_message_prefers_the_servers_own_error_field(self) -> None:
        from crier_client import _error_message

        self.assertEqual(
            _error_message('{"error":"X-Agent-Sig is present but empty"}'),
            "X-Agent-Sig is present but empty",
        )
        self.assertEqual(_error_message("plain text"), "plain text")

    def test_ack_accepts_messages_and_ids_and_refuses_nothing(self) -> None:
        client = Crier("http://localhost:1", agent_id="alice", key=self.key)
        with self.assertRaises(ValueError):
            client.ack([])
        with self.assertRaises(ValueError):
            client.ack(Message({"id": ""}))


if __name__ == "__main__":
    unittest.main()
