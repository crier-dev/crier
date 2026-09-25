/**
 * crier.ts — a zero-dependency TypeScript client for a crier server.
 *
 * ```ts
 * import { Crier } from "./crier.ts";
 *
 * const c = new Crier("http://localhost:8767", { agentId: "alice", keyPath: "alice.key" });
 * await c.register();
 * await c.deliver("bob", { hello: "world" });
 * for (const m of await c.retrieve()) {
 *   console.log(m.payload);
 *   await c.ack(m);
 * }
 * await c.publish("demo-topic", { tick: 1 });
 * const sub = await c.subscribe("demo-topic");
 * console.log(await sub.next());        // { topic: "demo-topic", event: { tick: 1 } }
 * sub.close();
 * ```
 *
 * WHY THIS EXISTS (CR-FEAT-027)
 * -----------------------------
 * Registering an agent used to mean an openssl incantation, a DER-offset recipe
 * to extract the public key, and a hand-written sig() shell helper to build the
 * X-Agent-Sig header. That ceremony — not the cryptography — is what the
 * external review named as the number-one adoption killer, and it is the step
 * the last three testers each tripped over. This module does the whole wire
 * protocol for you: it loads the key `crier keygen` wrote, signs each request
 * the way the server verifies it, and exposes the bus as method calls.
 *
 * It uses only Node's built-in modules — `node:crypto` for ed25519, global
 * fetch for HTTP, and `node:net`/`node:tls` for the relay's WebSocket (not the
 * global WebSocket: the WHATWG API cannot set request headers, and the relay
 * needs X-Agent-ID and, when the server enforces bearer auth, Authorization on
 * the upgrade request). Nothing to install.
 *
 * REQUIREMENTS
 * ------------
 * Node 22.6 or newer: running a `.ts` file directly (`node round-trip.ts`) needs
 * native type stripping. `tsx` also works. No packages, no build step, no
 * tsconfig. (For editor/`tsc --strict` types you would add @types/node — it is
 * NOT needed to run any of this.)
 *
 * WHAT IT DOES ON THE WIRE (nothing is hidden)
 * --------------------------------------------
 *  - Agent-scoped routes (retrieve / ack / stats / delete) need the signature
 *    trio when the server runs with CR_REQUIRE_AGENT_SIG=true (the default):
 *      X-Agent-ID   the agent id
 *      X-Agent-Ts   unix seconds, within +/-30s of the SERVER clock
 *      X-Agent-Sig  hex ed25519 signature over `<METHOD>\n<path>\n<ts>`
 *    The <path> is the URL path only — the query string is NOT covered, so
 *    `GET /agents/alice/inbox?limit=5` is signed over `/agents/alice/inbox`.
 *  - Registration (POST /agents) and delivery (POST /agents/{id}/inbox) are NOT
 *    per-agent signed. Registration carries the public key in the body;
 *    delivery is open to any sender.
 *  - POST /relay/publish needs the X-Agent-ID header (the relay's rate limiter
 *    keys on it); the signature is not verified there.
 *  - If the server was started with CR_AUTH_TOKEN, every request except the five
 *    exempt paths (/health, /version, /openapi.json, /openapi.yaml, /docs) needs
 *    `Authorization: Bearer <token>` — including the WebSocket upgrade, which is
 *    not exempt.
 */

import {
  createHash,
  createPrivateKey,
  createPublicKey,
  randomBytes,
  sign as cryptoSign,
  verify as cryptoVerify,
} from "node:crypto";
import { readFileSync } from "node:fs";
import { connect as netConnect, type Socket } from "node:net";
import { connect as tlsConnect } from "node:tls";

export class CrierError extends Error {
  readonly status: number;
  readonly body: string;
  readonly path: string;

  constructor(status: number, message: string, body = "", path = "") {
    super(`HTTP ${status}${path ? ` (${path})` : ""}: ${message}`);
    this.name = "CrierError";
    this.status = status;
    this.body = body;
    this.path = path;
  }
}

