/**
 * Tests for crier.ts — run with:  node --test clients/typescript/
 *
 * The bundled ed25519 is node:crypto's, so the interesting claims here are the
 * ones the library itself makes: the PKCS#8 layer (load a `crier keygen` file,
 * derive the SAME public key the server stores), the signature payload shape
 * (over `<METHOD>\n<path>\n<ts>`, query string excluded), and the RFC 8032
 * §7.1 vectors — the cross-language check, because the Go server and the Python
 * client sign the same bytes for the same key and message (ed25519 is
 * deterministic), so agreement with the RFC's published signature IS agreement
 * with the other two implementations.
 */

import { execFileSync } from "node:child_process";
import { createPublicKey, createPrivateKey } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { strict as assert } from "node:assert";
import { after, describe, it } from "node:test";

import { Crier, CrierError, Message, SigningKey, signingBackend } from "./crier.ts";

// RFC 8032 §7.1 TEST 1..3: seed, public key, message, signature (all hex).
const RFC8032_VECTORS: Array<[string, string, string, string]> = [
  [
    "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
    "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
    "",
    "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b",
  ],
  [
    "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
    "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
    "72",
    "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00",
  ],
  [
    "c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7",
    "fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025",
    "af82",
    "6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a",
  ],
];

const workdir = mkdtempSync(join(tmpdir(), "crier-ts-test-"));
after(() => rmSync(workdir, { recursive: true, force: true }));

/** A key written by `crier keygen` (the documented path), else by openssl. */
function writeRealKey(name: string): string {
  const path = join(workdir, `${name}.key`);
  const binary = process.env["CRIER_BIN"];
  if (binary) {
    execFileSync(binary, ["keygen", "-out", path, "-id", name]);
    return path;
  }
  execFileSync("openssl", ["genpkey", "-algorithm", "ED25519", "-out", path]);
  return path;
}

describe("RFC 8032 vectors (cross-language agreement)", () => {
  it("derives the published public key and reproduces the published signature", () => {
    for (const [seedHex, publicHex, messageHex, signatureHex] of RFC8032_VECTORS) {
      const key = SigningKey.fromHex(seedHex);
      assert.equal(key.publicKeyHex, publicHex, "public key derivation");
      const message = Buffer.from(messageHex, "hex");
      assert.equal(key.sign(message).toString("hex"), signatureHex, "signature bytes");
      assert.ok(key.verify(Buffer.from(signatureHex, "hex"), message));
    }
  });

  it("rejects a flipped bit, a different message and a foreign key", () => {
    const key = SigningKey.fromHex(RFC8032_VECTORS[0][0]);
    const signature = key.sign(Buffer.from("hello"));
    const tampered = Buffer.from(signature);
    tampered[0] ^= 0x01;
    assert.equal(key.verify(tampered, Buffer.from("hello")), false);
    assert.equal(key.verify(signature, Buffer.from("hello!")), false);
    assert.equal(SigningKey.fromHex(RFC8032_VECTORS[1][0]).verify(signature, Buffer.from("hello")), false);
  });

  it("reports the signing backend", () => {
    assert.match(signingBackend(), /ed25519/);
  });
});

