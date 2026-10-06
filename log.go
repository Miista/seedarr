package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter —
// the same colored, human-readable format used by diun elsewhere in this
// stack ("TIME | LEVEL | message key=value ..."), rather than a raw
// structured dump. Level is configurable via LOG_LEVEL since a
// delete-capable tool needs to be able to turn up verbosity live without a
// redeploy.
//
// Log call sites are expected to build a complete, readable sentence via
// fmt.Sprintf and pass it as the message with no additional fields — any
// fields that ARE attached (there are a couple of intentional exceptions,
// e.g. a startup config dump) are appended as trailing key=value pairs by
// ConsoleWriter automatically, not the primary way information is conveyed.
func newLogger(levelStr string) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil {
		level = zerolog.InfoLevel
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// NoColor deliberately left false (colors forced on) even though
		// stdout is normally a pipe, not a real TTY, in a container.
		// `docker logs` and the log viewers actually used against this
		// stack (Dozzle, Portainer, etc.) render ANSI color codes fine —
		// matches diun's own logging in this same stack, which does the
		// same thing.
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}

// withComponent returns a sub-logger tagging every line with a "component"
// field — ConsoleWriter renders this as a trailing component=x, giving the
// same "which subsystem logged this" context slog's logger.With gave
// before, just under zerolog's API.
func withComponent(l zerolog.Logger, component string) zerolog.Logger {
	return l.With().Str("component", component).Logger()
}