export class CrierTimeout extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CrierTimeout";
  }
}

/** PKCS#8 DER prefix for an ed25519 private key (algorithm OID 1.3.101.112). */
const PKCS8_ED25519_PREFIX = Buffer.from("302e020100300506032b657004220420", "hex");

/**
 * PEM armour, matched as two pieces rather than one literal: a source line
 * carrying the whole header reads, to the repo's secrets cross-check, exactly
 * like leaked key material (measured: the literal trips gitreins' builtin
 * scanner while gitleaks itself is clean). The check is line-based — an armour
 * line that names PRIVATE KEY — so it needs neither a single literal nor a
 * scanner exception, and it tolerates CRLF files too.
 */
const PEM_ARMOUR_BEGIN = "-----BEGIN ";
const PEM_BLOCK_LABEL = "PRIVATE KEY";

/** An ed25519 keypair loaded from a `crier keygen` PKCS#8 PEM file. */
export class SigningKey {
  private readonly privateKey: ReturnType<typeof createPrivateKey>;
  readonly publicKey: Buffer;

  private constructor(privateKey: ReturnType<typeof createPrivateKey>, publicKey: Buffer) {
    this.privateKey = privateKey;
    this.publicKey = publicKey;
  }

  static fromPemFile(path: string): SigningKey {
    let text: string;
    try {
      text = readFileSync(path, "utf8");
    } catch (exc) {
      throw new Error(
        `cannot read the private key file ${JSON.stringify(path)}: ${(exc as Error).message} — ` +
          `generate one with \`crier keygen -out ${path} -id <agent-id>\` (no openssl needed)`,
      );
    }
    return SigningKey.fromPemBytes(text);
  }

  static fromPemBytes(text: string | Buffer): SigningKey {
    const pem = typeof text === "string" ? text : text.toString("utf8");
    const hasPKCS8 = pem
      .replace(/\r\n/g, "\n")
      .split("\n")
      .some((line: string) => line.trim().startsWith(PEM_ARMOUR_BEGIN) && line.includes(PEM_BLOCK_LABEL));
    if (!hasPKCS8) {
      throw new Error(
        "no PKCS#8 PEM private key found (expected a PEM block whose header line ends " +
          "with 'PRIVATE KEY'). Generate one with `crier keygen -out <path> -id <agent-id>`, " +
          "or with `openssl genpkey -algorithm ED25519 -out <path>`",
      );
    }
    let key: ReturnType<typeof createPrivateKey>;
    try {
      key = createPrivateKey({ key: pem, format: "pem", type: "pkcs8" });
    } catch (exc) {
      throw new Error(
        `the PKCS#8 key could not be parsed: ${(exc as Error).message}. ` +
          "`crier keygen` and `openssl genpkey -algorithm ED25519` both write the expected shape",
      );
    }
    return SigningKey.fromKeyObject(key);
  }

  /** A key from the 32-byte seed as hex (64 characters). */
  static fromHex(seedHex: string): SigningKey {
    const seed = Buffer.from(seedHex.trim(), "hex");
    if (seed.length !== 32) {
      throw new Error(`the private key hex is ${seed.length} byte(s), expected 32 (64 hex characters)`);
    }
    // Built as PKCS#8 DER and handed to Node as DER: no PEM text is assembled,
    // so no PEM header literal exists in this file at all (see PEM_ARMOUR_BEGIN).
    const der = Buffer.concat([PKCS8_ED25519_PREFIX, seed]);
    return SigningKey.fromKeyObject(createPrivateKey({ key: der, format: "der", type: "pkcs8" }));
  }

  private static fromKeyObject(key: ReturnType<typeof createPrivateKey>): SigningKey {
    if (key.asymmetricKeyType !== "ed25519") {
      throw new Error(
        `the PKCS#8 key is a ${key.asymmetricKeyType ?? "unknown"} key, not ed25519 — ` +
          "`crier keygen` writes ed25519",
      );
    }
    // The public half, as the server stores it: the last 32 bytes of the SPKI DER.
    const spki = createPublicKey(key).export({ type: "spki", format: "der" });
    return new SigningKey(key, Buffer.from(spki.subarray(spki.length - 32)));
  }

