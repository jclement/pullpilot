// Package engine implements PullPilot's update cycle: discover in-scope
// containers, decide which have a genuinely newer (and soaked) image, and
// recreate them with health-gated rollback.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	apiregistry "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/rs/zerolog"

	"github.com/jclement/pullpilot/internal/config"
	"github.com/jclement/pullpilot/internal/labels"
	"github.com/jclement/pullpilot/internal/registry"
	"github.com/jclement/pullpilot/internal/state"
)

// Notifier receives human-facing events (updates, rollbacks, failures).
type Notifier interface {
	Notify(ctx context.Context, title, body string)
}

// Engine orchestrates one or more update cycles.
type Engine struct {
	cli  *client.Client
	reg  *registry.Client
	st   *state.State
	cfg  *config.Config
	log  zerolog.Logger
	note Notifier

	selfID         string
	project        string
	selfIdentified bool
}

// New connects to Docker, identifies PullPilot's own container, and resolves
// the default update scope (its own Compose project).
//
// Docker SDK note: we intentionally stay on github.com/docker/docker v27.3.1.
// Its successor, github.com/moby/moby/client, is still pre-1.0 (v0.5.0) with an
// actively-redesigned API; we'll migrate once it reaches a stable 1.0. The
// CVEs govulncheck flags on this module are daemon-side and unreachable from a
// client-only consumer.
func New(cfg *config.Config, st *state.State, log zerolog.Logger, note Notifier) (*Engine, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	e := &Engine{cli: cli, reg: registry.New(), st: st, cfg: cfg, log: log, note: note}
	e.identifySelf(context.Background())
	return e, nil
}

// Ping verifies the Docker daemon is reachable, with an actionable error. The
// client connects lazily, so without this a missing/forbidden socket only
// surfaces (cryptically) on the first cycle.
func (e *Engine) Ping(ctx context.Context) error {
	if _, err := e.cli.Ping(ctx); err != nil {
		host := os.Getenv("DOCKER_HOST")
		if host == "" {
			host = "unix:///var/run/docker.sock"
		}
		return fmt.Errorf("cannot reach the Docker daemon at %s: %w\n"+
			"  check that the socket is mounted into this container and PullPilot can access it:\n"+
			"  - mount it:  /var/run/docker.sock:/var/run/docker.sock\n"+
			"  - access it: run as root (user: \"0:0\") for the default socket, or use rootless Docker / the socket-proxy sample",
			host, err)
	}
	return nil
}

// Close releases the Docker client.
func (e *Engine) Close() error { return e.cli.Close() }

// identifySelf finds PullPilot's own container (hostname == container ID, or the
// io.pullpilot.self label) and records its Compose project for default scoping.
func (e *Engine) identifySelf(ctx context.Context) {
	host, _ := os.Hostname()
	list, err := e.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		e.log.Debug().Err(err).Msg("self-identification: could not list containers")
		return
	}
	for _, c := range list {
		if host != "" && strings.HasPrefix(c.ID, host) {
			e.selfID, e.project, e.selfIdentified = c.ID, c.Labels[labels.ComposeProject], true
			break
		}
	}
	if !e.selfIdentified {
		for _, c := range list {
			if labels.Parse(c.Labels, false).IsSelf {
				e.selfID, e.project, e.selfIdentified = c.ID, c.Labels[labels.ComposeProject], true
				break
			}
		}
	}

	switch {
	case e.selfIdentified:
		e.log.Info().Str("project", e.project).Msg("identified PullPilot's own container")
	case e.cfg.Scope.Mode == "project" && e.cfg.Scope.Project == "":
		// Only consequential in the default project scope: without a project we
		// fail safe and manage nothing (see discover).
		e.log.Warn().Msg("could not identify PullPilot's own container (custom hostname?) — " +
			"set io.pullpilot.self=true on this service, or set PP_SCOPE=project:<name>. " +
			"In the default project scope, PullPilot will refuse to manage anything rather than risk host-wide updates.")
	default:
		e.log.Debug().Msg("could not identify own container; scope is explicit, continuing")
	}
}

// Action is the decided outcome for a container.
type Action string

const (
	ActionUpToDate Action = "up-to-date"
	ActionSkip     Action = "skip"
	ActionMonitor  Action = "monitor"
	ActionSoaking  Action = "soaking"
	ActionUpdate   Action = "update"
)

// plan is the per-container evaluation result.
type plan struct {
	id        string
	name      string
	image     string
	current   string // running repo digest
	available string // remote digest
	action    Action
	reason    string
	soakLeft  time.Duration
	inspect   types.ContainerJSON
	settings  labels.Settings
	running   bool
}

