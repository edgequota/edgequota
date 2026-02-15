package observability

import (
	"log/slog"
	"os"

	"github.com/edgequota/edgequota/internal/config"
)

// NewLogger creates a structured logger using Go's log/slog.
func NewLogger(level config.LogLevel, format config.LogFormat) *slog.Logger {
	var lvl slog.Level

	switch level {
	case config.LogLevelDebug:
		lvl = slog.LevelDebug
	case config.LogLevelInfo, "":
		lvl = slog.LevelInfo
	case config.LogLevelWarn:
		lvl = slog.LevelWarn
	case config.LogLevelError:
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if format == config.LogFormatText {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