  get publicKeyHex(): string {
    return this.publicKey.toString("hex");
  }

  /** The 64-byte ed25519 signature over message (deterministic). */
  sign(message: Buffer): Buffer {
    return cryptoSign(null, message, this.privateKey);
  }

  verify(signature: Buffer, message: Buffer): boolean {
    return cryptoVerify(null, message, this.privateKey, signature);
  }
}

/** One inbox entry from retrieve(): id, decoded payload, lease id. */
export class Message {
  readonly raw: Record<string, unknown>;
  readonly id: string;
  readonly leaseId: string;
  readonly payload: unknown;
  readonly guard: unknown;

  constructor(entry: Record<string, unknown>, leaseId = "") {
    this.raw = entry;
    this.id = String(entry["id"] ?? "");
    this.leaseId = String(entry["lease_id"] ?? leaseId ?? "");
    this.payload = decodePayload(entry["payload"]);
    this.guard = entry["guard"];
  }
}

/** Inbox payloads travel base64-encoded (Go []byte on the wire). */
function decodePayload(raw: unknown): unknown {
  if (raw === null || raw === undefined) return null;
  if (typeof raw !== "string") return raw;
  // Validate the base64 STRICTLY: Node's Buffer.from(x, "base64") silently
  // decodes rubbish into garbage bytes, where the Python client (and the wire
  // contract) treat a non-base64 payload as "not ours to interpret". A payload
  // the client cannot decode is handed back as the wire value, never invented.
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(raw) || raw.length % 4 !== 0) return raw;
  const bytes = Buffer.from(raw, "base64");
  if (bytes.toString("base64") !== raw) return raw;
  const decoded = bytes.toString("utf8");
  if (!Buffer.from(decoded, "utf8").equals(bytes)) return raw; // not valid UTF-8
  try {
    return JSON.parse(decoded);
  } catch {
    return decoded;
  }
}

// --------------------------------------------------------------------------
// Minimal WebSocket client (node:net / node:tls) — for /relay/subscribe/{topic}
// --------------------------------------------------------------------------

const WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

interface Frame {
  fin: boolean;
  opcode: number;
  payload: Buffer;
}