// buildPlan discovers in-scope containers and evaluates each. record controls
// whether the soak first-seen timestamp is persisted (true for a real cycle,
// false for read-only views like `status` / dry-run).
func (e *Engine) buildPlan(ctx context.Context, record bool) ([]plan, error) {
	targets, err := e.discover(ctx)
	if err != nil {
		return nil, err
	}
	plans := make([]plan, 0, len(targets))
	for _, t := range targets {
		plans = append(plans, e.evaluate(ctx, t, record))
	}
	// Stable, deterministic order (io.pullpilot.order then name).
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].settings.Order != plans[j].settings.Order {
			return plans[i].settings.Order < plans[j].settings.Order
		}
		return plans[i].name < plans[j].name
	})
	return plans, nil
}

// Status prints a read-only table of every managed container and what PullPilot
// would do — without changing anything or advancing soak timers.
func (e *Engine) Status(ctx context.Context) error {
	plans, err := e.buildPlan(ctx, false)
	if err != nil {
		return err
	}
	fmt.Printf("scope: %s\n\n", e.scopeLabel())
	if len(plans) == 0 {
		fmt.Println("No managed containers in scope.")
		return nil
	}
	e.renderTable(os.Stdout, plans)
	return nil
}

// Run executes one full cycle. trigger is "schedule" or "webhook" (for logs).
func (e *Engine) Run(ctx context.Context, trigger string) {
	start := time.Now()
	e.log.Info().Str("trigger", trigger).Str("scope", e.scopeLabel()).Msg("update cycle starting")

	if e.cfg.DryRun {
		plans, err := e.buildPlan(ctx, false)
		if err != nil {
			e.log.Error().Err(err).Msg("discovery failed")
			return
		}
		fmt.Fprintln(os.Stderr, "[dry-run] plan (no changes will be made):")
		e.renderTable(os.Stderr, plans)
		return
	}

	// Heal any container left half-recreated by an interrupted previous cycle.
	e.reconcileOrphans(ctx)

	plans, err := e.buildPlan(ctx, true)
	if err != nil {
		e.log.Error().Err(err).Msg("discovery failed")
		return
	}

	var updated, soaking, failed int
	for _, p := range plans {
		switch p.action {
		case ActionUpdate:
			if err := e.apply(ctx, p); err != nil {
				failed++
				e.log.Error().Err(err).Str("container", p.name).Msg("update failed")
				e.note.Notify(ctx, "Update FAILED: "+p.name,
					fmt.Sprintf("%s could not be updated to %s: %v", p.name, short(p.available), err))
			} else {
				updated++
			}
		case ActionSoaking:
			soaking++
			e.log.Info().Str("container", p.name).Str("image", p.image).
				Str("to", short(p.available)).Dur("soak_left", p.soakLeft.Round(time.Minute)).
				Msg("new image detected, soaking")
			// Notify once per new digest, not every cycle.
			if e.st.FirstNotify(p.name, p.available) {
				e.note.Notify(ctx, "Update soaking: "+p.name,
					fmt.Sprintf("%s has a new image (%s); rolling out in %s unless stopped.",
						p.name, short(p.available), p.soakLeft.Round(time.Minute)))
			}
		case ActionMonitor:
			e.log.Info().Str("container", p.name).Str("to", short(p.available)).
				Str("reason", p.reason).Msg("update available (monitor-only)")
			if e.st.FirstNotify(p.name, p.available) {
				e.note.Notify(ctx, "Update available: "+p.name,
					fmt.Sprintf("%s has a new image (%s). %s — not applied.", p.name, short(p.available), p.reason))
			}
		default:
			e.log.Debug().Str("container", p.name).Str("action", string(p.action)).
				Str("reason", p.reason).Msg("no action")
		}
	}

	e.log.Info().Int("updated", updated).Int("soaking", soaking).Int("failed", failed).
		Int("checked", len(plans)).Dur("took", time.Since(start).Round(time.Millisecond)).
		Msg("update cycle complete")
}

func (e *Engine) scopeLabel() string {
	if e.cfg.Scope.Mode == "all" {
		return "all"
	}
	if p := e.resolvedProject(); p != "" {
		return "project:" + p
	}
	return "project:(unknown)"
}

// resolvedProject returns the project to scope to (explicit override, else the
// self-detected one) in project mode.
func (e *Engine) resolvedProject() string {
	if e.cfg.Scope.Project != "" {
		return e.cfg.Scope.Project
	}
	return e.project
}

