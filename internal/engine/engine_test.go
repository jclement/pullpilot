package engine

import "testing"

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
}

func TestShort(t *testing.T) {
	if short("sha256:0123456789abcdef") != "0123456789ab" {
		t.Errorf("short = %q", short("sha256:0123456789abcdef"))
	}
}
