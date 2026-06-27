# PullPilot

**A secure, compose-aware container auto-updater you drop into a Docker Compose stack.**

PullPilot keeps your images current without you babysitting them — and unlike
[Watchtower](https://github.com/containrrr/watchtower), it **soaks new images
before rolling them out** and **rolls back any update that fails its
healthcheck**. Update on a schedule, instantly via webhook, or both.

```yaml
services:
  pullpilot:
    image: ghcr.io/jclement/pullpilot:stable
    environment:
      DOCKER_HOST: tcp://docker-socket-proxy:2375
      PP_TIMEZONE: America/Edmonton
    volumes:
      - pullpilot-data:/data
    # ... see deploy/docker-compose.example.yml for the hardened, full version
```

---

## Why PullPilot

| | Watchtower | PullPilot |
|---|---|---|
| Auto-update containers | ✅ | ✅ |
| Compose-project aware by default | ❌ | ✅ |
| **Soak window** before rollout | ❌ | ✅ (default 24h) |
| **Health-gated automatic rollback** | ❌ | ✅ |
| Instant webhook updates (no inbound port) | ❌ | ✅ (Cloudflare relay) |
| Runs non-root, read-only, socket-proxied | partial | ✅ by design |
| Pull-before-stop (no downtime on failed pull) | ✅ | ✅ |

PullPilot's safety doesn't depend on image signatures (almost no real-world
images are signed). Instead it relies on three controls that work for **every**
image: **digest pinning**, a **soak window**, and **health-gated rollback**.

---

## How it works

On a schedule (and/or on a webhook poke), PullPilot:

1. **Discovers** the containers in its own Compose project (the default scope).
2. **Checks** each image's registry for a newer digest — a cheap manifest `HEAD`,
   no full pull.
3. **Soaks** the new digest: it must remain the current digest for a window
   (default 24h) before rollout, so a broken or malicious `:latest` push gets
   caught upstream first.
4. **Recreates** the container with the new image *only* after a full pull,
   preserving all config (env, mounts, networks, healthcheck, restart policy,
   labels…).
5. **Health-gates** the result and **rolls back to the previous digest** if it
   fails to come up healthy — and never retries that bad digest.

> **Approval, without a UI.** There's no button to babysit: **soak + notifications
> are the approval model.** PullPilot notifies when an image enters soak
> ("rolling out in 24h") and when it rolls out. To hold a service back, pin its
> digest or label it `io.pullpilot.monitor-only=true` (notify, never auto-update).

---

## Quick start

1. Copy [`deploy/docker-compose.example.yml`](deploy/docker-compose.example.yml)
   into your stack (it includes a scoped Docker socket-proxy so PullPilot never
   touches the raw socket).
2. Adjust `PP_TIMEZONE` and bring it up:

   ```bash
   docker compose up -d
   ```

With zero extra config, PullPilot polls **its own compose project nightly at
03:00 local time**, soaks new images for 24h, health-gates and rolls back
failures, and touches nothing else. Webhook, image cleanup, and self-update are
all **off** until you opt in.

---

## Image tags

| Tag | Channel | Default relay | Use for |
|---|---|---|---|
| `:stable` | latest release | production | **recommended** |
| `:vX.Y.Z`, `:X.Y` | pinned release | production | reproducible pins |
| `:latest`, `:edge` | bleeding edge (main) | test | trying unreleased changes |

Images are multi-arch (`linux/amd64`, `linux/arm64`), built on
`distroless/static:nonroot`, with SBOMs attached to releases.

---

## Configuration

Daemon-wide behavior is **environment variables** (`PP_*`) — all visible in your
compose file. Per-container tweaks are **labels** (`io.pullpilot.*`).

### Environment variables

| Var | Default | Meaning |
|---|---|---|
| `PP_SCHEDULE` | `0 3 * * *` | Cron for the baseline poll (nightly 03:00). |
| `PP_TIMEZONE` | host `TZ`, else `UTC` | e.g. `America/Edmonton`. |
| `PP_JITTER` | `30m` | Random delay added to each scheduled run (spreads registry load). |
| `PP_SCOPE` | `project` | `project` \| `all` \| `project:<name>`. |
| `PP_SOAK` | `24h` | Soak window before a new digest rolls out. |
| `PP_SELF_UPDATE` | `false` | Let PullPilot update its own container. |
| `PP_CLEANUP` | `false` | Remove old images after a healthy update. |
| `PP_WEBHOOK` | `false` | Enable instant webhook trigger. |
| `PP_WEBHOOK_URL` | baked default | Relay base URL (point at your own worker). |
| `PP_DATA_DIR` | `/data` | **Persistent** mount: keypair, webhook reg, soak state. |
| `PP_NOTIFY_URL` | – | [shoutrrr](https://containrrr.dev/shoutrrr/) URL (Slack/Discord/email…). |
| `PP_DRY_RUN` | `false` | Plan only, change nothing. |
| `PP_LOG_LEVEL` | `info` | `debug` shows every poll/digest/retry. |
| `PP_LOG_JSON` | `false` | Force JSON logs even on a TTY. |
| `PP_COMPAT_WATCHTOWER` | `false` | Honor `com.centurylinklabs.watchtower.*` labels. |

> ⚠️ **Persist `PP_DATA_DIR`.** PullPilot warns loudly at startup if it isn't a
> real volume. Without persistence, on restart the ed25519 identity regenerates
> (your webhook URL changes, breaking whatever POSTs to it) and soak timers reset.

### Per-container labels

| Label | Meaning |
|---|---|
| `io.pullpilot.enable` | `false` to exclude (or, with `PP_SCOPE` opt-in modes, `true` to include). |
| `io.pullpilot.exclude` | `true` — hard exclude, beats everything. |
| `io.pullpilot.monitor-only` | `true` — detect + notify, never update. |
| `io.pullpilot.soak` | per-container soak (`0` = immediate, `72h` = cautious). |
| `io.pullpilot.self` | mark PullPilot's own container (only needed if `hostname:` is overridden). |
| `io.pullpilot.health-timeout` | seconds/duration to wait for `healthy` before rollback. |
| `io.pullpilot.stop-timeout` | stop grace period. |
| `io.pullpilot.remove-anonymous-volumes` | `true` to allow destroying anon volumes on recreate (default off). |
| `io.pullpilot.order` | integer ordering within a cycle. |

Digest-pinned images (`repo@sha256:…`) are **never** updated — the pin is the
contract.

---

## Instant updates (webhook)

PullPilot can receive an instant "go check now" poke when CI pushes a new image,
**without exposing any inbound port**. A tiny Cloudflare Worker relays the poke
over a held WebSocket.

The poke is **content-free and non-authoritative** — it never names an image. The
daemon always re-derives "is there a newer image?" from the trusted registry and
applies the same soak + health gates. So a malicious relay, or anyone who learns
your poke URL, can at worst cause an extra (rate-limited) check — never a forced
or malicious update. The daemon authenticates its listen connection to the relay
with an ed25519 challenge-response.

Enable it:

```yaml
environment:
  PP_WEBHOOK: "true"
  # PP_WEBHOOK_URL defaults to the public relay; override to self-host.
```

On first start PullPilot provisions a webhook and logs your **poke URL**. Point
your CI / registry webhook at it (`POST` or `GET`). Abandoned webhooks are
auto-pruned after 6 months of inactivity.

### Self-hosting the relay

The relay holds **no shared secret** — your per-webhook ed25519 keypair is the
trust root. Deploy your own:

```bash
cd worker
npm install
wrangler kv namespace create REGISTRY            # paste id into wrangler.toml
wrangler kv namespace create REGISTRY --env test  # paste test id into wrangler.toml
wrangler deploy --env production
```

Then set `PP_WEBHOOK_URL: https://pullpilot-relay.<your-subdomain>.workers.dev`.

---

## Security

PullPilot defends several trust boundaries independently:

- **Relay ↔ daemon** — non-authoritative pokes, ed25519 listen auth, rate limits,
  and a mandatory cron floor mean the relay can never force or fake an update.
- **Registry ↔ daemon** — digest pinning, the soak window (a bad push gets caught
  upstream before it reaches you), and health-gated rollback.
- **Docker socket ↔ daemon** — reach Docker only through a **scoped socket-proxy**
  on an internal network (no host port); run non-root, read-only rootfs,
  `cap_drop: ALL`, `no-new-privileges`. Default scope is your own project.
- **Data dir** — the ed25519 key is written `0600`; secrets are never logged and
  full webhook URLs are redacted.

> **Honest caveat:** any tool that recreates containers inherently has enough
> Docker power to escalate to host root (it can create a privileged container).
> The socket-proxy reduces incidental surface; **rootless Docker** is the real
> mitigation for the host-root problem. Run it that way for sensitive hosts.

---

## Development

PullPilot uses [`mise`](https://mise.jdx.dev) for tooling.

```bash
mise install          # go, node, wrangler
mise run dev:server   # run the Cloudflare worker locally on :8787
mise run dev:client   # run the daemon against the local worker
mise run test         # go test ./... + worker vitest
mise run lint         # go vet + worker tsc
mise run release      # guess next semver, tag, push (triggers the release)
```

Repo layout:

```
cmd/pullpilot/   daemon entrypoint
internal/        engine, registry, webhook, config, state, …
worker/          Cloudflare Worker relay (TypeScript)
deploy/          example docker-compose
.github/         CI + release workflows
```

CI runs tests + govulncheck + trivy on every PR, and on `main` builds the
`:latest`/`:edge` image and deploys the worker to TEST. Tagging `vX.Y.Z` (via
`mise run release`) builds binaries + `:stable` images + SBOMs and deploys the
worker to PRODUCTION. One git tag drives both halves.

> **Maintainer setup:** add repo secrets `CF_API_TOKEN` (Workers Scripts: Edit)
> and `CF_ACCOUNT_ID` to enable worker deploys, and fill the KV namespace ids in
> `worker/wrangler.toml`. Image publishing uses the built-in `GITHUB_TOKEN`.

---

## License

[MIT](LICENSE) © 2026 Jeffrey Clement
