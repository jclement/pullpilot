// Package notify sends human-facing notifications via shoutrrr. With no URL
// configured it is a no-op (events still appear in the logs).
package notify

import (
	"context"

	"github.com/containrrr/shoutrrr"
	"github.com/rs/zerolog"
)

// Notifier sends notifications to a shoutrrr URL.
type Notifier struct {
	url string
	log zerolog.Logger
}

// New returns a Notifier. url may be empty (no-op).
func New(url string, log zerolog.Logger) *Notifier {
	return &Notifier{url: url, log: log}
}

// Notify delivers a single message. Failures are logged, never fatal.
func (n *Notifier) Notify(ctx context.Context, title, body string) {
	if n.url == "" {
		return
	}
	msg := title
	if body != "" {
		msg = title + "\n" + body
	}
	if err := shoutrrr.Send(n.url, msg); err != nil {
		n.log.Warn().Err(err).Msg("notification failed")
	}
}
