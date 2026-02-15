package observability

import (
	"testing"

	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestNewLogger(t *testing.T) {
	t.Run("creates JSON logger", func(t *testing.T) {
		l := NewLogger(config.LogLevelInfo, config.LogFormatJSON)
		assert.NotNil(t, l)
	})

	t.Run("creates text logger", func(t *testing.T) {
		l := NewLogger(config.LogLevelDebug, config.LogFormatText)
		assert.NotNil(t, l)
	})

	t.Run("defaults to info level for empty string", func(t *testing.T) {
		l := NewLogger("", config.LogFormatJSON)
		assert.NotNil(t, l)
	})

	t.Run("defaults to info level for unknown level", func(t *testing.T) {
		l := NewLogger("trace", config.LogFormatJSON)
		assert.NotNil(t, l)
	})

	t.Run("defaults to JSON format for unknown format", func(t *testing.T) {
		l := NewLogger(config.LogLevelInfo, "xml")
		assert.NotNil(t, l)
	})

	t.Run("creates warn level logger", func(t *testing.T) {
		l := NewLogger(config.LogLevelWarn, config.LogFormatJSON)
		assert.NotNil(t, l)
	})

	t.Run("creates error level logger", func(t *testing.T) {
		l := NewLogger(config.LogLevelError, config.LogFormatText)
		assert.NotNil(t, l)
	})
}
