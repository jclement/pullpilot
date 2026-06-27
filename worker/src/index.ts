/**
 * PullPilot — Webhook Relay (Cloudflare Worker)
 * ============================================
 *
 * A content-free "poke" relay for self-hosted PullPilot daemons that live
 * behind NAT. Daemons hold a WebSocket open to the relay; external systems
 * POST to a public URL to trigger a poke that the daemon receives. The relay
 * is UNTRUSTED for content — a poke carries no actionable data, only a signal
 * to "go check your source of truth".
 *
 * This file contains:
 *   - The Worker `fetch` handler and request routing.
 *   - KV-backed registry helpers (provision / lookup / TTL refresh).
 *   - A best-effort per-IP rate limiter for `/v1/provision`.
 *   - Re-export of the `WebhookRelay` Durable Object (defined in ./relay.ts),
 *     which owns the WebSocket hibernation handshake and poke delivery.
 *
 * Protocol details live in PROTOCOL constants below and in ./relay.ts. The Go
 * daemon is built against this exact contract — do not change it casually.
 */

import { WebhookRelay } from "./relay";
import { b64urlDecode, b64urlEncode } from "./relay";

// Re-export the Durable Object class so Wrangler can find it via the binding.
export { WebhookRelay };

export interface Env {
  /** Durable Object namespace; one DO instance per webhookId (idFromName). */
  WEBHOOK: DurableObjectNamespace<WebhookRelay>;
  /** KV registry of provisioned webhooks. Keys: `wh:<webhookId>`. */
  REGISTRY: KVNamespace;
  /** Reported by `/version`. */
  WORKER_VERSION?: string;
  /** KV expiration TTL (seconds). Default 6 months. Refreshed on activity. */
  PRUNE_AFTER?: string;
}

/** Default prune horizon: 6 months in seconds. */
const DEFAULT_PRUNE_AFTER = 15_552_000;

/** Maximum accepted ed25519 public key size (raw bytes). Guards oversized input. */
const ED25519_PUBKEY_BYTES = 32;

/** Maximum bytes we will read from a provision request body. */
const MAX_PROVISION_BODY = 4096;

/** Per-IP provision rate limit: max requests within the window. */
const PROVISION_RATE_LIMIT = 10;
/** Per-IP provision rate limit window, in seconds. */
const PROVISION_RATE_WINDOW = 60;

/** Shape of a registry record stored in KV under `wh:<webhookId>`. */
export interface WebhookRecord {
  /** base64url (unpadded) raw ed25519 public key, 32 bytes. */
  pubkey: string;
  /** Optional human label supplied at provision time. */
  label: string | null;
  /** ISO8601 creation timestamp. */
  createdAt: string;
  /** ISO8601 of the most recent successful connect/auth/poke. */
  lastSeen: string;
}

