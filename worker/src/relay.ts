/**
 * WebhookRelay — the per-webhook Durable Object.
 * ==============================================
 *
 * One DO instance exists per `webhookId` (routed via `idFromName(webhookId)`).
 * It owns:
 *
 *   - The WebSocket hibernation lifecycle for daemon listeners.
 *   - The challenge/response auth handshake (ed25519 over a fixed domain).
 *   - Poke delivery to authed sockets, with 5s coalescing.
 *   - A single coalesced "pending poke" flag for when no listener is connected.
 *   - A weekly alarm that refreshes KV TTL while a listener is authed, plus a
 *     short-fused (30s) variant that reaps never-authed sockets. Opening a
 *     socket lowers the alarm to fire within the reap window, and a small cap
 *     bounds concurrent unauthed sockets so a known webhookId can't be used to
 *     hold open unbounded connections that never authenticate.
 *
 * Storage uses the SQLite-backed Durable Object storage API (configured via
 * `new_sqlite_classes` in wrangler.toml). We use the simple key/value methods
 * (`storage.get` / `storage.put`) which are backed by SQLite.
 *
 * Hibernation note: because the DO can be evicted from memory between events,
 * all per-socket state (challenge bytes, expiry, authed flag) is persisted in
 * the socket's `serializeAttachment`, and durable flags (pending poke) live in
 * DO storage. The constructor must stay cheap.
 */

import { DurableObject } from "cloudflare:workers";
import type { Env, WebhookRecord } from "./index";

// ---------------------------------------------------------------------------
// Protocol constants — MUST match the Go daemon exactly.
// ---------------------------------------------------------------------------

/** Signature domain separation tag. Hashed/signed as raw ASCII bytes. */
export const DOMAIN = "pullpilot-webhook-relay/v1";

/** Seconds a hello-challenge remains valid before auth must complete. */
const CHALLENGE_TTL_SECONDS = 30;

/** Max simultaneously-authed sockets per webhook. Extras get close 1013. */
const MAX_AUTHED_SOCKETS = 4;

/**
 * Max simultaneously-unauthed (handshake-pending) sockets per webhook. Extras
 * get close 1013. Caps DoS via unbounded never-authenticating connections that
 * would otherwise persist until the reaper alarm fires.
 */
const MAX_UNAUTHED_SOCKETS = 8;

/** Coalesce pokes arriving within this many milliseconds into one delivery. */
const POKE_COALESCE_MS = 5_000;

/** Abuse guard: reject pokes beyond this many within the coalesce window. */
const POKE_MAX_PER_WINDOW = 100;

/** Reap never-authed sockets older than this (ms) on alarm. */
const UNAUTHED_MAX_AGE_MS = 30_000;

/** Alarm cadence: roughly weekly. Refreshes KV TTL for active webhooks. */
const ALARM_INTERVAL_MS = 7 * 24 * 60 * 60 * 1000;

/** WebSocket close codes used by this protocol. */
const CLOSE_AUTH_FAILED = 1008; // policy violation: bad/expired auth
const CLOSE_TOO_MANY = 1013; // try again later: socket cap reached

// DO storage keys.
const KEY_PENDING = "pending"; // single coalesced pending-poke flag ("1" | absent)
const KEY_LAST_POKE_AT = "lastPokeAt"; // ms epoch of last *delivered* poke (coalescing)
const KEY_POKE_WINDOW_START = "pokeWindowStart"; // ms epoch (abuse counter window)
const KEY_POKE_WINDOW_COUNT = "pokeWindowCount"; // count within current window

/** Per-socket state persisted across hibernation via serializeAttachment. */
interface SocketAttachment {
  /** base64url of the raw 32 challenge bytes issued in `hello`. */
  challenge: string;
  /** Unix seconds after which the challenge is invalid. */
  exp: number;
  /** Whether this socket has completed the auth handshake. */
  authed: boolean;
  /** ms epoch when the socket was opened (used to reap stale unauthed sockets). */
  openedAt: number;
}

