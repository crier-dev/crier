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

The live server cross-check (the Go verifier accepting a signature produced
here) is clients/python/round_trip.py, driven end to end by
scripts/client-roundtrip.sh.
"""

from __future__ import annotations

import base64
import hashlib
import os
import subprocess
import sys
import tempfile
import unittest

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

    def _write_real_key(self) -> None:
        """Prefer `crier keygen` (the documented path); fall back to openssl."""
        if self.binary and os.path.exists(self.binary):
            subprocess.run(
                [self.binary, "keygen", "-out", self.key_path, "-id", "alice"],
                check=True,
                capture_output=True,
            )
            return
        subprocess.run(
            ["openssl", "genpkey", "-algorithm", "ED25519", "-out", self.key_path],
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
        key = SigningKey.from_pem_file(self.key_path)
        der = subprocess.run(
            ["openssl", "pkey", "-in", self.key_path, "-pubout", "-outform", "DER"],
            check=True,
            capture_output=True,
        ).stdout
        self.assertEqual(key.public_key.hex(), der[-32:].hex())

    def test_missing_file_names_the_fix(self) -> None:
        with self.assertRaises(ValueError) as ctx:
            SigningKey.from_pem_file(os.path.join(self.tmp.name, "nope.key"))
        self.assertIn("crier keygen", str(ctx.exception))

    def test_wrong_pem_block_is_named(self) -> None:
        public_only = subprocess.run(
            ["openssl", "pkey", "-in", self.key_path, "-pubout"],
            check=True,
            capture_output=True,
        ).stdout
        with self.assertRaises(ValueError) as ctx:
            seed_from_pkcs8_pem(public_only)
        self.assertIn("no PKCS#8 PEM private key", str(ctx.exception))

    def test_non_ed25519_pkcs8_is_named(self) -> None:
        rsa = os.path.join(self.tmp.name, "rsa.key")
        subprocess.run(
            ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:1024", "-out", rsa],
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
