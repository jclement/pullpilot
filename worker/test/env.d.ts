/**
 * Type augmentation for tests.
 *
 * - Pulls in the `cloudflare:test` module declarations shipped by
 *   @cloudflare/vitest-pool-workers (provides `env`, `SELF`, `runInDurableObject`,
 *   etc.).
 * - Extends `ProvidedEnv` so `env` is typed with our Worker's bindings.
 */
/// <reference types="@cloudflare/vitest-pool-workers" />
import type { Env } from "../src/index";

declare module "cloudflare:test" {
  interface ProvidedEnv extends Env {}
}
