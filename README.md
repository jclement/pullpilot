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
      PP_TIMEZONE: America/Edmonton
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - pullpilot-data:/data
    user: "0:0"                       # root, to access the docker socket
    restart: unless-stopped
volumes:
  pullpilot-data:
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
| Pull-before-stop (no downtime on failed pull) | ✅ | ✅ |
| Self-heals an update interrupted by a reboot | ❌ | ✅ |

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
   into your stack — it mounts the Docker socket and adds cheap container
   hardening (read-only rootfs, dropped capabilities, no-new-privileges).
2. Adjust `PP_TIMEZONE` and bring it up:

   ```bash
   docker compose up -d
   ```

With zero extra config, PullPilot polls **its own compose project nightly at
03:00 local time**, soaks new images for 24h, health-gates and rolls back
failures, and touches nothing else. Webhook, image cleanup, and self-update are
all **off** until you opt in.

> Want to watch it work first? [`samples/playground/`](samples/playground/) is a
> safe debug + dry-run stack with the webhook on — `docker compose -f
> samples/playground/docker-compose.yml up`.

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
| `PP_SCOPE` | `project` | `project` \| `all` \| `project:<name>`. ⚠️ `all` manages every non-excluded container on the host. |
| `PP_SOAK` | `24h` | Soak window before a new digest rolls out (bare integer = seconds). |
| `PP_SELF_UPDATE` | `false` | Notify when a newer PullPilot image is available. (In-place self-update isn't supported yet — it would kill the daemon mid-update — so this is notify-only for now.) |
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
your CI / registry webhook at it — **either `POST` or `GET`** works, so even tools
that only do a plain `GET` (registries, `wget`/`curl` in cron) can trigger it.

Because a poke is non-authoritative (it can never name an image or skip a gate),
the worst anything can do by hitting the URL is cause one extra, rate-limited
check — so `GET` is safe. Still, treat the poke URL as a **write-capable secret**:
don't paste it anywhere that auto-fetches links (a chat that unfurls URLs, an HTML
`<img src>`). Abandoned webhooks are auto-pruned after 6 months of inactivity.

### Self-hosting the relay

The relay holds **no shared secret** — your per-webhook ed25519 keypair is the
trust root. Deploy your own:

```bash
cd worker
npm install
wrangler kv namespace create PULLPILOT_REGISTRY        # paste id into wrangler.toml
wrangler kv namespace create PULLPILOT_REGISTRY_TEST   # paste test id into wrangler.toml
wrangler deploy --env production
```

Then set `PP_WEBHOOK_URL: https://pullpilot-relay.<your-subdomain>.workers.dev`.

---

## Security

**Be clear-eyed about the Docker socket.** PullPilot has to talk to the Docker
API to recreate containers, and *anything* that can create a container can create
a privileged one that mounts the host's root filesystem — i.e. **socket access is
root-equivalent on the host.** This is the same risk profile as Watchtower and is
inherent to auto-updating containers; no amount of wrapping removes it. The one
control that genuinely changes the equation is **rootless Docker** — run it that
way on sensitive hosts and "create a privileged container" no longer means host
root. (A socket proxy that only allows certain endpoints is *not* a meaningful
fix here: PullPilot needs container-create, which is already enough to escalate.)

What PullPilot *does* do well:

- **No inbound ports.** The daemon never listens; the webhook-relay design exists
  precisely so you don't expose anything. The only way in is compromising the
  PullPilot image/process itself — so keep it pinned and trusted.
- **The realistic threat is a bad upstream image, not the socket** — handled by
  digest pinning, the **soak window** (a broken/malicious `:latest` is caught
  upstream before it reaches you), and **health-gated rollback**.
- **The relay is untrusted by design** — pokes are non-authoritative (they can
  never name an image or skip a gate), the listen connection is ed25519
  challenge-response authenticated, pokes are rate-limited, and a mandatory cron
  floor means a hostile relay can't even suppress updates.
- **Secrets** — the ed25519 key is written `0600` in a `0700` dir; secrets are
  never logged and webhook URLs are redacted.
- **Cheap container hardening** in the example (read-only rootfs, `cap_drop: ALL`,
  `no-new-privileges`) limits in-container surface without adding complexity.

Default scope is **your own compose project** — PullPilot won't touch anything
else unless you opt into `PP_SCOPE=all`.

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
