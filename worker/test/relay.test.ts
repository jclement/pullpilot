/**
 * Integration tests for the PullPilot webhook relay.
 *
 * These run inside workerd via @cloudflare/vitest-pool-workers, so the full
 * stack (routing -> KV -> Durable Object -> hibernatable WebSocket -> Ed25519
 * WebCrypto) is exercised exactly as in production. We use `SELF` to drive HTTP
 * requests through the deployed Worker entrypoint.
 */

import { env, SELF } from "cloudflare:test";
import { describe, it, expect } from "vitest";

// Domain separation tag — must match src/relay.ts and the Go daemon.
const DOMAIN = "pullpilot-webhook-relay/v1";

// --- helpers ---------------------------------------------------------------

function b64urlEncode(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64urlDecode(s: string): Uint8Array {
  const norm = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = norm.length % 4 === 0 ? "" : "=".repeat(4 - (norm.length % 4));
  const bin = atob(norm + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/** Generate an Ed25519 keypair and return the raw public key bytes + key pair. */
async function genKeypair() {
  const kp = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
  const rawPub = new Uint8Array(
    (await crypto.subtle.exportKey("raw", kp.publicKey)) as ArrayBuffer,
  );
  return { kp, rawPub };
}

/** Provision a webhook for a fresh keypair; returns ids + signing material. */
async function provision(label = "test") {
  const { kp, rawPub } = await genKeypair();
  const res = await SELF.fetch("https://relay.test/v1/provision", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ pubkey: b64urlEncode(rawPub), label }),
  });
  expect(res.status).toBe(201);
  const body = (await res.json()) as {
    webhookId: string;
    pokeUrl: string;
    listenUrl: string;
    ttlDays: number;
  };
  return { ...body, kp };
}

/** Sign DOMAIN || webhookId || challenge with the private key. */
async function signChallenge(
  privateKey: CryptoKey,
  webhookId: string,
  challengeB64: string,
): Promise<string> {
  const message = concat(
    new TextEncoder().encode(DOMAIN),
    new TextEncoder().encode(webhookId),
    b64urlDecode(challengeB64),
  );
  const sig = new Uint8Array(
    await crypto.subtle.sign({ name: "Ed25519" }, privateKey, message),
  );
  return b64urlEncode(sig);
}

/** Open a listen WebSocket and return it (already in CONNECTING/OPEN). */
async function openListen(listenUrl: string): Promise<WebSocket> {
  // Convert wss://host/... to a SELF.fetch upgrade request.
  const httpUrl = listenUrl.replace(/^wss?:/, "https:");
  const res = await SELF.fetch(httpUrl, {
    headers: { Upgrade: "websocket" },
  });
  expect(res.status).toBe(101);
  const ws = res.webSocket!;
  ws.accept();
  return ws;
}

/** Wait for the next text message frame, with a timeout. */
function nextMessage(ws: WebSocket, timeoutMs = 5000): Promise<any> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("timed out waiting for message")),
      timeoutMs,
    );
    ws.addEventListener(
      "message",
      (ev) => {
        clearTimeout(timer);
        resolve(JSON.parse(ev.data as string));
      },
      { once: true },
    );
  });
}

/** Wait for the socket to close, returning the close code. */
function nextClose(ws: WebSocket, timeoutMs = 5000): Promise<number> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("timed out waiting for close")),
      timeoutMs,
    );
    ws.addEventListener(
      "close",
      (ev) => {
        clearTimeout(timer);
        resolve(ev.code);
      },
      { once: true },
    );
  });
}

/** Full handshake: open, receive hello, sign, send auth, expect ready. */
async function connectAuthed(listenUrl: string, webhookId: string, kp: CryptoKeyPair) {
  const ws = await openListen(listenUrl);
  const hello = await nextMessage(ws);
  expect(hello.type).toBe("hello");
  expect(hello.v).toBe(1);
  expect(typeof hello.challenge).toBe("string");

  const sig = await signChallenge(kp.privateKey, webhookId, hello.challenge);
  ws.send(JSON.stringify({ type: "auth", sig }));

  const ready = await nextMessage(ws);
  expect(ready.type).toBe("ready");
  return ws;
}

// --- tests -----------------------------------------------------------------

describe("health & metadata", () => {
  it("GET /healthz returns ok", async () => {
    const res = await SELF.fetch("https://relay.test/healthz");
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("ok");
  });

  it("GET /version returns the configured version", async () => {
    const res = await SELF.fetch("https://relay.test/version");
    expect(res.status).toBe(200);
    const body = (await res.json()) as { version: string };
    expect(body.version).toBe(env.WORKER_VERSION ?? "dev");
  });
});

