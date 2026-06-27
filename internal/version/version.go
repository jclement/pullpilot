// Package version holds build metadata, injected at build time via -ldflags.
package version

// These are overridden at build time with:
//
//	-X github.com/jclement/pullpilot/internal/version.Version=...
//
// The DefaultWebhookURL is baked per build channel:
//   - release builds default to the production relay
//   - CI / edge builds override it to the test relay
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"

	// DefaultWebhookURL is the relay used when PP_WEBHOOK is enabled but
	// PP_WEBHOOK_URL is not set. Always overridable via env.
	DefaultWebhookURL = "https://pullpilot-relay.jclement.workers.dev"
)

// String returns a human-readable version line.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
