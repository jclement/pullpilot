package engine

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	apiregistry "github.com/docker/docker/api/types/registry"
)

func TestEncodeRegistryAuth(t *testing.T) {
	enc, err := encodeRegistryAuth("alice", "ghp_secret", "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("auth is not valid base64url: %v", err)
	}
	var ac apiregistry.AuthConfig
	if err := json.Unmarshal(raw, &ac); err != nil {
		t.Fatalf("auth is not valid JSON: %v", err)
	}
	if ac.Username != "alice" || ac.Password != "ghp_secret" || ac.ServerAddress != "ghcr.io" {
		t.Errorf("decoded auth = %+v", ac)
	}
}

func TestMatchRepoDigest(t *testing.T) {
	dg := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	other := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	if got := matchRepoDigest("nginx:latest", []string{"nginx@" + dg}); got != dg {
		t.Errorf("single repo digest: got %q want %q", got, dg)
	}
	if got := matchRepoDigest("ghcr.io/a/b:1", []string{"docker.io/x/y@" + other, "ghcr.io/a/b@" + dg}); got != dg {
		t.Errorf("multi repo digest: got %q want %q", got, dg)
	}
	if got := matchRepoDigest("nginx", nil); got != "" {
		t.Errorf("empty repo digests should yield empty, got %q", got)
	}
	// Multiple repo digests with no registry/repo match must NOT blindly use [0].
	if got := matchRepoDigest("ghcr.io/a/b:1", []string{"docker.io/x/y@" + other, "quay.io/p/q@" + dg}); got != "" {
		t.Errorf("multi no-match should yield empty (no blind fallback), got %q", got)
	}
}

func TestBuildNetworking(t *testing.T) {
	// Zero networks.
	cfg, extra := buildNetworking(&types.NetworkSettings{})
	if len(cfg.EndpointsConfig) != 0 || len(extra) != 0 {
		t.Fatal("zero networks should produce empty config")
	}

	// Two networks: one attached at create, the other deferred — both preserving
	// aliases and static IP (IPAMConfig).
	ns := &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{
		"anet": {Aliases: []string{"a"}, IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: "10.0.0.5"}},
		"bnet": {Aliases: []string{"b"}},
	}}
	cfg, extra = buildNetworking(ns)
	if len(cfg.EndpointsConfig) != 1 || len(extra) != 1 {
		t.Fatalf("expected 1 primary + 1 extra, got %d + %d", len(cfg.EndpointsConfig), len(extra))
	}
	prim := cfg.EndpointsConfig["anet"] // sorted: anet is primary
	if prim == nil || prim.IPAMConfig == nil || prim.IPAMConfig.IPv4Address != "10.0.0.5" {
		t.Error("primary network must preserve static IP (IPAMConfig)")
	}
	if extra["bnet"] == nil || len(extra["bnet"].Aliases) != 1 {
		t.Error("extra network must preserve aliases")
	}
}

func TestShort(t *testing.T) {
	if short("sha256:0123456789abcdef") != "0123456789ab" {
		t.Errorf("short = %q", short("sha256:0123456789abcdef"))
	}
}