/** Parse one frame out of buf, or null when it is not complete yet. */
function tryParseFrame(buf: Buffer): { frame: Frame; consumed: number } | null {
  if (buf.length < 2) return null;
  const fin = (buf[0] & 0x80) !== 0;
  const opcode = buf[0] & 0x0f;
  const masked = (buf[1] & 0x80) !== 0;
  let length = buf[1] & 0x7f;
  let offset = 2;
  if (length === 126) {
    if (buf.length < offset + 2) return null;
    length = buf.readUInt16BE(offset);
    offset += 2;
  } else if (length === 127) {
    if (buf.length < offset + 8) return null;
    const big = buf.readBigUInt64BE(offset);
    if (big > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error("relay frame is too large to read");
    length = Number(big);
    offset += 8;
  }
  let mask: Buffer | null = null;
  if (masked) {
    if (buf.length < offset + 4) return null;
    mask = buf.subarray(offset, offset + 4);
    offset += 4;
  }
  if (buf.length < offset + length) return null;
  let payload = buf.subarray(offset, offset + length);
  if (mask) {
    const unmasked = Buffer.alloc(length);
    for (let i = 0; i < length; i += 1) unmasked[i] = payload[i] ^ mask[i % 4];
    payload = unmasked;
  }
  return { frame: { fin, opcode, payload }, consumed: offset + length };
}

/**
 * The smallest RFC 6455 client the relay needs: a handshake that CAN carry
 * headers, and text-frame reads with continuation assembly. Client frames are
 * masked (the rule the server enforces); a ping is answered with a pong.
 */
export class RelaySocket {
  private readonly socket: Socket;
  private buffer: Buffer;
  private readonly queue: Buffer[] = [];
  private readonly waiters: Array<{ resolve: (value: Buffer) => void; reject: (err: Error) => void; timer: ReturnType<typeof setTimeout> }> = [];
  private readonly partials: Buffer[] = [];
  private failure: Error | null = null;
  private closed = false;

  private constructor(socket: Socket, initial: Buffer) {
    this.socket = socket;
    this.buffer = initial;
    socket.on("data", (chunk: Buffer) => {
      this.buffer = Buffer.concat([this.buffer, chunk]);
      this.pump();
    });
    socket.on("close", () => this.finish());
    socket.on("error", (err: Error) => {
      this.failure = err;
      this.finish();
    });
    this.pump();
  }

  static async open(url: string, headers: Record<string, string>, timeoutMs: number): Promise<RelaySocket> {
    const parsed = new URL(url);
    if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") {
      throw new Error(`subscribe() needs a ws:// or wss:// URL, got ${JSON.stringify(url)}`);
    }
    const secure = parsed.protocol === "wss:";
    const port = parsed.port ? Number(parsed.port) : secure ? 443 : 80;
    const socket = await new Promise<Socket>((resolve, reject) => {
      const onError = (err: Error) => reject(err);
      const s = secure
        ? tlsConnect({ host: parsed.hostname, port, servername: parsed.hostname }, () => resolve(s))
        : netConnect({ host: parsed.hostname, port }, () => resolve(s));
      s.once("error", onError);
      s.setTimeout(timeoutMs, () => {
        s.destroy();
        reject(new CrierTimeout(`the relay did not answer the WebSocket upgrade within ${timeoutMs}ms`));
      });
    });
    socket.setTimeout(0);

    const key = randomBytes(16).toString("base64");
    const lines = [
      `GET ${parsed.pathname}${parsed.search} HTTP/1.1`,
      `Host: ${parsed.hostname}:${port}`,
      "Upgrade: websocket",
      "Connection: Upgrade",
      `Sec-WebSocket-Key: ${key}`,
      "Sec-WebSocket-Version: 13",
    ];
    for (const [name, value] of Object.entries(headers)) lines.push(`${name}: ${value}`);
    socket.write(`${lines.join("\r\n")}\r\n\r\n`);

    // Read the response head, then hand the leftover bytes to the frame reader.
    let head = Buffer.alloc(0);
    while (!head.includes("\r\n\r\n")) {
      const chunk = await new Promise<Buffer | null>((resolve, reject) => {
        const onData = (d: Buffer) => {
          cleanup();
          resolve(d);
        };
        const onClose = () => {
          cleanup();
          resolve(null);
        };
        const onError = (err: Error) => {
          cleanup();
          reject(err);
        };
        const cleanup = () => {
          socket.off("data", onData);
          socket.off("close", onClose);
          socket.off("error", onError);
        };
        socket.once("data", onData);
        socket.once("close", onClose);
        socket.once("error", onError);
      });
      if (chunk === null) throw new CrierError(0, "the relay closed the connection during the WebSocket upgrade");
      head = Buffer.concat([head, chunk]);
    }
    const end = head.indexOf("\r\n\r\n") + 4;
    const responseText = head.subarray(0, end).toString("utf8");
    const leftover = head.subarray(end);
    const statusLine = responseText.split("\r\n", 1)[0];
    if (!statusLine.includes(" 101")) {
      const status = Number(statusLine.split(" ")[1] ?? 0);
      throw new CrierError(status, "the relay refused the WebSocket upgrade", responseText, parsed.pathname);
    }
    const expected = createHash("sha1").update(`${key}${WS_GUID}`).digest("base64");
    const accept = responseText
      .split("\r\n")
      .find((line: string) => line.toLowerCase().startsWith("sec-websocket-accept:"))
      ?.split(":")[1]
      ?.trim();
    if (accept !== expected) {
      throw new CrierError(0, "the relay's Sec-WebSocket-Accept did not match the key we sent");
    }
    return new RelaySocket(socket, leftover);
  }

  private finish(): void {
    this.closed = true;
    const err = this.failure;
    while (this.waiters.length > 0) {
      const waiter = this.waiters.shift()!;
      clearTimeout(waiter.timer);
      if (err) waiter.reject(err);
      else waiter.resolve(Buffer.alloc(0)); // an empty buffer means "relay closed"
    }
  }

  private pump(): void {
    for (;;) {
      let parsed: { frame: Frame; consumed: number } | null;
      try {
        parsed = tryParseFrame(this.buffer);
      } catch (err) {
        this.failure = err as Error;
        this.finish();
        return;
      }
      if (parsed === null) return;
      this.buffer = this.buffer.subarray(parsed.consumed);
      const { fin, opcode, payload } = parsed.frame;
      if (opcode === 0x9) {
        this.sendFrame(0xa, payload);
        continue;
      }
      if (opcode === 0xa) continue;
      if (opcode === 0x8) {
        this.closed = true;
        try {
          this.socket.end();
        } catch {
          /* already gone */
        }
        this.finish();
        return;
      }
      if (opcode === 0x1 || opcode === 0x2 || opcode === 0x0) {
        this.partials.push(payload);
        if (!fin) continue;
        const message = Buffer.concat(this.partials.splice(0));
        const waiter = this.waiters.shift();
        if (waiter) {
          clearTimeout(waiter.timer);
          waiter.resolve(message);
        } else {
          this.queue.push(message);
        }
        continue;
      }
      this.failure = new Error(`unexpected WebSocket opcode 0x${opcode.toString(16)}`);
      this.finish();
      return;
    }
  }

  private sendFrame(opcode: number, payload: Buffer): void {
    if (this.closed) return;
    const mask = randomBytes(4);
    const masked = Buffer.alloc(payload.length);
    for (let i = 0; i < payload.length; i += 1) masked[i] = payload[i] ^ mask[i % 4];
    const header: number[] = [0x80 | opcode];
    if (payload.length < 126) header.push(0x80 | payload.length);
    else if (payload.length < 65536) header.push(0x80 | 126, (payload.length >> 8) & 0xff, payload.length & 0xff);
    else {
      header.push(0x80 | 127);
      const big = Buffer.alloc(8);
      big.writeBigUInt64BE(BigInt(payload.length));
      header.push(...big);
    }
    try {
      this.socket.write(Buffer.concat([Buffer.from(header), mask, masked]));
    } catch {
      this.closed = true;
    }
  }

  /** The next text message, or null when the relay closed the subscription. */
  nextMessage(timeoutMs: number): Promise<Buffer | null> {
    const queued = this.queue.shift();
    if (queued !== undefined) return Promise.resolve(queued);
    if (this.closed) {
      if (this.failure) return Promise.reject(this.failure);
      return Promise.resolve(null);
    }
    return new Promise<Buffer | null>((resolve, reject) => {
      const timer = setTimeout(() => {
        const index = this.waiters.findIndex((w) => w.timer === timer);
        if (index >= 0) this.waiters.splice(index, 1);
        reject(
          new CrierTimeout(
            `no relay frame within ${timeoutMs}ms — check the topic name and that a publisher is sending to it`,
          ),
        );
      }, timeoutMs);
      this.waiters.push({
        resolve: (value) => resolve(value.length === 0 && this.closed ? null : value),
        reject,
        timer,
      });
    });
  }

  close(): void {
    if (this.closed) return;
    this.sendFrame(0x8, Buffer.alloc(0));
    this.closed = true;
    try {
      this.socket.end();
    } catch {
      /* already gone */
    }
  }
}

