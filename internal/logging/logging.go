// Package logging configures a zerolog logger. By default it emits colored,
// human-readable console output (readable straight out of `docker logs`); set
// PP_LOG_JSON=true for structured JSON. Colors honor the NO_COLOR convention.
package logging

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New returns a configured logger. level is one of trace/debug/info/warn/error.
// jsonOut forces structured JSON instead of the colored console writer.
func New(level string, jsonOut bool) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	zerolog.TimeFieldFormat = time.RFC3339

	if jsonOut {
		return zerolog.New(os.Stderr).With().Timestamp().Logger()
	}
	w := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
	if os.Getenv("NO_COLOR") != "" {
		w.NoColor = true
	}
	return zerolog.New(w).With().Timestamp().Logger()
}