// renderTable prints an aligned table of the plan (used by `status` and dry-run).
func (e *Engine) renderTable(w io.Writer, plans []plan) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tCURRENT\tAVAILABLE\tSTATE")
	for _, p := range plans {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.name, dash(short(p.current)), dash(short(p.available)), stateText(p))
	}
	tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func stateText(p plan) string {
	switch p.action {
	case ActionUpToDate:
		return "up to date"
	case ActionUpdate:
		return "update ready"
	case ActionSoaking:
		return fmt.Sprintf("soaking (%s left)", p.soakLeft.Round(time.Minute))
	case ActionMonitor:
		return "update available (" + p.reason + ")"
	default:
		return p.reason
	}
}

// discover enumerates in-scope, eligible containers (including stopped ones).
func (e *Engine) discover(ctx context.Context) ([]target, error) {
	opts := container.ListOptions{All: true}
	if e.cfg.Scope.Mode == "project" {
		proj := e.resolvedProject()
		if proj == "" {
			// Fail safe: an unknown project must NOT fall back to managing every
			// container on the host. Manage nothing until self-id or PP_SCOPE is fixed.
			e.log.Error().Msg("project scope but no project could be determined — managing nothing. " +
				"Set io.pullpilot.self=true on this service, or set PP_SCOPE=project:<name>.")
			return nil, nil
		}
		opts.Filters = filters.NewArgs(filters.Arg("label", labels.ComposeProject+"="+proj))
	}
	list, err := e.cli.ContainerList(ctx, opts)
	if err != nil {
		return nil, err
	}
	var targets []target
	for _, c := range list {
		set := labels.Parse(c.Labels, e.cfg.CompatWatchtower)
		if !e.eligible(c.ID, set) {
			continue
		}
		insp, err := e.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			e.log.Warn().Err(err).Str("container", trimName(c.Names)).Msg("inspect failed, skipping")
			continue
		}
		targets = append(targets, target{
			id:      c.ID,
			name:    strings.TrimPrefix(insp.Name, "/"),
			image:   insp.Config.Image,
			inspect: insp,
			set:     set,
			running: insp.State != nil && insp.State.Running,
		})
	}
	return targets, nil
}

// eligible applies scope and label opt-in/opt-out rules.
func (e *Engine) eligible(id string, set labels.Settings) bool {
	if id == e.selfID && !e.cfg.SelfUpdate {
		return false // PullPilot's own container; only considered when self-update is enabled
	}
	if set.Oneoff || set.Exclude {
		return false
	}
	if set.Enable != nil && !*set.Enable {
		return false
	}
	// In "all" scope we manage everything not explicitly excluded; otherwise the
	// compose-project filter already scoped us.
	return true
}

type target struct {
	id      string
	name    string
	image   string
	inspect types.ContainerJSON
	set     labels.Settings
	running bool
}

// evaluate decides the action for one container. record persists the soak
// first-seen timestamp; pass false for read-only views.
func (e *Engine) evaluate(ctx context.Context, t target, record bool) plan {
	p := plan{id: t.id, name: t.name, image: t.image, inspect: t.inspect, settings: t.set, running: t.running}

	ref, err := registry.ParseRef(t.image)
	if err != nil {
		p.action, p.reason = ActionSkip, "unparseable image ref"
		return p
	}
	if ref.Pinned() {
		p.action, p.reason = ActionSkip, "pinned by digest"
		return p
	}

	current, err := e.runningDigest(ctx, t)
	if err != nil || current == "" {
		p.action, p.reason = ActionSkip, "no local repo digest (locally-built image?)"
		return p
	}
	p.current = current

	remote, err := e.reg.RemoteDigest(ctx, t.image)
	if err != nil {
		// Common and non-fatal (locally-built images have no registry); keep it
		// out of the warn stream — the detail is at debug and in `status`.
		p.action, p.reason = ActionSkip, "registry unreachable"
		e.log.Debug().Err(err).Str("container", t.name).Msg("registry check failed")
		return p
	}
	p.available = remote

	if remote == current {
		p.action, p.reason = ActionUpToDate, "current"
		return p
	}
	if t.id == e.selfID {
		// A real self-update would have to stop/recreate the very container this
		// process runs in, killing the daemon mid-apply. Until an out-of-band
		// orchestrator handoff is implemented, surface self-updates as a
		// notification only — never apply them in-place.
		p.action, p.reason = ActionMonitor, "self-update available (notify only)"
		return p
	}
	if e.st.IsBad(remote) {
		p.action, p.reason = ActionSkip, "digest previously failed health check"
		return p
	}
	if t.set.MonitorOnly {
		p.action, p.reason = ActionMonitor, "monitor-only"
		return p
	}

	// Soak gate.
	soak := e.cfg.Soak
	if t.set.Soak != nil {
		soak = *t.set.Soak
	}
	var first time.Time
	if record {
		first = e.st.SeenAt(t.name, remote, time.Now())
	} else if t0, ok := e.st.PeekSeen(t.name, remote); ok {
		first = t0
	} else {
		first = time.Now() // not yet tracked; show the full soak as remaining
	}
	if elapsed := time.Since(first); elapsed < soak {
		p.action, p.reason = ActionSoaking, "within soak window"
		p.soakLeft = soak - elapsed
		return p
	}
	p.action, p.reason = ActionUpdate, "newer image, soak elapsed"
	return p
}