describe("PKCS#8 key loading", () => {
  it("loads a crier-keygen key and derives the same public half openssl does", () => {
    const path = writeRealKey("alice");
    const key = SigningKey.fromPemFile(path);
    assert.equal(key.publicKeyHex.length, 64);
    const der = execFileSync("openssl", ["pkey", "-in", path, "-pubout", "-outform", "DER"]);
    assert.equal(key.publicKeyHex, Buffer.from(der).subarray(-32).toString("hex"));
  });

  it("round-trips a hex seed to the same key as the keygen'd file (DER path, no PEM assembled)", () => {
    // Regression, twice over: the PEM construction this path used to go through
    // was rejected by OpenSSL when its base64 body wrapped with a trailing
    // newline (an empty line inside the block), and the whole-header literal it
    // needed read as key material to the repo's secrets scanner. fromHex now
    // builds PKCS#8 DER and hands DER to Node, so neither exists — while the
    // key it produces must still be the file's key.
    const path = writeRealKey("bob");
    const fromFile = SigningKey.fromPemFile(path);
    const der = createPrivateKey({ key: readFileSync(path, "utf8"), format: "pem", type: "pkcs8" }).export({
      type: "pkcs8",
      format: "der",
    });
    const seedHex = der.subarray(-32).toString("hex");
    const fromHex = SigningKey.fromHex(seedHex);
    assert.equal(fromHex.publicKeyHex, fromFile.publicKeyHex);
    assert.equal(
      fromHex.publicKeyHex,
      createPublicKey(createPrivateKey({ key: readFileSync(path, "utf8"), format: "pem", type: "pkcs8" }))
        .export({ type: "spki", format: "der" })
        .subarray(-32)
        .toString("hex"),
    );
  });

  it("names the fix for a missing file and for a wrong PEM block", () => {
    assert.throws(() => SigningKey.fromPemFile(join(workdir, "nope.key")), /crier keygen/);
    const publicPem = execFileSync("openssl", ["pkey", "-in", writeRealKey("carol"), "-pubout"]).toString();
    assert.throws(() => SigningKey.fromPemBytes(publicPem), /no PKCS#8 PEM private key/);
    assert.throws(() => SigningKey.fromHex("zz"), /expected 32/);
  });
});

describe("client surface (no server)", () => {
  const key = SigningKey.fromHex(RFC8032_VECTORS[0][0]);

  it("requires a key and an http(s) base url", () => {
    assert.throws(() => new Crier("http://localhost:1", { agentId: "alice" }), /crier keygen/);
    assert.throws(() => new Crier("ftp://localhost", { key }), /http:\/\/ or https:\/\//);
  });

  it("signs METHOD\\npath\\nts — verified against the key's own public half", () => {
    const client = new Crier("http://localhost:1", { agentId: "alice", key });
    // The header builder is private; reaching for it is deliberate — the claim
    // under test is exactly what those three headers contain.
    const internals = client as unknown as {
      headers(
        method: string,
        path: string,
        body: Buffer | undefined,
        signed: boolean,
        extra?: Record<string, string>,
      ): Record<string, string>;
    };
    const headers = internals.headers("GET", "/agents/alice/inbox", undefined, true);
    assert.equal(headers["X-Agent-ID"], "alice");
    const payload = `GET\n/agents/alice/inbox\n${headers["X-Agent-Ts"]}`;
    assert.ok(
      key.verify(Buffer.from(headers["X-Agent-Sig"], "hex"), Buffer.from(payload)),
      "X-Agent-Sig must verify over <METHOD>\\n<path>\\n<ts> with no query string",
    );
  });

  it("refuses a signed request without an agent id", async () => {
    const client = new Crier("http://localhost:1", { key });
    await assert.rejects(() => client.retrieve(), /agentId is required|needs an agent id/);
  });

  it("decodes base64 payloads and leaves non-base64 alone", () => {
    assert.deepEqual(new Message({ id: "m1", payload: Buffer.from('{"a":1}').toString("base64"), lease_id: "L" }).payload, { a: 1 });
    assert.equal(new Message({ id: "m2", payload: "not base64!!" }).payload, "not base64!!");
    assert.equal(new Message({ id: "m3" }).payload, null);
    assert.equal(new Message({ id: "m4" }, "batch-lease").leaseId, "batch-lease");
  });

  it("needs an agent id for ack and at least one message id", async () => {
    const client = new Crier("http://localhost:1", { key });
    await assert.rejects(() => client.ack([]), /at least one message id/);
    await assert.rejects(() => client.ack(new Message({ id: "" })), /at least one message id/);
  });

  it("builds a client from the environment", () => {
    process.env["CRIER_URL"] = "http://example:9999";
    process.env["CRIER_AGENT_ID"] = "carol";
    process.env["CRIER_AGENT_PRIVATE_KEY_FILE"] = join(workdir, "carol-env.key");
    try {
      assert.throws(() => Crier.fromEnv(), /crier keygen/);
      const client = Crier.fromEnv({ key });
      assert.equal(client.baseUrl, "http://example:9999");
      assert.equal(client.agentId, "carol");
    } finally {
      delete process.env["CRIER_URL"];
      delete process.env["CRIER_AGENT_ID"];
      delete process.env["CRIER_AGENT_PRIVATE_KEY_FILE"];
    }
  });

  it("surfaces the server's own error message", async () => {
    const client = new Crier("http://127.0.0.1:1", { agentId: "bob", key, timeoutMs: 200 });
    await assert.rejects(
      () => client.register(),
      (exc: unknown) => exc instanceof CrierError && exc.status === 0 && /cannot reach/.test(exc.message),
    );
  });
});