// ---------------------------------------------------------------------------
// base64url (unpadded) helpers — shared with the Worker entrypoint.
// ---------------------------------------------------------------------------

/** Encode bytes as base64url without padding. */
export function b64urlEncode(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Decode base64url (padded or unpadded) to bytes. Throws on invalid input. */
export function b64urlDecode(s: string): Uint8Array {
  const norm = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = norm.length % 4 === 0 ? "" : "=".repeat(4 - (norm.length % 4));
  const bin = atob(norm + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Concatenate Uint8Arrays into a single buffer. */
function concatBytes(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/** Generate `n` cryptographically-random bytes. */
function randomBytes(n: number): Uint8Array {
  const b = new Uint8Array(n);
  crypto.getRandomValues(b);
  return b;
}

/** Lowercase hex of random bytes (poke ids). */
function randomHex(n: number): string {
  let hex = "";
  for (const b of randomBytes(n)) hex += b.toString(16).padStart(2, "0");
  return hex;
}

export class WebhookRelay extends DurableObject<Env> {
  /** Cached webhookId derived from the incoming request path (per activation). */
  private webhookId: string | null = null;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // Keep the constructor cheap — it runs on every wake from hibernation.
    // Register the auto-response so `ping` text frames are answered `pong`
    // by the runtime even while hibernated (no DO wake, no billing).
    this.ctx.setWebSocketAutoResponse(
      new WebSocketRequestResponsePair("ping", "pong"),
    );
  }

  /**
   * Entry point for both the WebSocket upgrade (`/v1/listen/...`) and the
   * internal poke fetch (`/v1/poke/...`, rewritten by the Worker). We
   * distinguish the two by the presence of the Upgrade header.
   */
  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    // Recover the webhookId from the path for both listen and poke routes.
    const match = url.pathname.match(/^\/v1\/(?:listen|poke)\/([^/]+)$/);
    this.webhookId = match ? match[1] : null;

    if (request.headers.get("Upgrade") === "websocket") {
      return await this.handleUpgrade();
    }
    return this.handlePoke(request);
  }

  // -------------------------------------------------------------------------
  // WebSocket upgrade + handshake
  // -------------------------------------------------------------------------

  /** Accept a hibernatable WebSocket and send the `hello` challenge. */
  private async handleUpgrade(): Promise<Response> {
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);

    // Cap concurrent unauthed (handshake-pending) sockets. Without this an
    // attacker who knows a webhookId could open unbounded connections that
    // never authenticate and would only be reaped by the next alarm. We accept
    // the socket so we can send a close frame, then close it with 1013.
    const unauthedCount = this.ctx
      .getWebSockets()
      .filter((s) => {
        const a = s.deserializeAttachment() as SocketAttachment | null;
        return a?.authed !== true;
      }).length;
    if (unauthedCount >= MAX_UNAUTHED_SOCKETS) {
      this.ctx.acceptWebSocket(server);
      server.close(CLOSE_TOO_MANY, "too many connections");
      return new Response(null, { status: 101, webSocket: client });
    }

    // Accept with hibernation. The runtime keeps the client connected while
    // this DO sleeps; webSocketMessage/Close re-wake it as needed.
    this.ctx.acceptWebSocket(server);

    // Issue a fresh challenge and persist it on the socket so it survives
    // hibernation between the hello and the auth reply.
    const challenge = randomBytes(32);
    const exp = Math.floor(Date.now() / 1000) + CHALLENGE_TTL_SECONDS;
    const attachment: SocketAttachment = {
      challenge: b64urlEncode(challenge),
      exp,
      authed: false,
      openedAt: Date.now(),
    };
    server.serializeAttachment(attachment);

    server.send(
      JSON.stringify({
        type: "hello",
        v: 1,
        challenge: b64urlEncode(challenge),
        exp,
      }),
    );

    // Schedule the reaper alarm to fire soon so this never-authed socket gets
    // cleaned up if the handshake doesn't complete. We lower (never raise) any
    // existing alarm so the weekly KV-refresh cadence doesn't delay reaping.
    await this.scheduleReaperAlarm();

    return new Response(null, { status: 101, webSocket: client });
  }

  /** Handle inbound text frames: only `auth` is meaningful (ping is auto-handled). */
  override async webSocketMessage(
    ws: WebSocket,
    message: ArrayBuffer | string,
  ): Promise<void> {
    if (typeof message !== "string") {
      // Binary frames are not part of the protocol; ignore.
      return;
    }

    let msg: { type?: unknown; sig?: unknown };
    try {
      msg = JSON.parse(message);
    } catch {
      return; // ignore malformed frames
    }

    if (msg.type === "auth" && typeof msg.sig === "string") {
      await this.handleAuth(ws, msg.sig);
    }
    // Any other message type (including stray "ping" not caught by
    // auto-response) is ignored.
  }

  /** Verify the client's ed25519 signature over DOMAIN || webhookId || challenge. */
  private async handleAuth(ws: WebSocket, sigB64: string): Promise<void> {
    const att = ws.deserializeAttachment() as SocketAttachment | null;
    if (!att) {
      ws.close(CLOSE_AUTH_FAILED, "no challenge");
      return;
    }

    // Already authed? Idempotent no-op (a re-auth shouldn't reset state).
    if (att.authed) return;

    // Reject expired challenges.
    if (Math.floor(Date.now() / 1000) > att.exp) {
      ws.close(CLOSE_AUTH_FAILED, "challenge expired");
      return;
    }

    const webhookId = this.webhookId;
    if (!webhookId) {
      ws.close(CLOSE_AUTH_FAILED, "unknown webhook");
      return;
    }

    // Enforce the authed-socket cap (counts only *other* already-authed sockets).
    const authedCount = this.ctx
      .getWebSockets()
      .filter((s) => {
        if (s === ws) return false;
        const a = s.deserializeAttachment() as SocketAttachment | null;
        return a?.authed === true;
      }).length;
    if (authedCount >= MAX_AUTHED_SOCKETS) {
      ws.close(CLOSE_TOO_MANY, "too many connections");
      return;
    }

    // Look up the pubkey from KV (DO has no copy of its own).
    const record = await this.getRecord(webhookId);
    if (!record) {
      ws.close(CLOSE_AUTH_FAILED, "unknown webhook");
      return;
    }

    let ok = false;
    try {
      const pubkeyBytes = b64urlDecode(record.pubkey);
      const key = await crypto.subtle.importKey(
        "raw",
        pubkeyBytes as BufferSource,
        { name: "Ed25519" },
        false,
        ["verify"],
      );
      const challengeBytes = b64urlDecode(att.challenge);
      const message = concatBytes(
        new TextEncoder().encode(DOMAIN),
        new TextEncoder().encode(webhookId),
        challengeBytes,
      );
      const sig = b64urlDecode(sigB64);
      ok = await crypto.subtle.verify(
        { name: "Ed25519" },
        key,
        sig as BufferSource,
        message as BufferSource,
      );
    } catch {
      ok = false;
    }

    if (!ok) {
      ws.close(CLOSE_AUTH_FAILED, "bad signature");
      return;
    }

    // Success: mark authed, notify, flush any pending poke, refresh KV.
    att.authed = true;
    ws.serializeAttachment(att);
    ws.send(JSON.stringify({ type: "ready" }));

    await this.touchRecord(webhookId);
    await this.flushPending();
  }

  /** Clean up handled implicitly; nothing extra to do on close for our state. */
  override async webSocketClose(
    ws: WebSocket,
    code: number,
    reason: string,
  ): Promise<void> {
    // With hibernation the runtime manages socket lifecycle; we keep no
    // in-memory registry. Closing our side is harmless and explicit.
    try {
      ws.close(code, reason);
    } catch {
      // already closing/closed
    }
  }

  override async webSocketError(_ws: WebSocket, _error: unknown): Promise<void> {
    // No-op: errored sockets are dropped by the runtime; pending-poke state
    // (if any) remains durable and is delivered to the next authed socket.
  }

  // -------------------------------------------------------------------------
  // Poke delivery
  // -------------------------------------------------------------------------

  /**
   * Handle a poke (already normalized by the Worker to `{ reason }`).
   * Applies abuse limiting and 5s coalescing, then delivers to all authed
   * sockets, or stores a single pending flag if none are connected.
   */
  private async handlePoke(request: Request): Promise<Response> {
    const webhookId = this.webhookId;
    if (!webhookId) {
      return new Response("unknown webhook", { status: 404 });
    }

    let reason: string | null = null;
    try {
      const parsed = (await request.json()) as { reason?: unknown };
      if (typeof parsed.reason === "string") reason = parsed.reason;
    } catch {
      reason = null;
    }

    const now = Date.now();

    // --- Abuse guard: cap pokes per coalesce window -----------------------
    const windowStart = (await this.ctx.storage.get<number>(KEY_POKE_WINDOW_START)) ?? 0;
    let windowCount = (await this.ctx.storage.get<number>(KEY_POKE_WINDOW_COUNT)) ?? 0;
    if (now - windowStart > POKE_COALESCE_MS) {
      // New window.
      await this.ctx.storage.put(KEY_POKE_WINDOW_START, now);
      windowCount = 0;
    }
    if (windowCount >= POKE_MAX_PER_WINDOW) {
      return new Response(JSON.stringify({ error: "rate_limited" }), {
        status: 429,
        headers: { "content-type": "application/json" },
      });
    }
    await this.ctx.storage.put(KEY_POKE_WINDOW_COUNT, windowCount + 1);

    // Refresh KV TTL + lastSeen on every accepted poke.
    await this.touchRecord(webhookId);

    // --- Coalescing: collapse pokes within POKE_COALESCE_MS into one ------
    const lastPokeAt = (await this.ctx.storage.get<number>(KEY_LAST_POKE_AT)) ?? 0;
    const withinCoalesce = now - lastPokeAt < POKE_COALESCE_MS;

    const authedSockets = this.ctx
      .getWebSockets()
      .filter((s) => {
        const a = s.deserializeAttachment() as SocketAttachment | null;
        return a?.authed === true;
      });

    if (authedSockets.length === 0) {
      // No listener: store a SINGLE coalesced pending flag (not a queue).
      await this.ctx.storage.put(KEY_PENDING, "1");
      return this.accepted();
    }

    if (withinCoalesce) {
      // A poke was just delivered; coalesce this one (no duplicate delivery).
      // The daemon already knows to re-check; suppressing avoids thundering.
      return this.accepted();
    }

    // Deliver now.
    await this.ctx.storage.put(KEY_LAST_POKE_AT, now);
    this.deliverPoke(authedSockets, reason);
    return this.accepted();
  }

  /** Build the poke frame and send it to the given sockets. */
  private deliverPoke(sockets: WebSocket[], reason: string | null): void {
    const frame = JSON.stringify({
      type: "poke",
      v: 1,
      id: randomHex(16),
      ts: new Date().toISOString(),
      reason,
    });
    for (const s of sockets) {
      try {
        s.send(frame);
      } catch {
        // Socket may be mid-close; ignore.
      }
    }
  }

  /**
   * Flush a single pending poke (if present) to all currently-authed sockets,
   * then clear the flag. Called right after a successful auth.
   */
  private async flushPending(): Promise<void> {
    const pending = await this.ctx.storage.get<string>(KEY_PENDING);
    if (pending !== "1") return;

    const authedSockets = this.ctx
      .getWebSockets()
      .filter((s) => {
        const a = s.deserializeAttachment() as SocketAttachment | null;
        return a?.authed === true;
      });
    if (authedSockets.length === 0) return;

    await this.ctx.storage.delete(KEY_PENDING);
    await this.ctx.storage.put(KEY_LAST_POKE_AT, Date.now());
    // Pending pokes carry no reason (the original was content-free / dropped).
    this.deliverPoke(authedSockets, null);
  }

  private accepted(): Response {
    return new Response(JSON.stringify({ status: "accepted" }), {
      status: 202,
      headers: { "content-type": "application/json" },
    });
  }

  // -------------------------------------------------------------------------
  // Alarm: weekly KV TTL refresh + stale unauthed-socket reaper
  // -------------------------------------------------------------------------

  /**
   * Schedule the reaper alarm to fire soon (within UNAUTHED_MAX_AGE_MS) so a
   * freshly-opened unauthed socket gets reaped if it never authenticates. We
   * only ever LOWER an existing alarm: if the weekly KV-refresh alarm (or an
   * earlier reaper alarm) is already pending sooner, we leave it untouched.
   */
  private async scheduleReaperAlarm(): Promise<void> {
    const target = Date.now() + UNAUTHED_MAX_AGE_MS;
    const existing = await this.ctx.storage.getAlarm();
    if (existing === null || target < existing) {
      await this.ctx.storage.setAlarm(target);
    }
  }

  override async alarm(): Promise<void> {
    const now = Date.now();
    const sockets = this.ctx.getWebSockets();

    let hasAuthed = false;
    let hasUnauthed = false;
    for (const s of sockets) {
      const a = s.deserializeAttachment() as SocketAttachment | null;
      if (!a) continue;
      if (a.authed) {
        hasAuthed = true;
      } else if (now - a.openedAt > UNAUTHED_MAX_AGE_MS) {
        // Reap sockets that connected but never completed auth.
        try {
          s.close(CLOSE_AUTH_FAILED, "auth timeout");
        } catch {
          // already closing
        }
      } else {
        // Still within its grace window — keep the reaper running for it.
        hasUnauthed = true;
      }
    }

    // While an authed listener exists, keep the KV record alive.
    if (hasAuthed && this.webhookId) {
      await this.touchRecord(this.webhookId);
    }

    // Reschedule based on what's left:
    //   - any unauthed socket still pending: reap again soon (30s),
    //   - else any authed socket: keep the weekly KV-refresh cadence,
    //   - else nothing connected: let the DO go idle (no alarm).
    if (hasUnauthed) {
      await this.ctx.storage.setAlarm(now + UNAUTHED_MAX_AGE_MS);
    } else if (hasAuthed) {
      await this.ctx.storage.setAlarm(now + ALARM_INTERVAL_MS);
    }
  }

  // -------------------------------------------------------------------------
  // KV helpers (the DO reads/writes the same registry as the Worker)
  // -------------------------------------------------------------------------

  private async getRecord(webhookId: string): Promise<WebhookRecord | null> {
    const raw = await this.env.PULLPILOT_REGISTRY.get(`wh:${webhookId}`);
    if (raw === null) return null;
    try {
      return JSON.parse(raw) as WebhookRecord;
    } catch {
      return null;
    }
  }

  private async touchRecord(webhookId: string): Promise<void> {
    const record = await this.getRecord(webhookId);
    if (!record) return;
    record.lastSeen = new Date().toISOString();
    const parsed = Number.parseInt(this.env.PRUNE_AFTER ?? "", 10);
    const ttl = Number.isFinite(parsed) && parsed > 0 ? parsed : 15_552_000;
    await this.env.PULLPILOT_REGISTRY.put(`wh:${webhookId}`, JSON.stringify(record), {
      expirationTtl: ttl,
    });
  }
}