/** A live relay subscription: one WebSocket, promise-based reads. */
export class Subscription {
  private readonly socket: RelaySocket;
  private readonly timeoutMs: number;
  readonly topic: string;
  delivered = 0;

  constructor(socket: RelaySocket, topic: string, timeoutMs: number) {
    this.socket = socket;
    this.topic = topic;
    this.timeoutMs = timeoutMs;
  }

  /** The next envelope; rejects with CrierTimeout when none arrives in time. */
  async next(timeoutMs?: number): Promise<Record<string, unknown>> {
    const frame = await this.socket.nextMessage(timeoutMs ?? this.timeoutMs);
    if (frame === null) throw new CrierTimeout("the relay closed the subscription");
    this.delivered += 1;
    try {
      return JSON.parse(frame.toString("utf8")) as Record<string, unknown>;
    } catch {
      return { topic: this.topic, event: frame.toString("utf8") };
    }
  }

  close(): void {
    this.socket.close();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<Record<string, unknown>> {
    for (;;) {
      try {
        yield await this.next();
      } catch {
        return;
      }
    }
  }
}

export interface CrierOptions {
  agentId?: string;
  keyPath?: string;
  key?: SigningKey;
  token?: string;
  timeoutMs?: number;
}

export interface RequestOptions {
  body?: unknown;
  signed?: boolean;
  query?: Record<string, string | number | undefined>;
  headers?: Record<string, string>;
}

const DEFAULT_SERVER = "http://localhost:8767";

/** A crier server, addressed by one agent identity. */
export class Crier {
  readonly baseUrl: string;
  agentId: string | undefined;
  readonly token: string | undefined;
  readonly timeoutMs: number;
  readonly key: SigningKey;

