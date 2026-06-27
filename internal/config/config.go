// Package config loads PullPilot's daemon-wide configuration from PP_* env vars.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jclement/pullpilot/internal/version"
	"github.com/rs/zerolog"
)

// Scope selects which containers PullPilot manages.
type Scope struct {
	Mode    string // "project" | "all" | "project"
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
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Schedule:         env("PP_SCHEDULE", "0 3 * * *"),
		Timezone:         env("PP_TIMEZONE", env("TZ", "UTC")),
		Jitter:           dur("PP_JITTER", 30*time.Minute),
		Soak:             dur("PP_SOAK", 24*time.Hour),
		SelfUpdate:       boolean("PP_SELF_UPDATE", false),
		Cleanup:          boolean("PP_CLEANUP", false),
		Webhook:          boolean("PP_WEBHOOK", false),
		WebhookURL:       env("PP_WEBHOOK_URL", version.DefaultWebhookURL),
		DataDir:          env("PP_DATA_DIR", "/data"),
		NotifyURL:        env("PP_NOTIFY_URL", ""),
		DryRun:           boolean("PP_DRY_RUN", false),
		LogLevel:         env("PP_LOG_LEVEL", "info"),
		LogJSON:          boolean("PP_LOG_JSON", false),
		CompatWatchtower: boolean("PP_COMPAT_WATCHTOWER", false),
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

func boolean(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}

func dur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
