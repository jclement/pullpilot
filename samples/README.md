# Samples

## `playground/` — watch PullPilot work

A safe, self-contained stack for kicking the tires. It runs with **debug logging**
and **dry-run on** (so nothing is actually restarted), polls every 2 minutes, and
turns the **webhook on with no persistent volume** so you can see it provision a
relay webhook (and the persistence warning) live.

```bash
docker compose -f samples/playground/docker-compose.yml up
```

Then:

1. Watch the startup logs — you'll see the config summary, the
   `data dir is NOT a persistent mount` warning, each container being checked,
   and a `relay connected  poke_url=…` line.
2. Copy that **poke URL** and trigger an instant check:
   ```bash
   curl -X POST '<poke URL from the logs>'
   ```
   You'll see the debounced cycle fire ~10s later.
3. Tear it down with `docker compose -f samples/playground/docker-compose.yml down`.

To make it actually apply updates, drop `PP_DRY_RUN` and set a short
`PP_SOAK` (e.g. `PP_SOAK: 1m`). See the main [README](../README.md) for all
options and the production-style example in
[`deploy/`](../deploy/docker-compose.example.yml).
