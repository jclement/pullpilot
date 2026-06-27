package registry

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		tag      string
		pinned   bool
	}{
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", false},
		{"nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27", false},
		{"ghcr.io/jclement/pullpilot:v1.2.3", "ghcr.io", "jclement/pullpilot", "v1.2.3", false},
		{"docker.io/library/redis:7", "registry-1.docker.io", "library/redis", "7", false},
		{"quay.io/prometheus/node-exporter:latest", "quay.io", "prometheus/node-exporter", "latest", false},
		{"nginx@sha256:" + zeros(), "registry-1.docker.io", "library/nginx", "", true},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if r.Registry != c.registry || r.Repository != c.repo || r.Tag != c.tag || r.Pinned() != c.pinned {
			t.Errorf("%s: got %+v pinned=%v", c.in, r, r.Pinned())
		}
	}
}

func TestParseChallenge(t *testing.T) {
	ch := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if ch == nil || ch.realm != "https://auth.docker.io/token" || ch.service != "registry.docker.io" {
		t.Fatalf("bad challenge parse: %+v", ch)
	}
	if parseChallenge("Basic realm=x") != nil {
		t.Error("non-bearer challenge should be nil")
	}
}

func zeros() string { return "0000000000000000000000000000000000000000000000000000000000000000" }
