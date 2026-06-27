import { defineWorkersConfig } from "@cloudflare/vitest-pool-workers/config";

/**
 * Runs tests inside the actual workerd runtime via @cloudflare/vitest-pool-workers,
 * so Durable Objects, KV, WebSockets, and WebCrypto Ed25519 all behave like prod.
 * We point the pool at wrangler.toml so the WEBHOOK DO and REGISTRY KV bindings
 * are available to tests.
 */
export default defineWorkersConfig({
  test: {
    poolOptions: {
      workers: {
        // Run all test files in one worker without per-test isolated storage.
        // The hibernatable-WebSocket DO keeps live sockets across `it` blocks,
        // which is incompatible with the isolated-storage stacking teardown
        // (it asserts on the SQLite shadow files). State is namespaced by a
        // fresh webhookId per test, so cross-test leakage is not a concern.
        singleWorker: true,
        isolatedStorage: false,
        // Bindings (the WEBHOOK DO and PULLPILOT_REGISTRY KV namespace) are
        // derived from wrangler.toml; the pool provisions a local in-memory KV
        // automatically, so the production KV id placeholder is irrelevant here.
        wrangler: { configPath: "./wrangler.toml" },
      },
    },
  },
});
