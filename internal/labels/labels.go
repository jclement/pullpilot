// Package labels interprets PullPilot's per-container labels (io.pullpilot.*),
// the Compose project labels, and Watchtower compatibility aliases.
package labels

import (
	"strconv"
	"strings"
	"time"
)

const (
	Enable         = "io.pullpilot.enable"
	Exclude        = "io.pullpilot.exclude"
	MonitorOnly    = "io.pullpilot.monitor-only"
	Soak           = "io.pullpilot.soak"
	Self           = "io.pullpilot.self"
	HealthTimeout  = "io.pullpilot.health-timeout"
	StopTimeout    = "io.pullpilot.stop-timeout"
	RemoveAnonVols = "io.pullpilot.remove-anonymous-volumes"
	Order          = "io.pullpilot.order"

	ComposeProject = "com.docker.compose.project"
	ComposeService = "com.docker.compose.service"
	ComposeOneoff  = "com.docker.compose.oneoff"

	wtEnable  = "com.centurylinklabs.watchtower.enable"
	wtMonitor = "com.centurylinklabs.watchtower.monitor-only"
)

// Settings is the resolved per-container behavior.
type Settings struct {
	Enable         *bool          // explicit enable/disable (nil = unset)
	Exclude        bool           // hard exclude
	MonitorOnly    bool           // detect + notify, never update
	Soak           *time.Duration // per-container soak override (nil = use global)
	IsSelf         bool           // PullPilot's own container marker
	HealthTimeout  *time.Duration
	StopTimeout    *time.Duration
	RemoveAnonVols bool
	Order          int
	Project        string
	Service        string
	Oneoff         bool
}

// Parse interprets a container's labels. When compat is true, Watchtower labels
// are honored as aliases.
func Parse(m map[string]string, compat bool) Settings {
	s := Settings{
		Project: m[ComposeProject],
		Service: m[ComposeService],
		Oneoff:  truthy(m[ComposeOneoff]),
	}
	if v, ok := m[Enable]; ok {
		b := truthy(v)
		s.Enable = &b
	}
	s.Exclude = truthy(m[Exclude])
	s.MonitorOnly = truthy(m[MonitorOnly])
	s.IsSelf = truthy(m[Self])
	s.RemoveAnonVols = truthy(m[RemoveAnonVols])
	if d, ok := duration(m[Soak]); ok {
		s.Soak = &d
	}
	if d, ok := duration(m[HealthTimeout]); ok {
		s.HealthTimeout = &d
	}
	if d, ok := duration(m[StopTimeout]); ok {
		s.StopTimeout = &d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(m[Order])); err == nil {
		s.Order = n
	}

	if compat {
		if s.Enable == nil {
			if v, ok := m[wtEnable]; ok {
				b := truthy(v)
				s.Enable = &b
			}
		}
		if !s.MonitorOnly {
			s.MonitorOnly = truthy(m[wtMonitor])
		}
	}
	return s
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

func duration(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, true
	}
	// Bare integer = seconds.
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}