// runningDigest returns the repo digest of the image the container is running.
func (e *Engine) runningDigest(ctx context.Context, t target) (string, error) {
	insp, _, err := e.cli.ImageInspectWithRaw(ctx, t.inspect.Image)
	if err != nil {
		return "", err
	}
	return matchRepoDigest(t.image, insp.RepoDigests), nil
}

// matchRepoDigest picks the digest whose repository matches the image ref.
func matchRepoDigest(image string, repoDigests []string) string {
	if len(repoDigests) == 0 {
		return ""
	}
	want, err := registry.ParseRef(image)
	if err == nil {
		for _, rd := range repoDigests {
			name, dg, ok := strings.Cut(rd, "@")
			if !ok {
				continue
			}
			if got, err := registry.ParseRef(name); err == nil &&
				got.Repository == want.Repository && got.Registry == want.Registry {
				return dg
			}
		}
	}
	// Only fall back to the sole digest when the image is under exactly one
	// repository; a blind [0] across multiple repos could compare against an
	// unrelated repository and report perpetual "newer image".
	if len(repoDigests) == 1 {
		if _, dg, ok := strings.Cut(repoDigests[0], "@"); ok {
			return dg
		}
	}
	return ""
}

// apply pulls the new image and recreates the container, health-gating the
// result and rolling back to the prior container on failure.
func (e *Engine) apply(ctx context.Context, p plan) error {
	e.log.Info().Str("container", p.name).Str("image", p.image).
		Str("from", short(p.current)).Str("to", short(p.available)).Msg("updating")

	// 1. Pull the new image fully BEFORE touching the running container. Bound it
	// so a slow/throttled registry can't stall the whole daemon (it holds the
	// single-flight lock) indefinitely.
	pullCtx, cancelPull := context.WithTimeout(ctx, 10*time.Minute)
	err := e.pull(pullCtx, p.image)
	cancelPull()
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	// The recreate is a critical section: once the old container is stopped and
	// renamed, the sequence MUST complete (or roll back) even if the daemon is
	// asked to shut down. Detach from ctx cancellation so a SIGTERM mid-recreate
	// can't strand the container stopped-and-renamed. Interrupted recreates are
	// also reconciled at the start of the next cycle (reconcileOrphans). Size the
	// budget to the health-gate window so a long io.pullpilot.health-timeout
	// can't be cut short and force a spurious rollback.
	healthTimeout := 90 * time.Second
	if p.settings.HealthTimeout != nil {
		healthTimeout = *p.settings.HealthTimeout
	}
	opTimeout := healthTimeout + 2*time.Minute
	if opTimeout < 5*time.Minute {
		opTimeout = 5 * time.Minute
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()

	oldName := p.name
	tmpName := p.name + "_pp_old"
	insp := p.inspect

	// 2. Stop (honoring grace) and free the name by renaming the old container.
	stopTimeout := (*int)(nil)
	if p.settings.StopTimeout != nil {
		s := int(p.settings.StopTimeout.Seconds())
		stopTimeout = &s
	}
	if p.running {
		if err := e.cli.ContainerStop(opCtx, p.id, container.StopOptions{Timeout: stopTimeout}); err != nil {
			return fmt.Errorf("stop old: %w", err)
		}
	}
	if err := e.cli.ContainerRename(opCtx, p.id, tmpName); err != nil {
		return fmt.Errorf("rename old: %w", err)
	}

	// 3. Create the replacement from the old container's config + new image.
	cfg := *insp.Config
	cfg.Image = p.image
	// Docker sets Config.Hostname to the container's short ID when no hostname
	// was given; reusing it verbatim would give the new container the OLD id as
	// its hostname, breaking PullPilot's own hostname-prefix self-identification
	// after a recreate. Clear it in that case so Docker assigns the new id.
	if cfg.Hostname != "" && strings.HasPrefix(p.id, cfg.Hostname) {
		cfg.Hostname = ""
	}
	hostCfg := insp.HostConfig
	netCfg, extraNets := buildNetworking(insp.NetworkSettings)

	created, err := e.cli.ContainerCreate(opCtx, &cfg, hostCfg, netCfg, nil, oldName)
	if err != nil {
		// Recreate failed: restore the old container.
		e.rollback(opCtx, p, tmpName, "")
		return fmt.Errorf("create new: %w", err)
	}

	// 4. Attach any additional networks (create reliably attaches only one). A
	// failure here means the container would go live with degraded connectivity,
	// so roll back rather than delete the working old container.
	for name, ep := range extraNets {
		if err := e.cli.NetworkConnect(opCtx, name, created.ID, ep); err != nil {
			e.rollback(opCtx, p, tmpName, created.ID)
			return fmt.Errorf("attach network %s: %w", name, err)
		}
	}

	// 5. Start (only if the old one was running) and health-gate.
	if p.running {
		if err := e.cli.ContainerStart(opCtx, created.ID, container.StartOptions{}); err != nil {
			e.rollback(opCtx, p, tmpName, created.ID)
			return fmt.Errorf("start new: %w", err)
		}
		if err := e.healthGate(opCtx, created.ID, p.settings); err != nil {
			// A genuine unhealthy/exited/restart-loop verdict blacklists the
			// digest so it is never auto-retried. An interrupted gate (timeout or
			// cancellation) must NOT permanently reject an otherwise-good image.
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				e.st.MarkBad(p.available)
			}
			e.rollback(opCtx, p, tmpName, created.ID)
			e.note.Notify(opCtx, "Update rolled back: "+p.name,
				fmt.Sprintf("%s failed health check on %s and was rolled back to %s.",
					p.name, short(p.available), short(p.current)))
			return fmt.Errorf("health gate: %w", err)
		}
	}

	// 6. Success: remove the old container and record state. Anonymous volumes
	// are preserved unless the container opts in via the label.
	_ = e.cli.ContainerRemove(opCtx, tmpName, container.RemoveOptions{RemoveVolumes: p.settings.RemoveAnonVols})
	e.st.MarkApplied(p.name, p.available)
	if e.cfg.Cleanup {
		if _, err := e.cli.ImageRemove(opCtx, p.current, image.RemoveOptions{}); err != nil {
			e.log.Debug().Err(err).Msg("old image cleanup skipped (still in use?)")
		}
	}
	e.log.Info().Str("container", p.name).Str("now", short(p.available)).Msg("updated and healthy")
	e.note.Notify(opCtx, "Updated: "+p.name,
		fmt.Sprintf("%s updated %s → %s and is healthy.", p.name, short(p.current), short(p.available)))
	return nil
}

