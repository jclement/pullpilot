// Package registry resolves the current manifest digest for an image tag from
// its registry, using a cheap manifest HEAD and the standard bearer-token auth
// challenge flow. It never pulls image layers.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// acceptManifest advertises both OCI and Docker, single and index media types.
// Advertising all four avoids registries down-converting and returning the wrong
// Docker-Content-Digest.
var acceptManifest = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Client resolves remote digests.
type Client struct {
	http  *http.Client
	creds *CredStore
}

// New returns a Client. If a Docker config.json is present (DOCKER_CONFIG or
// ~/.docker/config.json), its registry credentials are used; otherwise requests
// are anonymous.
func New() *Client {
	return &Client{
		http:  &http.Client{Timeout: 20 * time.Second},
		creds: loadCredStore(),
	}
}

// Ref is a parsed image reference.
type Ref struct {
	Registry   string // network host, e.g. registry-1.docker.io
	Repository string // e.g. library/nginx
	Tag        string // e.g. latest (empty if pinned by digest)
	Digest     string // sha256:… if the ref is pinned by digest
}

// Pinned reports whether the reference is pinned to an immutable digest.
func (r Ref) Pinned() bool { return r.Digest != "" }

// ParseRef normalizes an image reference (adding docker.io/library and :latest
// as needed) and splits it into registry host, repository, tag and/or digest.
func ParseRef(image string) (Ref, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return Ref{}, fmt.Errorf("parse image %q: %w", image, err)
	}
	r := Ref{
		Registry:   registryHost(reference.Domain(named)),
		Repository: reference.Path(named),
	}
	if c, ok := named.(reference.Canonical); ok {
		r.Digest = c.Digest().String()
	}
	if t, ok := named.(reference.Tagged); ok {
		r.Tag = t.Tag()
	} else if r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// registryHost maps the logical docker.io domain to its actual API endpoint.
func registryHost(domain string) string {
	if domain == "docker.io" || domain == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return domain
}

// RemoteDigest returns the current manifest (or index) digest for the tag of the
// given image reference. For a digest-pinned reference it returns the pin.
func (c *Client) RemoteDigest(ctx context.Context, image string) (string, error) {
	ref, err := ParseRef(image)
	if err != nil {
		return "", err
	}
	if ref.Pinned() {
		return ref.Digest, nil
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, ref.Tag)

	digest, err := c.head(ctx, url, "")
	if err == nil && digest != "" {
		return digest, nil
	}
	// On an auth challenge, mint a token and retry once.
	var ch *challenge
	if ae, ok := err.(*authError); ok {
		ch = ae.challenge
	} else {
		return "", err
	}
	token, err := c.authenticate(ctx, ref, ch)
	if err != nil {
		return "", err
	}
	return c.head(ctx, url, token)
}

// head issues a manifest HEAD and returns the Docker-Content-Digest header.
// A 401 is surfaced as *authError carrying the parsed WWW-Authenticate challenge.
func (c *Client) head(ctx context.Context, url, bearer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", acceptManifest)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		d := resp.Header.Get("Docker-Content-Digest")
		if d == "" {
			return "", fmt.Errorf("registry returned no Docker-Content-Digest for %s", url)
		}
		return d, nil
	case http.StatusUnauthorized:
		if bearer != "" {
			return "", fmt.Errorf("registry denied access (401) to %s", url)
		}
		ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		if ch == nil {
			return "", fmt.Errorf("registry 401 with no usable auth challenge for %s", url)
		}
		return "", &authError{challenge: ch}
	default:
		return "", fmt.Errorf("registry returned %s for %s", resp.Status, url)
	}
}

type authError struct{ challenge *challenge }

func (e *authError) Error() string { return "registry auth required" }

type challenge struct {
	realm   string
	service string
	scope   string
}

// parseChallenge parses a Bearer WWW-Authenticate header.
func parseChallenge(h string) *challenge {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return nil
	}
	ch := &challenge{}
	for _, part := range splitParams(h[len("bearer "):]) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			ch.realm = v
		case "service":
			ch.service = v
		case "scope":
			ch.scope = v
		}
	}
	if ch.realm == "" {
		return nil
	}
	return ch
}

// splitParams splits comma-separated auth params, respecting quoted values.
func splitParams(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

// authenticate exchanges the challenge for a bearer token, using stored
// credentials for the registry when available (else anonymous).
func (c *Client) authenticate(ctx context.Context, ref Ref, ch *challenge) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ch.realm, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	scope := ch.scope
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", ref.Repository)
	}
	q.Set("scope", scope)
	req.URL.RawQuery = q.Encode()

	if user, pass, ok := c.creds.Basic(ref.Registry); ok {
		req.SetBasicAuth(user, pass)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s returned %s", ch.realm, resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// Credentials returns the stored basic-auth username/password and registry host
// for an image, so they can be forwarded to the Docker daemon when it pulls
// (the daemon does not see PullPilot's mounted config.json). ok is false if no
// credentials are configured for that registry.
func (c *Client) Credentials(image string) (user, pass, host string, ok bool) {
	ref, err := ParseRef(image)
	if err != nil {
		return "", "", "", false
	}
	u, p, found := c.creds.Basic(ref.Registry)
	return u, p, ref.Registry, found
}

// CredStore holds registry credentials parsed from a Docker config.json.
type CredStore struct {
	auths map[string]string // registry host -> base64(user:pass)
}

// Basic returns username/password for a registry host, if known.
func (s *CredStore) Basic(host string) (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	for _, key := range []string{host, "https://" + host, "https://" + host + "/v1/", normalizeHubKey(host)} {
		if enc, ok := s.auths[key]; ok && enc != "" {
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				continue
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if ok {
				return user, pass, true
			}
		}
	}
	return "", "", false
}

func normalizeHubKey(host string) string {
	if host == "registry-1.docker.io" {
		return "https://index.docker.io/v1/"
	}
	return host
}

func loadCredStore() *CredStore {
	path := os.Getenv("DOCKER_CONFIG")
	if path != "" {
		path = filepath.Join(path, "config.json")
	} else if home, err := os.UserHomeDir(); err == nil {
		path = filepath.Join(home, ".docker", "config.json")
	}
	store := &CredStore{auths: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return store
	}
	for host, a := range cfg.Auths {
		store.auths[host] = a.Auth
	}
	return store
}
