// Package logging configures a zerolog logger: colored console output on a TTY,
// structured JSON otherwise (systemd / Docker log drivers).
package logging

import (
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

// New returns a configured logger. level is one of trace/debug/info/warn/error.
// forceJSON disables the pretty console writer even on a TTY.
func New(level string, forceJSON bool) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	zerolog.TimeFieldFormat = time.RFC3339

	if !forceJSON && isatty.IsTerminal(os.Stderr.Fd()) {
		w := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
		return zerolog.New(w).With().Timestamp().Logger()
	}
	return zerolog.New(os.Stderr).With().Timestamp().Logger()
}