// rollback removes a failed new container and restores the renamed old one.
func (e *Engine) rollback(ctx context.Context, p plan, tmpName, newID string) {
	if newID != "" {
		_ = e.cli.ContainerStop(ctx, newID, container.StopOptions{})
		_ = e.cli.ContainerRemove(ctx, newID, container.RemoveOptions{})
	}
	if err := e.cli.ContainerRename(ctx, tmpName, p.name); err != nil {
		e.log.Error().Err(err).Str("container", p.name).Msg("rollback rename failed — manual intervention may be needed")
		e.note.Notify(ctx, "ACTION NEEDED: "+p.name+" may be down",
			fmt.Sprintf("PullPilot could not restore %s after a failed update (%v). "+
				"Check for a container named %s_pp_old and recover it manually.", p.name, err, p.name))
		return
	}
	if p.running {
		if err := e.cli.ContainerStart(ctx, p.id, container.StartOptions{}); err != nil {
			e.log.Error().Err(err).Str("container", p.name).Msg("rollback start failed")
		}
	}
	e.log.Warn().Str("container", p.name).Str("restored", short(p.current)).Msg("rolled back")
}

// reconcileOrphans heals containers left half-recreated by an interrupted
// previous cycle (a leftover "<name>_pp_old"). If the replacement is in place,
// the orphan is removed; otherwise the orphan is restored under its name.
func (e *Engine) reconcileOrphans(ctx context.Context) {
	list, err := e.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return
	}
	present := map[string]bool{}
	for _, c := range list {
		for _, n := range c.Names {
			present[strings.TrimPrefix(n, "/")] = true
		}
	}
	for _, c := range list {
		for _, raw := range c.Names {
			n := strings.TrimPrefix(raw, "/")
			orig, ok := strings.CutSuffix(n, "_pp_old")
			if !ok {
				continue
			}
			if present[orig] {
				// Replacement already exists; this is the superseded old container.
				_ = e.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{})
				e.log.Warn().Str("container", orig).Msg("removed orphaned _pp_old from an interrupted update")
			} else {
				// Interrupted before the replacement existed: restore the original.
				if err := e.cli.ContainerRename(ctx, c.ID, orig); err != nil {
					e.log.Error().Err(err).Str("container", orig).Msg("failed to restore interrupted update — manual intervention may be needed")
					continue
				}
				_ = e.cli.ContainerStart(ctx, c.ID, container.StartOptions{})
				e.log.Warn().Str("container", orig).Msg("restored container from an interrupted update")
			}
		}
	}
}