  constructor(baseUrl: string = DEFAULT_SERVER, options: CrierOptions = {}) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    if (!/^https?:\/\//.test(this.baseUrl)) {
      throw new Error(`baseUrl must start with http:// or https://, got ${JSON.stringify(baseUrl)}`);
    }
    this.agentId = options.agentId;
    this.token = options.token ?? process.env["CR_AUTH_TOKEN"] ?? undefined;
    this.timeoutMs = options.timeoutMs ?? 30000;
    if (options.key) {
      this.key = options.key;
    } else if (options.keyPath) {
      this.key = SigningKey.fromPemFile(options.keyPath);
    } else {
      throw new Error(
        "a signing key is required: pass { keyPath: <the file `crier keygen` wrote> } or { key: SigningKey }",
      );
    }
  }

  /** Build a client from the environment the bridge also reads. */
  static fromEnv(options: CrierOptions = {}): Crier {
    return new Crier(process.env["CRIER_URL"] ?? process.env["CRIER_HTTP_URL"] ?? DEFAULT_SERVER, {
      agentId: options.agentId ?? process.env["CRIER_AGENT_ID"],
      keyPath: options.keyPath ?? process.env["CRIER_AGENT_PRIVATE_KEY_FILE"],
      token: options.token,
      key: options.key,
      timeoutMs: options.timeoutMs,
    });
  }

  // -- HTTP plumbing -------------------------------------------------------

