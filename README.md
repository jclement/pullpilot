# PullPilot

**A secure, compose-aware container auto-updater you drop into a Docker Compose stack.**

PullPilot keeps your images current without you babysitting them — and unlike
[Watchtower](https://github.com/containrrr/watchtower), it **soaks new images
before rolling them out** and **rolls back any update that fails its
healthcheck**. Update on a schedule, instantly via webhook, or both.

```yaml
# Minimal — gets you running. For real deployments use the HARDENED version in
# deploy/docker-compose.example.yml (read-only rootfs, dropped caps, no-new-privileges).
services:
  pullpilot:
    image: ghcr.io/jclement/pullpilot:stable   # :stable is the recommended tag
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

> Pin to **`:stable`** (or a specific `:vX.Y.Z`). Avoid `:latest`/`:edge` for real
> deployments — they're bleeding-edge **and point at the *test* webhook relay by
> default** (see [Image tags](#image-tags)).

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

## Commands

PullPilot is a single binary with a few subcommands. The container runs
`serve` by default; the others are handy to run with `docker exec`.

| Command | What it does |
|---|---|
| `pullpilot serve` | Run the daemon: schedule + optional webhook. **Default** — no arguments needed. |
| `pullpilot status` | Print a **read-only** table of every managed container and what PullPilot would do. Changes nothing and does **not** advance soak timers. |
| `pullpilot run` | Run **one** update cycle now and exit. Honors `PP_DRY_RUN`. |
| `pullpilot version` | Print the version. |

**`status`** is the "is it working / what will it do next?" command:

```console
$ docker exec pullpilot pullpilot status
scope: project:media

SERVICE    CURRENT       AVAILABLE     STATE
jellyfin   8f1c2a9b3d4e  8f1c2a9b3d4e  up to date
radarr     a1b2c3d4e5f6  9e8d7c6b5a40  soaking (19h0m left)
sonarr     0f1e2d3c4b5a  7a6b5c4d3e2f  update ready
prowlarr   -             -             pinned by digest
sabnzbd    1a2b3c4d5e6f  2b3c4d5e6f70  update available (monitor-only)
```

The `STATE` column is the per-container verdict: `up to date`, `soaking (Xh left)`,
`update ready`, `pinned by digest`, `update available (monitor-only)`, plus skip
reasons like `no local repo digest (locally-built image?)`,
`digest previously failed health check`, and `registry unreachable`.

**`run`** forces a cycle right now instead of waiting for the schedule. Combine it
with `PP_DRY_RUN=true` to preview a cycle as the same plan table (printed instead
of taking action):

```bash
docker exec pullpilot pullpilot run        # apply now (subject to soak/health gates)
docker exec -e PP_DRY_RUN=true pullpilot pullpilot run   # preview only, change nothing
```

> `status` runs read-only and quiet (warnings only); `serve`/`run` log at
> `PP_LOG_LEVEL`. If the Docker socket isn't reachable, `status`/`run` exit with an
> actionable error and `serve` logs it loudly and retries (see
> [Troubleshooting](#troubleshooting)).

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

Confirm it's working and see what it'll do:

```bash
docker exec pullpilot pullpilot status   # read-only table; advances nothing
```

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

> ⚠️ **Use `:stable` (or a pinned `:X.Y.Z`) for real deployments.** `:latest` and
> `:edge` are bleeding-edge builds from `main` **and they bake the *test* relay as
> their default webhook URL** — fine for experimenting (and what
> [`samples/playground/`](samples/playground/) uses), but you don't want a real
> stack's instant updates depending on the test relay. Either pin to `:stable`, or
> set `PP_WEBHOOK_URL` explicitly.

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
| `PP_LOG_JSON` | `false` | Emit structured JSON instead of the default colored console output (for log shippers). |
| `PP_COMPAT_WATCHTOWER` | `false` | Honor `com.centurylinklabs.watchtower.*` labels (`enable`, `monitor-only`). |

> **Logs are colored, human-readable console output by default** — readable
> straight from `docker logs`. Set `PP_LOG_JSON=true` for structured JSON if you
> ship logs somewhere. The `NO_COLOR` convention is honored (set `NO_COLOR=1` to
> drop ANSI colors while keeping the console format).

> ⚠️ **Persist `PP_DATA_DIR`.** PullPilot warns loudly at startup if it isn't a
> real volume. Without persistence, on restart the ed25519 identity regenerates
> (your webhook URL changes, breaking whatever POSTs to it) and soak timers reset.

### Per-container labels

**There is no opt-in mode.** Every container *in scope* (your compose project by
default, see [`PP_SCOPE`](#environment-variables)) is managed unless you opt it
**out**. The labels below tune or exclude individual containers.

| Label | Meaning |
|---|---|
| `io.pullpilot.exclude` | `true` — **opt out** completely. Hard exclude; beats everything. |
| `io.pullpilot.enable` | `false` — opt this container out (same effect as `exclude`). There is no `true` "opt-in"; leaving it unset already means managed. |
| `io.pullpilot.monitor-only` | `true` — detect + notify on a new image, but **never** update it. |
| `io.pullpilot.soak` | Per-container soak override (`0` = roll out immediately, `72h` = extra-cautious). Bare integer = seconds. |
| `io.pullpilot.self` | Mark PullPilot's own container. Only needed if you override its `hostname:` (see [Troubleshooting](#troubleshooting)). |
| `io.pullpilot.health-timeout` | How long to wait for `healthy` before rolling back (default `90s`). Bare integer = seconds. |
| `io.pullpilot.stop-timeout` | Stop grace period before the old container is killed on recreate. |
| `io.pullpilot.remove-anonymous-volumes` | `true` to destroy anonymous volumes when recreating (default off — they're preserved). |
| `io.pullpilot.order` | Integer ordering within a cycle (lower first; ties break by name). |

> **To hold a service back**, you have two tools: digest-pin it
> (`image: repo@sha256:…` — **never** updated, the pin is the contract) or label
> it `io.pullpilot.monitor-only=true` (you still get notified, nothing is applied).

---

## How it behaves

A concise reference for what PullPilot actually does in each situation.

- **Scope selection.** Default scope is `project`: PullPilot manages only
  containers sharing its own Compose project (`com.docker.compose.project`).
  `PP_SCOPE=all` manages every non-excluded container on the host;
  `PP_SCOPE=project:<name>` targets a specific project (see
  [Recipes](#common-setups--recipes)).
  - **Fail-safe.** In the default project scope, if PullPilot can't identify its
    own container (so it can't learn its project), it **manages nothing** rather
    than silently going host-wide. Fix it with `io.pullpilot.self=true` or an
    explicit `PP_SCOPE` (see [Troubleshooting](#troubleshooting)).
- **Soak timer.** When a newer digest first appears, PullPilot records the
  first-seen time (persisted in `PP_DATA_DIR`) and waits out the soak window
  (`PP_SOAK`, or `io.pullpilot.soak` per container) before rolling out. The timer
  is per **digest**: if the tag moves to an even newer digest mid-soak, the clock
  resets to that newest digest. `status` and dry-run **peek** at the timer without
  advancing it.
- **Health gate + rollback.** After recreate, PullPilot waits for the container.
  - With a healthcheck: it must report `healthy` within the health timeout
    (`90s` default, override with `io.pullpilot.health-timeout`). `unhealthy` or a
    timeout triggers rollback.
  - **Without a healthcheck:** best-effort crash-loop detection — the container
    must stay running and not increment its restart count for the **full** window.
  - On failure it **rolls back to the previous digest** and **blacklists the bad
    digest** so it's never auto-retried (see below). A rollback (and any
    pull/create/start failure) sends a notification.
- **Bad-digest blacklist.** A digest that genuinely fails its health gate is
  recorded and **never retried** — until a *newer* digest appears. This is the
  answer to "a broken update happened once, why won't it try again?": it won't,
  by design, until upstream ships something newer. (An *interrupted* gate — daemon
  shutdown or timeout cancellation — does **not** blacklist the digest.)
- **Self-heal of an interrupted update.** A recreate is a critical section; it
  detaches from shutdown signals so a `SIGTERM` mid-update can't strand a
  container stopped-and-renamed. If a cycle is still interrupted (e.g. host
  reboot), the next cycle reconciles the leftover `<name>_pp_old`: it removes the
  orphan if the replacement is already in place, or restores the original if not.
- **Monitor-only.** `io.pullpilot.monitor-only=true` detects new images and
  notifies, but never updates.
- **Digest pinning.** `image: repo@sha256:…` is **never** updated — the pin is the
  contract. It shows as `pinned by digest` in `status`.
- **Cleanup.** `PP_CLEANUP=true` removes the old image after a healthy update
  (best-effort; skipped if still in use). Off by default.
- **Jitter.** `PP_JITTER` (default `30m`) adds a random delay to each *scheduled*
  run to spread registry load. Webhook and startup runs are not jittered.
- **Notify once per new digest.** Soak/monitor notifications fire once per new
  digest, not every cycle — you won't get nightly "still soaking" spam.
- **Self-update is notify-only.** A newer PullPilot image is surfaced as a
  notification (when `PP_SELF_UPDATE=true`), never applied in place — applying it
  would kill the daemon mid-update. Upgrade it yourself (see
  [Upgrading PullPilot](#upgrading-pullpilot)).

---

## Common setups / Recipes

Copy-paste snippets for the things people actually ask for. All of these go in
the `pullpilot` service of your compose file (`environment:` / `volumes:`).

### Private registry login

PullPilot pulls images, so it needs your registry credentials to fetch private
ones. It reads them from a Docker `config.json` — either `$DOCKER_CONFIG/config.json`
or `~/.docker/config.json` of the user the daemon runs as. Mount your existing
config read-only:

```yaml
# Default example runs as root (user: "0:0"), whose home is /root:
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - pullpilot-data:/data
  - ${HOME}/.docker/config.json:/root/.docker/config.json:ro
```

If PullPilot runs **non-root** (e.g. the socket-proxy sample, `user: "65532:65532"`,
home `/home/nonroot`), point at that home instead — or, simplest and
home-independent, set `DOCKER_CONFIG`:

```yaml
environment:
  DOCKER_CONFIG: /pp-docker
volumes:
  - ${HOME}/.docker/config.json:/pp-docker/config.json:ro
```

PullPilot reads only the `auths` block — it does **not** run credential helpers.
So the mounted `config.json` must contain **inline** `auths` (a `docker login` on
a server with no `credsStore`/`credHelpers`). On Docker Desktop / setups with a
credential helper the tokens live in the OS keychain and `config.json` has no
`auths` to read — log in on the host without a helper, or copy the auth entry in.
Anonymous Docker Hub pulls that hit a rate limit are fixed the same way.

### Notifications end-to-end

Set `PP_NOTIFY_URL` to any [shoutrrr](https://containrrr.dev/shoutrrr/) URL. The
notification **title** ("PullPilot · Updated: …") is sent as the message header on
services that support one (ntfy/Telegram headers, Discord embeds).

```yaml
environment:
  # ntfy — subscribe to this topic in the ntfy app:
  PP_NOTIFY_URL: "ntfy://ntfy.sh/my-pullpilot-topic"
  # Discord (from a channel webhook URL .../webhooks/<id>/<token>):
  # PP_NOTIFY_URL: "discord://<token>@<id>"
```

You get notified when an image **enters soak** ("rolling out in 24h"), when it's
**updated** (or **monitor-only** has a new image available), and on **failures** —
a failed pull/create/start, a health-gate **rollback**, and the rare
"manual intervention needed" case.

### Managing a second / foreign compose stack

By default PullPilot manages its own project. To have it manage a *different*
stack instead, name that project:

```yaml
environment:
  PP_SCOPE: "project:media"   # manage the 'media' compose project, not this one
```

(The project name is the `com.docker.compose.project` label — usually the stack's
directory name, or whatever you pass to `-p`.)

### Pin or hold a specific service

On the **target** container (not PullPilot), either label it monitor-only or
digest-pin it:

```yaml
services:
  database:
    image: postgres:16
    labels:
      io.pullpilot.monitor-only: "true"   # notify on new images, never auto-update

  cache:
    image: redis:7@sha256:abcd...         # digest-pinned: never updated at all
```

---

## Upgrading PullPilot

Self-update is **notify-only** (applying it in place would kill the daemon
mid-update), so PullPilot upgrades like any other compose service — you do it:

```bash
docker compose pull pullpilot
docker compose up -d pullpilot
```

If you pin a specific version (`:vX.Y.Z`), bump the tag in your compose file
first, then run the two commands above. Pinning to **`:stable`** gives you the
latest release on each `pull`; pinning `:vX.Y.Z` gives reproducible upgrades on
your schedule. Set `PP_SELF_UPDATE=true` if you want a notification when a newer
PullPilot image is available.

---

## Troubleshooting

First-run problems and how to fix them, by symptom.

### "Cannot connect to the Docker daemon" / "permission denied"

PullPilot can't reach the Docker socket. On boot it now **pings Docker and surfaces
this exact guidance loudly** instead of looking healthy and silently doing nothing.
(`status` and `run` exit non-zero immediately; `serve` logs the error and keeps
retrying on schedule, so fixing the mount and restarting clears it.)

- **Cause:** the socket isn't mounted, or PullPilot can't access it.
- **Fix:** mount the socket and run with access to it:
  ```yaml
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
  user: "0:0"   # root — needed for the default socket
  ```
- **NAS / Pi / Synology quirk:** the socket may live elsewhere
  (e.g. Synology DSM exposes it at `/var/run/docker.sock` but under a different
  package path on some setups) — mount the path your host actually uses, on both
  sides if needed (`/your/host/path/docker.sock:/var/run/docker.sock`).
- **Don't want to run as root?** Use **rootless Docker** (see
  [Running rootless](#running-rootless-the-one-real-mitigation)) or the
  [`samples/socket-proxy/`](samples/socket-proxy/) variant — those are the
  non-root alternatives.

### `checked=0` / "nothing happens"

PullPilot only manages **other** containers in the **same compose project**. If
it's the only thing in its project, there's nothing to check.

- **Fix:** add the `pullpilot` service to an existing stack (so it shares that
  project), or set `PP_SCOPE=project:<name>` to target one. To just watch it work,
  use [`samples/playground/`](samples/playground/), which ships throwaway targets.

### "could not identify PullPilot's own container" (WARN) / nothing managed

PullPilot finds itself by matching its hostname to a container ID, or the
`io.pullpilot.self` label. If you set a custom `hostname:` on the service, it
can't, and in the **default project scope it then manages nothing** (the
fail-safe — it refuses to risk going host-wide).

- **Fix:** add `io.pullpilot.self: "true"` to the PullPilot service, **or** set
  `PP_SCOPE=project:<name>` explicitly.

### Docker Hub rate-limit / registry `401`

The registry check (or pull) was rejected.

- **Private image / Hub rate limit:** log in — see
  [Private registry login](#private-registry-login).
- **Locally-built image** (no registry copy): shows as
  `no local repo digest (locally-built image?)` or `registry unreachable` and is
  **skipped**. That's normal — PullPilot can only update images it can re-pull.

### "data dir is NOT a persistent mount" warning

`PP_DATA_DIR` isn't a real volume, so the ed25519 identity, soak timers, and
webhook registration live in the container's ephemeral layer.

- **Consequence:** on restart the identity regenerates → **your webhook poke URL
  changes** (breaking whatever POSTs to it), and soak timers reset.
- **Fix:** mount a named volume (or bind mount) at `PP_DATA_DIR`:
  ```yaml
  volumes:
    - pullpilot-data:/data
  ```

### "It updated once, broke, and now won't update"

A health-checked update that **failed its gate is never retried** — the digest is
blacklisted until a *newer* digest appears upstream. This is intentional (it stops
PullPilot from re-applying a known-bad image every cycle). Check `pullpilot status`:
a held image shows `digest previously failed health check`. Push/await a newer
image and PullPilot will try that one.

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

> The held connection **reconnects every few minutes** — that's normal:
> Cloudflare periodically recycles the Durable Object holding the socket. No pokes
> are lost (the relay stores one pending poke and delivers it on reconnect, and
> the scheduled poll is the backstop). Routine reconnects are silent at the
> default log level; you'll only see a `WARN` if reconnection *keeps* failing.

Enable it:

```yaml
environment:
  PP_WEBHOOK: "true"
  # PP_WEBHOOK_URL defaults to the public relay; override to self-host.
```

> ⚠️ On `:latest`/`:edge` images, the baked default `PP_WEBHOOK_URL` is the
> **test** relay. For real deployments use `:stable` (which defaults to the
> production relay) or set `PP_WEBHOOK_URL` explicitly. The webhook also needs a
> **persistent `PP_DATA_DIR`** — without it the identity (and your poke URL)
> changes on every restart.

On first start PullPilot provisions a webhook and logs your **poke URL**. Point
your CI / registry webhook at it — **either `POST` or `GET`** works, so even tools
that only do a plain `GET` (registries, `wget`/`curl` in cron) can trigger it.

Because a poke is non-authoritative (it can never name an image or skip a gate),
the worst anything can do by hitting the URL is cause one extra, rate-limited
check — so `GET` is safe. Still, treat the poke URL as a **write-capable secret**:
don't paste it anywhere that auto-fetches links (a chat that unfurls URLs, an HTML
`<img src>`). Abandoned webhooks are auto-pruned after 6 months of inactivity.

### When to fire the poke (timing matters!)

> ⚠️ **Fire the poke *after* the new image is pushed to the registry — not on
> `git push`.** A poke just makes PullPilot re-check the registry *now*. If you
> trigger it before your build has pushed the image (e.g. from a GitHub `push`
> event), PullPilot checks, sees the **same old digest**, and does nothing — then
> the image lands minutes later with no poke, and you wait for the next scheduled
> poll. Wire the trigger to the moment the image actually exists.

**Best: the last step of your build/release pipeline**, after `docker push`
succeeds. Store the poke URL as a secret and `curl` it:

```yaml
# .github/workflows/release.yml — after the image is pushed
- name: Notify PullPilot
  run: curl -fsS -X POST "${{ secrets.PULLPILOT_POKE_URL }}"
```

```bash
# any CI / Makefile / script, right after `docker push …`
curl -fsS -X POST "$PULLPILOT_POKE_URL"
```

**Or a registry "image pushed" webhook** — these fire only once the image is in
the registry, so the timing is correct:

- **Docker Hub** → repository **Webhooks** → add your poke URL.
- **GitHub Container Registry (GHCR)** → repo/org webhook on the
  **`registry_package` / `package` — *published*** event (this fires when the
  image is published, **not** on `git push`).
- **Harbor / others** → the registry's push/notification webhook.

**Heads-up on soak:** a correctly-timed poke *starts the soak clock* the moment
the image lands — it doesn't roll out instantly under the default `PP_SOAK=24h`.
For near-immediate rollout on a poke, set `PP_SOAK=0` (or per service via
`io.pullpilot.soak: 0`). A mistimed or duplicate poke is always harmless
(idempotent + debounced), and the scheduled poll is the backstop.

### Self-hosting the relay

The relay holds **no shared secret** — your per-webhook ed25519 keypair is the
trust root. Deploy your own:

```bash
cd worker
npm install

# Create the production KV namespace and paste the printed id into BOTH the
# top-level [[kv_namespaces]] block AND [[env.production.kv_namespaces]] in
# wrangler.toml — they use the SAME id (the binding is PULLPILOT_REGISTRY).
wrangler kv namespace create PULLPILOT_REGISTRY

# Optional: only if you also want a test environment that mirrors the
# :latest/:edge channel. Paste this id into [[env.test.kv_namespaces]].
wrangler kv namespace create PULLPILOT_REGISTRY --env test

wrangler deploy --env production
# wrangler deploy --env test   # only if you created the test namespace above
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

**The honest bottom line:** trusting PullPilot with the socket is the same
decision as trusting Watchtower, or any auto-updater — you are trusting the
PullPilot image and the upstream images it pulls. If that trust is acceptable to
you (it is for most homelabs), the example compose is safe to run as-is. If it
isn't, run Docker rootless; nothing in between meaningfully changes the
root-equivalence.

### Running rootless (the one real mitigation)

Under [rootless Docker](https://docs.docker.com/engine/security/rootless/), the
daemon and your containers run as your unprivileged user, not `root`. **In one
line: container-create stops being host-root** — a container that mounts `/` or
grabs every capability is confined to *your* user, so PullPilot's socket access is
no longer root-equivalent. Practical notes for using it with PullPilot:

- The socket lives at `$XDG_RUNTIME_DIR/docker.sock` (typically
  `/run/user/<uid>/docker.sock`), **not** `/var/run/docker.sock`. Point PullPilot
  at the rootless socket — either mount that path, or set `DOCKER_HOST` — and drop
  the `user: "0:0"` line (rootless needs no in-container root):

  ```yaml
  volumes:
    - /run/user/1000/docker.sock:/var/run/docker.sock
    # user: "0:0"   ← not needed under rootless
  # Alternatively, mount your own socket path and set:
  #   environment:
  #     DOCKER_HOST: unix:///run/user/1000/docker.sock
  ```
- Keep the example's hardening (read-only rootfs, `cap_drop: ALL`,
  `no-new-privileges`); it composes cleanly with rootless.
- Rootless has its own constraints (no host-port < 1024 without setup, some
  storage-driver caveats) — see the upstream docs before committing a host to it.

What PullPilot *does* do well:

- **No inbound ports.** The daemon never listens; the webhook-relay design exists
  precisely so you don't expose anything. The only way in is compromising the
  PullPilot image/process itself — so keep it pinned and trusted.
- **The realistic threat is a bad upstream image, not the socket** — handled by
  digest pinning, the **soak window** (a broken/malicious `:latest` is caught
  upstream before it reaches you), and **health-gated rollback**.
- **The relay is untrusted by design** — pokes are non-authoritative (they can
  never name an image or skip a gate), the listen connection is ed25519
  challenge-response authenticated, and pokes are rate-limited + coalesced. The
  webhook is pure acceleration: your local schedule (`PP_SCHEDULE`) runs
  regardless, so a hostile or dead relay can speed nothing up and suppress
  nothing — at worst you fall back to your normal poll cadence.
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

> **Maintainer / self-hoster setup:** add repo secrets `CLOUDFLARE_API_TOKEN`
> (Workers Scripts: Edit) and `CLOUDFLARE_ACCOUNT_ID` to enable worker deploys,
> and fill the KV namespace ids in `worker/wrangler.toml`. Image publishing uses
> the built-in `GITHUB_TOKEN`. (Worker deploys are skipped if
> `CLOUDFLARE_API_TOKEN` is unset, so forks build fine without it.)

---

## License

[MIT](LICENSE) © 2026 Jeffrey Clement