// healthGate waits for the container to become healthy, or — when it has no
// healthcheck — to stay running without restart-looping for a short window.
func (e *Engine) healthGate(ctx context.Context, id string, set labels.Settings) error {
	timeout := 90 * time.Second
	if set.HealthTimeout != nil {
		timeout = *set.HealthTimeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var startRestarts int
	first := true
	for {
		insp, err := e.cli.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if insp.State == nil {
			return fmt.Errorf("no state")
		}
		if first {
			startRestarts = insp.RestartCount
			first = false
		}
		if insp.State.Health != nil {
			switch insp.State.Health.Status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container reported unhealthy")
			}
		} else {
			// No healthcheck: best-effort crash-loop detection — require it to
			// stay running and not restart-loop for the full window.
			if !insp.State.Running {
				return fmt.Errorf("container exited (%d)", insp.State.ExitCode)
			}
			if insp.RestartCount > startRestarts {
				return fmt.Errorf("container is restart-looping")
			}
		}
		if time.Now().After(deadline) {
			if insp.State.Health != nil {
				return fmt.Errorf("did not become healthy within %s", timeout)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pull fetches the image and drains the progress stream to completion. It
// forwards registry credentials to the Docker daemon (the daemon does its own
// auth and never sees PullPilot's mounted config.json), so private images pull.
func (e *Engine) pull(ctx context.Context, ref string) error {
	opts := image.PullOptions{}
	if user, pass, host, ok := e.reg.Credentials(ref); ok {
		if auth, err := encodeRegistryAuth(user, pass, host); err == nil {
			opts.RegistryAuth = auth
			e.log.Debug().Str("registry", host).Msg("pulling with registry credentials")
		}
	}
	rc, err := e.cli.ImagePull(ctx, ref, opts)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

// encodeRegistryAuth builds the base64 X-Registry-Auth value the Docker daemon
// expects for an authenticated pull.
func encodeRegistryAuth(user, pass, server string) (string, error) {
	buf, err := json.Marshal(apiregistry.AuthConfig{Username: user, Password: pass, ServerAddress: server})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

// buildNetworking returns a NetworkingConfig that attaches the primary network
// at create time (preserving aliases) and the remaining networks to connect
// afterward.
func buildNetworking(ns *types.NetworkSettings) (*network.NetworkingConfig, map[string]*network.EndpointSettings) {
	extra := map[string]*network.EndpointSettings{}
	if ns == nil || len(ns.Networks) == 0 {
		return &network.NetworkingConfig{}, extra
	}
	names := make([]string, 0, len(ns.Networks))
	for n := range ns.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	primary := names[0]
	cfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		primary: endpointFrom(ns.Networks[primary]),
	}}
	for _, n := range names[1:] {
		extra[n] = endpointFrom(ns.Networks[n])
	}
	return cfg, extra
}

// endpointFrom carries forward the endpoint configuration that must survive a
// recreate: aliases, static IPs (IPAMConfig), links and driver options. Runtime
// fields (assigned IP, gateway, endpoint id) are intentionally not copied.
func endpointFrom(ep *network.EndpointSettings) *network.EndpointSettings {
	if ep == nil {
		return &network.EndpointSettings{}
	}
	return &network.EndpointSettings{
		Aliases:    ep.Aliases,
		Links:      ep.Links,
		IPAMConfig: ep.IPAMConfig,
		DriverOpts: ep.DriverOpts,
	}
}

func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func trimName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
