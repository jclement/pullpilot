// Package webhook implements the daemon side of the relay: it provisions a
// webhook (registering its ed25519 public key), holds an authenticated
// WebSocket to the relay, and turns content-free "poke" events into debounced
// update triggers. A poke never carries an image — it only asks the daemon to
// run its normal, registry-derived check sooner.
package webhook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mrand "math/rand/v2"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
)

// domain binds signatures to this protocol + relay (must match the Worker).
const domain = "pullpilot-webhook-relay/v1"

// debounce coalesces a burst of pokes into a single trigger.
const debounce = 10 * time.Second

// Client manages the persistent relay connection.
type Client struct {
	baseURL string
	dataDir string
	log     zerolog.Logger
	trigger func(reason string)

	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	webhookID string
	listenURL string

	mu           sync.Mutex
	pendingTimer *time.Timer
}

type registration struct {
	BaseURL   string `json:"base_url"`
	WebhookID string `json:"webhook_id"`
	PokeURL   string `json:"poke_url"`
	ListenURL string `json:"listen_url"`
}

// New loads or creates the keypair, then loads or provisions the webhook.
func New(baseURL, dataDir string, log zerolog.Logger, trigger func(reason string)) (*Client, error) {
	c := &Client{baseURL: strings.TrimRight(baseURL, "/"), dataDir: dataDir, log: log, trigger: trigger}
	if err := c.loadOrCreateKey(); err != nil {
		return nil, err
	}
	if err := c.loadOrProvision(); err != nil {
		return nil, err
	}
	return c, nil
}

// PokeURL returns the public trigger URL (for display).
func (c *Client) PokeURL() string {
	return c.baseURL + "/v1/poke/" + c.webhookID
}

func (c *Client) keyPath() string { return filepath.Join(c.dataDir, "ed25519.key") }
func (c *Client) regPath() string { return filepath.Join(c.dataDir, "webhook.json") }

