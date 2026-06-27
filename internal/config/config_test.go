package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"PP_SCHEDULE", "PP_SCOPE", "PP_SOAK", "PP_WEBHOOK", "PP_TIMEZONE", "TZ"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule != "0 3 * * *" {
		t.Errorf("schedule default = %q", c.Schedule)
	}
	if c.Soak != 24*time.Hour {
		t.Errorf("soak default = %v", c.Soak)
	}
	if c.Webhook {
		t.Error("webhook should default off")
	}
	if c.Scope.Mode != "project" {
		t.Errorf("scope default = %q", c.Scope.Mode)
	}
}

func TestScopeParsing(t *testing.T) {
	t.Setenv("PP_SCOPE", "project:homelab")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Scope.Mode != "project" || c.Scope.Project != "homelab" {
		t.Errorf("got %+v", c.Scope)
	}

	t.Setenv("PP_SCOPE", "bogus")
	if _, err := Load(); err == nil {
		t.Error("expected error for invalid scope")
	}
}

func TestUnknownEnvWarns(t *testing.T) {
	t.Setenv("PP_SOOK", "5m") // typo of PP_SOAK
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings {
		if strings.Contains(w, "PP_SOOK") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning about unknown PP_SOOK, got %v", c.Warnings)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://relay.example.workers.dev/v1/poke/wh_secret", true)
	if got != "https://relay.example.workers.dev/…" {
		t.Errorf("redact = %q", got)
	}
	if redactURL("https://x/y", false) != "(disabled)" {
		t.Error("disabled webhook should redact to (disabled)")
	}
}