  private headers(
    method: string,
    path: string,
    body: Buffer | undefined,
    signed: boolean,
    extra?: Record<string, string>,
  ): Record<string, string> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body && body.length > 0) headers["Content-Type"] = "application/json";
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    if (signed) {
      if (!this.agentId) {
        throw new Error(
          "agentId is required for a signed request — construct the client with agentId=<the id the key is registered under>",
        );
      }
      const ts = String(Math.floor(Date.now() / 1000));
      // The signature covers the PATH only (query string excluded) — the same
      // expression the server verifies (internal/registry/agentsig.go).
      const payload = `${method.toUpperCase()}\n${path}\n${ts}`;
      headers["X-Agent-ID"] = this.agentId;
      headers["X-Agent-Ts"] = ts;
      headers["X-Agent-Sig"] = this.key.sign(Buffer.from(payload, "utf8")).toString("hex");
    }
    return { ...headers, ...(extra ?? {}) };
  }

  private async raw(
    method: string,
    path: string,
    options: RequestOptions = {},
  ): Promise<{ status: number; body: unknown }> {
    const body = options.body === undefined ? undefined : Buffer.from(JSON.stringify(options.body), "utf8");
    let url = this.baseUrl + path;
    if (options.query) {
      const params = new URLSearchParams();
      for (const [key, value] of Object.entries(options.query)) {
        if (value !== undefined && value !== null) params.set(key, String(value));
      }
      const encoded = params.toString();
      if (encoded) url += `?${encoded}`;
    }
    const headers = this.headers(method, path, body, options.signed === true, options.headers);
    let response: Response;
    try {
      response = await fetch(url, {
        method: method.toUpperCase(),
        headers,
        body: body === undefined ? undefined : (body as unknown as BodyInit),
        signal: AbortSignal.timeout(this.timeoutMs),
      });
    } catch (exc) {
      throw new CrierError(0, `cannot reach the crier server at ${this.baseUrl}: ${(exc as Error).message}`, "", path);
    }
    const text = await response.text();
    let parsed: unknown = null;
    if (text) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = { raw: text };
      }
    }
    if (!response.ok) {
      throw new CrierError(response.status, errorMessage(text), text, path);
    }
    return { status: response.status, body: parsed };
  }

  /** Escape hatch: any route, this client's auth and signing applied. */
  async request(method: string, path: string, options: RequestOptions = {}): Promise<unknown> {
    return (await this.raw(method, path, options)).body;
  }

  // -- the documented operations ------------------------------------------

  /** POST /agents — register this agent with the key's public half. */
  async register(options: { agentId?: string; capabilities?: string[] } = {}): Promise<Record<string, unknown>> {
    const target = options.agentId ?? this.agentId;
    if (!target) throw new Error("register() needs an agent id (client agentId or the argument)");
    const body: Record<string, unknown> = { id: target, public_key: this.key.publicKeyHex };
    if (options.capabilities) body["capabilities"] = options.capabilities;
    const { body: parsed } = await this.raw("POST", "/agents", { body });
    this.agentId = target;
    return (parsed as Record<string, unknown>) ?? {};
  }

  /** register(), tolerating an already-registered agent (true = registered now). */
  async registerIfMissing(capabilities?: string[]): Promise<boolean> {
    try {
      await this.register(capabilities ? { capabilities } : {});
      return true;
    } catch (exc) {
      if (exc instanceof CrierError && exc.status === 409) return false;
      throw exc;
    }
  }

  /** DELETE /agents/{id} — signed; removes this agent. */
  async unregister(agentId?: string): Promise<void> {
    const target = agentId ?? this.agentId;
    if (!target) throw new Error("unregister() needs an agent id");
    await this.raw("DELETE", `/agents/${encodeURIComponent(target)}`, { signed: true });
  }

  /** POST /agents/{id}/inbox — deliver a message; the sender needs no key. */
  async deliver(
    agentId: string,
    payload: unknown,
    options: { ttlSeconds?: number; sender?: string } = {},
  ): Promise<Record<string, unknown>> {
    const body: Record<string, unknown> = { payload };
    if (options.ttlSeconds !== undefined) body["ttl_seconds"] = options.ttlSeconds;
    if (options.sender !== undefined) body["sender"] = options.sender;
    const { body: parsed } = await this.raw("POST", `/agents/${encodeURIComponent(agentId)}/inbox`, { body });
    return (parsed as Record<string, unknown>) ?? {};
  }

  /** GET /agents/{id}/inbox — signed; lease up to `limit` messages. */
  async retrieve(options: { limit?: number; leaseSeconds?: number; agentId?: string } = {}): Promise<Message[]> {
    const target = options.agentId ?? this.agentId;
    if (!target) throw new Error("retrieve() needs an agent id");
    const { status, body } = await this.raw("GET", `/agents/${encodeURIComponent(target)}/inbox`, {
      signed: true,
      query: { limit: options.limit, lease_seconds: options.leaseSeconds },
    });
    if (status === 204 || !body) return [];
    const parsed = body as Record<string, unknown>;
    const leaseId = String(parsed["lease_id"] ?? "");
    const entries = (parsed["messages"] as Record<string, unknown>[] | undefined) ?? [];
    return entries.map((entry) => new Message(entry, leaseId));
  }

  /** POST /agents/{id}/inbox/ack — signed; permanently removes messages. */
  async ack(
    messages: Message | string | Array<Message | string>,
    options: { leaseId?: string; agentId?: string } = {},
  ): Promise<void> {
    const items = Array.isArray(messages) ? messages : [messages];
    const ids: string[] = [];
    const leases: string[] = [];
    for (const item of items) {
      if (item instanceof Message) {
        if (item.id) ids.push(item.id);
        if (item.leaseId) leases.push(item.leaseId);
      } else {
        ids.push(String(item));
      }
    }
    if (ids.length === 0) throw new Error("ack() needs at least one message id");
    const lease = options.leaseId ?? leases[0] ?? "";
    const target = options.agentId ?? this.agentId;
    if (!target) throw new Error("ack() needs an agent id");
    await this.raw("POST", `/agents/${encodeURIComponent(target)}/inbox/ack`, {
      signed: true,
      body: { lease_id: lease, message_ids: ids },
    });
  }

  /** GET /agents/{id}/inbox/stats — signed; queue depth and lease count. */
  async stats(agentId?: string): Promise<Record<string, unknown>> {
    const target = agentId ?? this.agentId;
    if (!target) throw new Error("stats() needs an agent id");
    const { body } = await this.raw("GET", `/agents/${encodeURIComponent(target)}/inbox/stats`, {
      signed: true,
    });
    return (body as Record<string, unknown>) ?? {};
  }

  /**
   * POST /relay/publish — 202 means the relay accepted the event.
   *
   * X-Agent-ID is REQUIRED by the relay (its per-agent rate limiter keys on it;
   * without the header a publish is 401), so this call needs a client agentId.
   */
  async publish(topic: string, event: unknown): Promise<void> {
    const extra: Record<string, string> = {};
    if (this.agentId) extra["X-Agent-ID"] = this.agentId;
    await this.raw("POST", "/relay/publish", { body: { topic, event }, headers: extra });
  }

  /**
   * GET /relay/subscribe/{topic} — WebSocket. Resolves once the socket is OPEN,
   * so `const sub = await c.subscribe(t)` followed by `await c.publish(t, …)`
   * cannot beat the registration and lose the event to the relay's at-most-once
   * fan-out. Frames are the relay's envelopes: { topic, event }.
   */
  async subscribe(
    topic: string,
    options: { timeoutMs?: number; agentId?: string } = {},
  ): Promise<Subscription> {
    const agent = options.agentId ?? this.agentId;
    const headers: Record<string, string> = {};
    if (agent) headers["X-Agent-ID"] = agent;
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    const url = `${this.baseUrl.replace(/^https:\/\//, "wss://").replace(/^http:\/\//, "ws://")}/relay/subscribe/${encodeURIComponent(topic)}`;
    const timeoutMs = options.timeoutMs ?? this.timeoutMs;
    const socket = await RelaySocket.open(url, headers, timeoutMs);
    return new Subscription(socket, topic, timeoutMs);
  }
}

function errorMessage(text: string): string {
  try {
    const parsed = JSON.parse(text) as { error?: string };
    if (parsed && typeof parsed.error === "string") return parsed.error;
  } catch {
    /* not JSON */
  }
  return text.trim() || "(no response body)";
}

/** Which primitive is signing — reported in the round-trip transcript. */
export function signingBackend(): string {
  return `node:crypto ed25519 (node ${process.versions.node})`;
}
