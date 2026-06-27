// Package notify sends human-facing notifications via shoutrrr. With no URL
// configured it is a no-op (events still appear in the logs). Sends are
// fire-and-forget so a slow notification endpoint never blocks an update cycle.
package notify

import (
	"context"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/router"
	"github.com/containrrr/shoutrrr/pkg/types"
	"github.com/rs/zerolog"
)

// Notifier sends notifications to a shoutrrr URL.
type Notifier struct {
	sender *router.ServiceRouter // nil when disabled
	log    zerolog.Logger
}

// New returns a Notifier. url may be empty (no-op). An unparseable URL disables
// notifications with a warning rather than failing startup.
func New(url string, log zerolog.Logger) *Notifier {
	n := &Notifier{log: log}
	if url == "" {
		return n
	}
	sender, err := shoutrrr.CreateSender(url)
	if err != nil {
		log.Warn().Err(err).Msg("invalid PP_NOTIFY_URL — notifications disabled")
		return n
	}
	n.sender = sender
	return n
}

// Notify delivers a message asynchronously. The title is passed as a shoutrrr
// param so services that support it (Discord embeds, ntfy/Telegram headers) show
// it prominently. Failures are logged, never fatal.
func (n *Notifier) Notify(_ context.Context, title, body string) {
	if n.sender == nil {
		return
	}
	go func() {
		params := types.Params{"title": "PullPilot · " + title}
		for _, err := range n.sender.Send(body, &params) {
			if err != nil {
				n.log.Warn().Err(err).Msg("notification failed")
			}
		}
	}()
}