describe("provision", () => {
  it("returns a webhookId and poke/listen URLs", async () => {
    const p = await provision("my-daemon");
    expect(p.webhookId).toMatch(/^wh_[0-9a-f]{64}$/);
    expect(p.pokeUrl).toBe(`https://relay.test/v1/poke/${p.webhookId}`);
    expect(p.listenUrl).toBe(`wss://relay.test/v1/listen/${p.webhookId}`);
    expect(p.ttlDays).toBe(180); // 15552000 / 86400
  });

  it("rejects a malformed pubkey", async () => {
    const res = await SELF.fetch("https://relay.test/v1/provision", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ pubkey: "not-base64url!!" }),
    });
    expect(res.status).toBe(400);
  });

  it("rejects a wrong-length pubkey", async () => {
    const res = await SELF.fetch("https://relay.test/v1/provision", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ pubkey: b64urlEncode(new Uint8Array(16)) }),
    });
    expect(res.status).toBe(400);
  });
});

describe("listen + auth", () => {
  it("rejects an unknown webhook with 404", async () => {
    const res = await SELF.fetch(
      "https://relay.test/v1/listen/wh_deadbeef",
      { headers: { Upgrade: "websocket" } },
    );
    expect(res.status).toBe(404);
  });

  it("authenticates with a valid signature and replies ready", async () => {
    const p = await provision();
    const ws = await connectAuthed(p.listenUrl, p.webhookId, p.kp);
    ws.close();
  });

  it("closes the socket (1008) on a bad signature", async () => {
    const p = await provision();
    const ws = await openListen(p.listenUrl);
    const hello = await nextMessage(ws);
    expect(hello.type).toBe("hello");

    // Sign garbage instead of the real challenge.
    const bogus = b64urlEncode(new Uint8Array(64));
    ws.send(JSON.stringify({ type: "auth", sig: bogus }));

    const code = await nextClose(ws);
    expect(code).toBe(1008);
  });

  it("closes (1008) when signing the wrong message with the right key", async () => {
    const p = await provision();
    const ws = await openListen(p.listenUrl);
    const hello = await nextMessage(ws);

    // Correct key, but sign a different challenge => verification fails.
    const wrongChallenge = b64urlEncode(crypto.getRandomValues(new Uint8Array(32)));
    const sig = await signChallenge(p.kp.privateKey, p.webhookId, wrongChallenge);
    ws.send(JSON.stringify({ type: "auth", sig }));

    const code = await nextClose(ws);
    expect(code).toBe(1008);
  });
});

describe("poke delivery", () => {
  it("returns 404 for an unknown webhook", async () => {
    const res = await SELF.fetch("https://relay.test/v1/poke/wh_nope", {
      method: "POST",
    });
    expect(res.status).toBe(404);
  });

  it("delivers a poke to a connected authed socket", async () => {
    const p = await provision();
    const ws = await connectAuthed(p.listenUrl, p.webhookId, p.kp);

    // Attach the message listener BEFORE triggering the poke so we cannot miss
    // a frame that is delivered synchronously during the poke fetch.
    const pokePromise = nextMessage(ws);

    const pokeRes = await SELF.fetch(p.pokeUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ reason: "ci-test" }),
    });
    expect(pokeRes.status).toBe(202);
    expect(await pokeRes.json()).toEqual({ status: "accepted" });

    const poke = await pokePromise;
    expect(poke.type).toBe("poke");
    expect(poke.v).toBe(1);
    expect(poke.id).toMatch(/^[0-9a-f]{32}$/);
    expect(typeof poke.ts).toBe("string");
    expect(poke.reason).toBe("ci-test");
    ws.close();
  });

  it("stores a pending poke when no listener is connected and flushes on auth", async () => {
    const p = await provision();

    // Poke with nobody listening: should be accepted and stored as pending.
    const pokeRes = await SELF.fetch(p.pokeUrl, { method: "POST" });
    expect(pokeRes.status).toBe(202);

    // Now connect + auth: the pending poke must be flushed immediately.
    const ws = await connectAuthed(p.listenUrl, p.webhookId, p.kp);
    const poke = await nextMessage(ws);
    expect(poke.type).toBe("poke");
    expect(poke.reason).toBeNull(); // pending pokes are content-free
    ws.close();
  });

  it("accepts a GET poke (dumb registries)", async () => {
    const p = await provision();
    const res = await SELF.fetch(p.pokeUrl, { method: "GET" });
    expect(res.status).toBe(202);
    expect(await res.json()).toEqual({ status: "accepted" });
  });
});

describe("unauthed socket reaping (DoS guard)", () => {
  it("rejects unauthed sockets past the cap with 1013", async () => {
    const p = await provision();
    const MAX_UNAUTHED_SOCKETS = 8;

    // Open the cap's worth of unauthed sockets and keep them parked.
    const parked: WebSocket[] = [];
    for (let i = 0; i < MAX_UNAUTHED_SOCKETS; i++) {
      const ws = await openListen(p.listenUrl);
      const hello = await nextMessage(ws);
      expect(hello.type).toBe("hello");
      parked.push(ws);
    }

    // The next unauthed socket must be rejected with the too-many code (1013).
    const overflow = await openListen(p.listenUrl);
    const code = await nextClose(overflow);
    expect(code).toBe(1013); // CLOSE_TOO_MANY

    for (const ws of parked) ws.close();
  });
});
