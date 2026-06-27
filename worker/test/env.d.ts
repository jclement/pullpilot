/**
 * Type augmentation for tests.
 *
 * - Pulls in the `cloudflare:test` module declarations shipped by
 *   @cloudflare/vitest-pool-workers (provides `env`, `SELF`, `runInDurableObject`,
 *   etc.).
 * - Extends the global `Cloudflare.Env` so `env` is typed with our Worker's
 *   bindings. As of vitest-pool-workers 0.16 (vitest 4), `cloudflare:test`
 *   types `env` as `Cloudflare.Env` rather than an augmentable `ProvidedEnv`.
 */
/// <reference types="@cloudflare/vitest-pool-workers/types" />
import type { Env as WorkerEnv } from "../src/index";

declare global {
  namespace Cloudflare {
    interface Env extends WorkerEnv {}
  }
}

export {};