func (c *Client) loadOrCreateKey() error {
	// Always ensure the data dir exists and is private, even if it was created
	// (possibly world-readable) by the volume mount or an earlier component.
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(c.dataDir, 0o700)

	if data, err := os.ReadFile(c.keyPath()); err == nil && len(data) == ed25519.SeedSize {
		c.priv = ed25519.NewKeyFromSeed(data)
		c.pub = c.priv.Public().(ed25519.PublicKey)
		return nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	if err := os.WriteFile(c.keyPath(), seed, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	c.priv = ed25519.NewKeyFromSeed(seed)
	c.pub = c.priv.Public().(ed25519.PublicKey)
	c.log.Info().Msg("generated new ed25519 webhook identity")
	return nil
}

func (c *Client) loadOrProvision() error {
	if data, err := os.ReadFile(c.regPath()); err == nil {
		var r registration
		if json.Unmarshal(data, &r) == nil && r.WebhookID != "" && r.BaseURL == c.baseURL {
			c.webhookID, c.listenURL = r.WebhookID, r.ListenURL
			c.log.Info().Str("poke_url", c.PokeURL()).Msg("loaded existing webhook")
			return nil
		}
	}
	return c.provision()
}

func (c *Client) provision() error {
	body, _ := json.Marshal(map[string]string{
		"pubkey": base64.RawURLEncoding.EncodeToString(c.pub),
		"label":  "pullpilot",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/provision", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provision returned %s", resp.Status)
	}
	var r registration
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decode provision: %w", err)
	}
	r.BaseURL = c.baseURL
	c.webhookID, c.listenURL = r.WebhookID, r.ListenURL
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(c.regPath(), data, 0o600); err != nil {
		return fmt.Errorf("save webhook: %w", err)
	}
	c.log.Info().Str("poke_url", c.PokeURL()).Msg("provisioned new webhook")
	return nil
}

// Run holds the connection, reconnecting with full-jitter backoff until ctx ends.
func (c *Client) Run(ctx context.Context) {
	defer c.stopPending()
	const base, cap = time.Second, 60 * time.Second
	attempt := 0
	for ctx.Err() == nil {
		authedAt, err := c.connectAndListen(ctx)
		if ctx.Err() != nil {
			return
		}
		if !authedAt.IsZero() && time.Since(authedAt) > 60*time.Second {
			attempt = 0 // stable authed session; reset backoff
		}
		backoff := time.Duration(float64(base) * float64(int64(1)<<min(attempt, 6)))
		if backoff > cap {
			backoff = cap
		}
		sleep := time.Duration(mrand.Int64N(int64(backoff) + 1)) // full jitter
		c.log.Warn().Err(err).Dur("retry_in", sleep.Round(time.Second)).Msg("relay disconnected")
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

type frame struct {
	Type      string `json:"type"`
	V         int    `json:"v,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Exp       int64  `json:"exp,omitempty"`
	Sig       string `json:"sig,omitempty"`
	ID        string `json:"id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// connectAndListen dials, authenticates, and pumps pokes until the connection
// fails. authedAt is the instant auth succeeded (zero if it never did), used by
// Run to reset backoff only after a genuinely stable session.
func (c *Client) connectAndListen(ctx context.Context) (authedAt time.Time, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, c.listenURL, nil)
	cancel()
	if err != nil {
		return time.Time{}, fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(64 * 1024)
	defer conn.CloseNow()

	// Handshake: hello -> auth -> ready.
	hello, err := c.read(ctx, conn)
	if err != nil {
		return time.Time{}, err
	}
	if hello.Type != "hello" {
		return time.Time{}, fmt.Errorf("expected hello, got %q", hello.Type)
	}
	chal, err := base64.RawURLEncoding.DecodeString(hello.Challenge)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad challenge: %w", err)
	}
	msg := append([]byte(domain), []byte(c.webhookID)...)
	msg = append(msg, chal...)
	sig := ed25519.Sign(c.priv, msg)
	if err := c.write(ctx, conn, frame{Type: "auth", Sig: base64.RawURLEncoding.EncodeToString(sig)}); err != nil {
		return time.Time{}, err
	}
	ready, err := c.read(ctx, conn)
	if err != nil {
		return time.Time{}, err
	}
	if ready.Type != "ready" {
		return time.Time{}, fmt.Errorf("auth rejected (got %q)", ready.Type)
	}
	authedAt = time.Now()
	c.log.Info().Str("poke_url", c.PokeURL()).Msg("relay connected")

	// Heartbeat.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go c.heartbeat(hbCtx, conn)

	// Receive loop.
	for {
		f, err := c.read(ctx, conn)
		if err != nil {
			return authedAt, err
		}
		if f.Type == "poke" {
			c.log.Debug().Str("id", f.ID).Str("reason", f.Reason).Msg("poke received")
			c.schedule(f.Reason)
		}
	}
}

func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wc, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wc, websocket.MessageText, []byte("ping"))
			cancel()
			if err != nil {
				// A failed heartbeat write means the link is dead; close now so
				// the receive loop unblocks immediately instead of waiting out
				// the read deadline.
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// schedule debounces pokes into a single trigger.
func (c *Client) schedule(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingTimer != nil {
		c.pendingTimer.Stop()
	}
	r := reason
	if r == "" {
		r = "webhook"
	}
	c.pendingTimer = time.AfterFunc(debounce, func() { c.trigger(r) })
}

// stopPending cancels any debounced trigger (called on shutdown).
func (c *Client) stopPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingTimer != nil {
		c.pendingTimer.Stop()
	}
}

func (c *Client) read(ctx context.Context, conn *websocket.Conn) (frame, error) {
	// Just over two heartbeat intervals: pongs reset this every 30s on a healthy
	// link, so it never trips normally but detects a silently-dropped (NAT) link
	// in ~90s instead of minutes.
	rc, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	typ, data, err := conn.Read(rc)
	if err != nil {
		return frame{}, err
	}
	if typ != websocket.MessageText {
		return frame{}, nil
	}
	if string(data) == "pong" {
		return frame{Type: "pong"}, nil
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return frame{}, fmt.Errorf("bad frame: %w", err)
	}
	return f, nil
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, f frame) error {
	data, _ := json.Marshal(f)
	wc, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wc, websocket.MessageText, data)
}
