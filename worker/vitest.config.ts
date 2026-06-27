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
        wrangler: { configPath: "./wrangler.toml" },
        miniflare: {
          // Provide a local KV namespace for REGISTRY (the toml id is a
          // production placeholder; tests use an in-memory namespace).
          kvNamespaces: ["REGISTRY"],
        },
      },
    },
  },
});
