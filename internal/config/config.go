// Package config loads PullPilot's daemon-wide configuration from PP_* env vars.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/pullpilot/internal/version"
	"github.com/rs/zerolog"
)

// Scope selects which containers PullPilot manages.
type Scope struct {
	Mode    string // "project" | "all"
	Project string // explicit project name for "project:<name>"
}

// Config is the resolved daemon configuration.
type Config struct {
	Schedule         string
	Timezone         string
	Jitter           time.Duration
	Scope            Scope
	Soak             time.Duration
	SelfUpdate       bool
	Cleanup          bool
	Webhook          bool
	WebhookURL       string
	DataDir          string
	NotifyURL        string
	DryRun           bool
	LogLevel         string
	LogJSON          bool
	CompatWatchtower bool

	// Warnings collects non-fatal config problems (e.g. an unparseable value
	// that fell back to a default), surfaced in the boot summary.
	Warnings []string
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	var warnings []string
	durOf := func(key string, def time.Duration) time.Duration {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return def
		}
		if d, ok := parseDuration(v); ok {
			return d
		}
		warnings = append(warnings, fmt.Sprintf("%s=%q is not a valid duration; using %s", key, v, def))
		return def
	}
	boolOf := func(key string, def bool) bool {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return def
		}
		if b, ok := parseBool(v); ok {
			return b
		}
		warnings = append(warnings, fmt.Sprintf("%s=%q is not a valid boolean; using %v", key, v, def))
		return def
	}

	// Catch typo'd PP_* vars (otherwise silently ignored).
	known := map[string]bool{
		"PP_SCHEDULE": true, "PP_TIMEZONE": true, "PP_JITTER": true, "PP_SCOPE": true,
		"PP_SOAK": true, "PP_SELF_UPDATE": true, "PP_CLEANUP": true, "PP_WEBHOOK": true,
		"PP_WEBHOOK_URL": true, "PP_DATA_DIR": true, "PP_NOTIFY_URL": true, "PP_DRY_RUN": true,
		"PP_LOG_LEVEL": true, "PP_LOG_JSON": true, "PP_COMPAT_WATCHTOWER": true,
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "PP_") && !known[k] {
			warnings = append(warnings, fmt.Sprintf("unknown env var %s (typo?) — ignored", k))
		}
	}

	c := &Config{
		Schedule:         env("PP_SCHEDULE", "0 3 * * *"),
		Timezone:         env("PP_TIMEZONE", env("TZ", "UTC")),
		Jitter:           durOf("PP_JITTER", 30*time.Minute),
		Soak:             durOf("PP_SOAK", 24*time.Hour),
		SelfUpdate:       boolOf("PP_SELF_UPDATE", false),
		Cleanup:          boolOf("PP_CLEANUP", false),
		Webhook:          boolOf("PP_WEBHOOK", false),
		WebhookURL:       env("PP_WEBHOOK_URL", version.DefaultWebhookURL),
		DataDir:          env("PP_DATA_DIR", "/data"),
		NotifyURL:        env("PP_NOTIFY_URL", ""),
		DryRun:           boolOf("PP_DRY_RUN", false),
		LogLevel:         env("PP_LOG_LEVEL", "info"),
		LogJSON:          boolOf("PP_LOG_JSON", false),
		CompatWatchtower: boolOf("PP_COMPAT_WATCHTOWER", false),
		Warnings:         warnings,
	}

	scope := env("PP_SCOPE", "project")
	switch {
	case scope == "project" || scope == "":
		c.Scope = Scope{Mode: "project"}
	case scope == "all":
		c.Scope = Scope{Mode: "all"}
	case strings.HasPrefix(scope, "project:"):
		c.Scope = Scope{Mode: "project", Project: strings.TrimPrefix(scope, "project:")}
	default:
		return nil, fmt.Errorf("invalid PP_SCOPE %q (want project|all|project:<name>)", scope)
	}

	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return nil, fmt.Errorf("invalid PP_TIMEZONE %q: %w", c.Timezone, err)
	}
	return c, nil
}

// Location returns the configured time.Location (validated in Load).
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// LogSummary emits a single redacted boot summary event. Secrets are never logged.
func (c *Config) LogSummary(log zerolog.Logger) {
	scope := c.Scope.Mode
	if c.Scope.Project != "" {
		scope = "project:" + c.Scope.Project
	}
	log.Info().
		Str("version", version.Version).
		Str("commit", version.Commit).
		Str("schedule", c.Schedule).
		Str("timezone", c.Timezone).
		Dur("jitter", c.Jitter).
		Str("scope", scope).
		Dur("soak", c.Soak).
		Bool("self_update", c.SelfUpdate).
		Bool("cleanup", c.Cleanup).
		Bool("webhook", c.Webhook).
		Str("webhook_url", redactURL(c.WebhookURL, c.Webhook)).
		Str("data_dir", c.DataDir).
		Bool("notify", c.NotifyURL != "").
		Bool("dry_run", c.DryRun).
		Msg("pullpilot starting")
	for _, w := range c.Warnings {
		log.Warn().Msg("config: " + w)
	}
}

// redactURL shows the relay host but never a full (secret-bearing) webhook URL.
func redactURL(u string, enabled bool) string {
	if !enabled {
		return "(disabled)"
	}
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return u[:i+3] + rest[:j] + "/…"
		}
	}
	return u
}

// DataDirPersistent reports whether dir appears to be a real mount (bind/volume)
// rather than the container's ephemeral layer. The second return is false when
// it cannot be determined (e.g. /proc is unavailable, as on non-Linux dev hosts).
func DataDirPersistent(dir string) (persistent bool, determinable bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, false
	}
	defer f.Close()

	clean := strings.TrimRight(dir, "/")
	if clean == "" {
		clean = "/"
	}
	s := bufio.NewScanner(f)
	for s.Scan() {
		// mountinfo field 5 (1-indexed) is the mount point.
		fields := strings.Fields(s.Text())
		if len(fields) >= 5 && fields[4] == clean {
			return true, true
		}
	}
	return false, true
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// parseBool reports the parsed value and whether it was a recognized boolean.
func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true, true
	case "0", "false", "no", "off", "n":
		return false, true
	}
	return false, false
}

// parseDuration accepts Go durations ("24h", "30m") and, like the label parser,
// a bare integer as seconds. The second return reports whether it parsed.
func parseDuration(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if d, err := time.ParseDuration(v); err == nil {
		return d, true
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}