/** Resolve the effective prune TTL (seconds) from env, with a safe default. */
export function pruneAfterSeconds(env: Env): number {
  const parsed = Number.parseInt(env.PRUNE_AFTER ?? "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_PRUNE_AFTER;
}

/** Build the KV key for a webhook record. */
export function registryKey(webhookId: string): string {
  return `wh:${webhookId}`;
}

/** Read and parse a webhook record from KV, or null if missing/expired/corrupt. */
export async function getWebhookRecord(
  env: Env,
  webhookId: string,
): Promise<WebhookRecord | null> {
  const raw = await env.REGISTRY.get(registryKey(webhookId));
  if (raw === null) return null;
  try {
    return JSON.parse(raw) as WebhookRecord;
  } catch {
    return null;
  }
}

/**
 * Refresh a webhook's KV TTL and `lastSeen`. Called on successful connect,
 * auth, and poke so that active webhooks never expire while in use. This is a
 * read-modify-write; concurrent writers may race but the only field that
 * changes is `lastSeen`, so last-writer-wins is acceptable.
 */
export async function touchWebhook(env: Env, webhookId: string): Promise<void> {
  const record = await getWebhookRecord(env, webhookId);
  if (!record) return;
  record.lastSeen = new Date().toISOString();
  await env.REGISTRY.put(registryKey(webhookId), JSON.stringify(record), {
    expirationTtl: pruneAfterSeconds(env),
  });
}

/** Generate a webhookId: `wh_` followed by 32 random bytes in lowercase hex. */
function generateWebhookId(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  let hex = "";
  for (const b of bytes) hex += b.toString(16).padStart(2, "0");
  return `wh_${hex}`;
}

/** JSON response helper with sane defaults. */
function json(body: unknown, status = 200, headers: HeadersInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

/** Extract the best-effort client IP for rate limiting. */
function clientIp(request: Request): string {
  return (
    request.headers.get("cf-connecting-ip") ??
    request.headers.get("x-forwarded-for")?.split(",")[0]?.trim() ??
    "unknown"
  );
}

/**
 * Best-effort per-IP rate limiter backed by KV counters. KV is eventually
 * consistent, so this is a soft limit (good enough to blunt abuse without a
 * dedicated DO). Returns true if the caller is over the limit.
 */
async function isRateLimited(env: Env, ip: string): Promise<boolean> {
  const key = `rl:provision:${ip}`;
  const current = Number.parseInt((await env.REGISTRY.get(key)) ?? "0", 10);
  if (current >= PROVISION_RATE_LIMIT) return true;
  // Increment with a fresh window TTL. Resetting the TTL on every request makes
  // this a sliding window approximation; acceptable for abuse mitigation.
  await env.REGISTRY.put(key, String(current + 1), {
    expirationTtl: PROVISION_RATE_WINDOW,
  });
  return false;
}

/** Compute the public origin (`https://host` / `wss://host`) from the request. */
function origins(request: Request): { http: string; ws: string } {
  const url = new URL(request.url);
  const httpScheme = url.protocol === "http:" ? "http" : "https";
  const wsScheme = url.protocol === "http:" ? "ws" : "wss";
  return {
    http: `${httpScheme}://${url.host}`,
    ws: `${wsScheme}://${url.host}`,
  };
}

/**
 * POST /v1/provision — register a new webhook for an ed25519 public key.
 */
async function handleProvision(request: Request, env: Env): Promise<Response> {
  if (request.method !== "POST") {
    return json({ error: "method_not_allowed" }, 405);
  }

  // Per-IP rate limit to deter mass provisioning.
  const ip = clientIp(request);
  if (await isRateLimited(env, ip)) {
    return json({ error: "rate_limited" }, 429);
  }

  // Bounded body read to avoid memory abuse.
  const text = await request.text();
  if (text.length > MAX_PROVISION_BODY) {
    return json({ error: "body_too_large" }, 400);
  }

  let body: { pubkey?: unknown; label?: unknown };
  try {
    body = JSON.parse(text);
  } catch {
    return json({ error: "invalid_json" }, 400);
  }

  if (typeof body.pubkey !== "string" || body.pubkey.length === 0) {
    return json({ error: "missing_pubkey" }, 400);
  }

  // Validate the pubkey decodes to exactly 32 raw bytes.
  let pubkeyBytes: Uint8Array;
  try {
    pubkeyBytes = b64urlDecode(body.pubkey);
  } catch {
    return json({ error: "invalid_pubkey_encoding" }, 400);
  }
  if (pubkeyBytes.length !== ED25519_PUBKEY_BYTES) {
    return json({ error: "invalid_pubkey_length" }, 400);
  }

  // Sanity-check the key is actually importable as ed25519.
  try {
    await crypto.subtle.importKey(
      "raw",
      pubkeyBytes as BufferSource,
      { name: "Ed25519" },
      false,
      ["verify"],
    );
  } catch {
    return json({ error: "invalid_pubkey" }, 400);
  }

  const label =
    typeof body.label === "string" ? body.label.slice(0, 256) : null;

  const webhookId = generateWebhookId();
  const now = new Date().toISOString();
  const record: WebhookRecord = {
    // Store re-encoded canonical (unpadded) base64url to normalize input.
    pubkey: b64urlEncode(pubkeyBytes),
    label,
    createdAt: now,
    lastSeen: now,
  };

  const ttl = pruneAfterSeconds(env);
  await env.REGISTRY.put(registryKey(webhookId), JSON.stringify(record), {
    expirationTtl: ttl,
  });

  const { http, ws } = origins(request);
  return json(
    {
      webhookId,
      pokeUrl: `${http}/v1/poke/${webhookId}`,
      listenUrl: `${ws}/v1/listen/${webhookId}`,
      ttlDays: Math.floor(ttl / 86400),
    },
    201,
  );
}

/**
 * GET /v1/listen/<id> — upgrade to a WebSocket and hand off to the DO.
 * The DO drives the auth handshake and poke delivery with hibernation.
 */
async function handleListen(
  request: Request,
  env: Env,
  webhookId: string,
): Promise<Response> {
  if (request.headers.get("Upgrade") !== "websocket") {
    return new Response("expected websocket upgrade", { status: 426 });
  }

  // Reject unknown/expired webhooks before spinning up a DO.
  const record = await getWebhookRecord(env, webhookId);
  if (!record) {
    return new Response("unknown webhook", { status: 404 });
  }

  const id = env.WEBHOOK.idFromName(webhookId);
  const stub = env.WEBHOOK.get(id);
  // Forward to the DO; it performs the WebSocket upgrade with hibernation.
  return stub.fetch(request);
}

/**
 * POST|GET /v1/poke/<id> — trigger a content-free poke. No auth.
 */
async function handlePoke(
  request: Request,
  env: Env,
  webhookId: string,
): Promise<Response> {
  if (request.method !== "POST" && request.method !== "GET") {
    return json({ error: "method_not_allowed" }, 405);
  }

  const record = await getWebhookRecord(env, webhookId);
  if (!record) {
    return new Response("unknown webhook", { status: 404 });
  }

  // Extract an optional `reason` from a JSON body (POST only). The relay never
  // interprets this value; it is passed through opaquely to the daemon.
  let reason: string | null = null;
  if (request.method === "POST") {
    const ct = request.headers.get("content-type") ?? "";
    if (ct.includes("application/json")) {
      try {
        const text = await request.text();
        if (text.length <= MAX_PROVISION_BODY) {
          const parsed = JSON.parse(text) as { reason?: unknown };
          if (typeof parsed.reason === "string") {
            reason = parsed.reason.slice(0, 256);
          }
        }
      } catch {
        // Malformed body is non-fatal; a poke is content-free by design.
        reason = null;
      }
    }
  }

  const id = env.WEBHOOK.idFromName(webhookId);
  const stub = env.WEBHOOK.get(id);
  return stub.fetch(
    new Request(request.url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ reason }),
    }),
  );
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const path = url.pathname;

    // --- Liveness / metadata ------------------------------------------------
    if (path === "/healthz") {
      return new Response("ok", {
        status: 200,
        headers: { "content-type": "text/plain" },
      });
    }
    if (path === "/version") {
      return json({ version: env.WORKER_VERSION ?? "dev" });
    }

    // --- Provision ----------------------------------------------------------
    if (path === "/v1/provision") {
      return handleProvision(request, env);
    }

    // --- Listen (WebSocket) -------------------------------------------------
    const listenMatch = path.match(/^\/v1\/listen\/([^/]+)$/);
    if (listenMatch) {
      return handleListen(request, env, listenMatch[1]);
    }

    // --- Poke ---------------------------------------------------------------
    const pokeMatch = path.match(/^\/v1\/poke\/([^/]+)$/);
    if (pokeMatch) {
      return handlePoke(request, env, pokeMatch[1]);
    }

    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
