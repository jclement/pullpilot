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

## `socket-proxy/` — hardened, no raw socket

A defense-in-depth variant: PullPilot runs **non-root + read-only** and reaches
Docker over TCP through a [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy)
that only exposes the endpoints it needs, so the daemon never holds the raw
socket.

```bash
docker compose -f samples/socket-proxy/docker-compose.yml up -d
```

Note this is **not** a hard security boundary — recreating containers needs
`POST`, which is enough to escalate to host root regardless. It limits incidental
API surface; **rootless Docker** is the real mitigation. See the main README's
Security section.
