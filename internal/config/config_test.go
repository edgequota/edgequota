package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseEnv is a test helper that applies env overrides to cfg using the same
// mechanism as Load(). It mirrors the EDGEQUOTA_ prefix used in production.
func parseEnv(t *testing.T, cfg *Config) {
	t.Helper()
	require.NoError(t, env.ParseWithOptions(cfg, env.Options{Prefix: "EDGEQUOTA_"}))
}

func TestDefaults(t *testing.T) {
	t.Run("returns non-nil config with sensible defaults", func(t *testing.T) {
		cfg := Defaults()

		assert.Equal(t, ":8080", cfg.Server.Address)
		assert.Equal(t, ":9090", cfg.Admin.Address)
		assert.Equal(t, "30s", cfg.Server.ReadTimeout)
		assert.Equal(t, "30s", cfg.Backend.Timeout)
		assert.Equal(t, 100, cfg.Backend.MaxIdleConns)
		assert.Equal(t, RedisModeSingle, cfg.Redis.Mode)
		assert.Equal(t, []string{"localhost:6379"}, cfg.Redis.Endpoints)
		assert.Equal(t, 10, cfg.Redis.PoolSize)
		assert.Equal(t, KeyStrategyClientIP, cfg.RateLimit.Static.KeyStrategy.Type)
		assert.Equal(t, int64(1), cfg.RateLimit.Static.Burst)
		assert.Equal(t, FailurePolicyPassThrough, cfg.RateLimit.FailurePolicy)
		assert.Equal(t, 429, cfg.RateLimit.FailureCode)
		assert.Equal(t, LogLevelInfo, cfg.Logging.Level)
		assert.Equal(t, LogFormatJSON, cfg.Logging.Format)
		assert.Equal(t, "edgequota", cfg.Tracing.ServiceName)
		assert.Equal(t, 0.1, cfg.Tracing.SampleRate)
	})
}

func TestLoadFromYAML(t *testing.T) {
	t.Run("parses valid YAML file", func(t *testing.T) {
		yamlContent := `
server:
  address: ":9999"
redis:
  endpoints:
    - "redis:6379"
  mode: "single"
rate_limit:
  static:
    backend_url: "http://my-backend:3000"
    average: 200
    burst: 50
    period: "1s"
logging:
  level: "debug"
  format: "text"
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, ":9999", cfg.Server.Address)
		assert.Equal(t, "http://my-backend:3000", cfg.RateLimit.Static.BackendURL)
		assert.Equal(t, int64(200), cfg.RateLimit.Static.Average)
		assert.Equal(t, int64(50), cfg.RateLimit.Static.Burst)
		assert.Equal(t, LogLevelDebug, cfg.Logging.Level)
		assert.Equal(t, LogFormatText, cfg.Logging.Format)
	})

	t.Run("returns error for invalid YAML", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "bad.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte("{{invalid"), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

		_, err := Load()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing config file")
	})

	t.Run("uses defaults when config file does not exist", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent/config.yaml")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://fallback-backend:8080")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, "http://fallback-backend:8080", cfg.RateLimit.Static.BackendURL)
		assert.Equal(t, ":8080", cfg.Server.Address) // default
	})
}

func TestEnvOverrides(t *testing.T) {
	t.Run("env overrides string field", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://default:8080"

		t.Setenv("EDGEQUOTA_SERVER_ADDRESS", ":7777")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://env-backend:9090")

		parseEnv(t, cfg)

		assert.Equal(t, ":7777", cfg.Server.Address)
		assert.Equal(t, "http://env-backend:9090", cfg.RateLimit.Static.BackendURL)
	})

	t.Run("env overrides int field", func(t *testing.T) {
		cfg := Defaults()
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE", "500")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BURST", "100")
		t.Setenv("EDGEQUOTA_BACKEND_MAX_IDLE_CONNS", "50")

		parseEnv(t, cfg)

		assert.Equal(t, int64(500), cfg.RateLimit.Static.Average)
		assert.Equal(t, int64(100), cfg.RateLimit.Static.Burst)
		assert.Equal(t, 50, cfg.Backend.MaxIdleConns)
	})

	t.Run("env overrides bool field", func(t *testing.T) {
		cfg := Defaults()
		t.Setenv("EDGEQUOTA_AUTH_ENABLED", "true")
		t.Setenv("EDGEQUOTA_SERVER_TLS_ENABLED", "true")

		parseEnv(t, cfg)

		assert.True(t, cfg.Auth.Enabled)
		assert.True(t, cfg.Server.TLS.Enabled)
	})

	t.Run("env overrides float64 field", func(t *testing.T) {
		cfg := Defaults()
		t.Setenv("EDGEQUOTA_TRACING_SAMPLE_RATE", "0.5")

		parseEnv(t, cfg)

		assert.Equal(t, 0.5, cfg.Tracing.SampleRate)
	})

	t.Run("env overrides slice field with comma separation", func(t *testing.T) {
		cfg := Defaults()
		t.Setenv("EDGEQUOTA_REDIS_ENDPOINTS", "redis1:6379,redis2:6379,redis3:6379")

		parseEnv(t, cfg)

		assert.Equal(t, []string{"redis1:6379", "redis2:6379", "redis3:6379"}, cfg.Redis.Endpoints)
	})

	t.Run("env vars override YAML values", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "http://yaml-backend:8080"
server:
  address: ":8888"
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)
		t.Setenv("EDGEQUOTA_SERVER_ADDRESS", ":5555")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, ":5555", cfg.Server.Address)                                 // env wins
		assert.Equal(t, "http://yaml-backend:8080", cfg.RateLimit.Static.BackendURL) // YAML preserved
	})

	t.Run("env preserves YAML values when env var is not set", func(t *testing.T) {
		cfg := Defaults()
		cfg.Server.Address = ":1234" // pretend YAML set this

		parseEnv(t, cfg)

		assert.Equal(t, ":1234", cfg.Server.Address) // preserved
	})
}

func TestEnvParseErrors(t *testing.T) {
	t.Run("returns error for invalid int env var", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://backend:8080")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_AVERAGE", "not-a-number")

		_, err := Load()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing environment variables")
	})

	t.Run("returns error for invalid bool env var", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://backend:8080")
		t.Setenv("EDGEQUOTA_AUTH_ENABLED", "not-a-bool")

		_, err := Load()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing environment variables")
	})

	t.Run("returns error for invalid float env var", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://backend:8080")
		t.Setenv("EDGEQUOTA_TRACING_SAMPLE_RATE", "not-a-float")

		_, err := Load()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing environment variables")
	})
}

func TestNormalize(t *testing.T) {
	t.Run("normalizes camelCase YAML values to lowercase", func(t *testing.T) {
		yamlContent := `
rate_limit:
  failure_policy: "passThrough"
  static:
    backend_url: "http://backend:8080"
    key_strategy:
      type: "clientIP"
redis:
  mode: "Single"
logging:
  level: "INFO"
  format: "JSON"
server:
  tls:
    min_version: "TLS1.3"
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, FailurePolicyPassThrough, cfg.RateLimit.FailurePolicy)
		assert.Equal(t, KeyStrategyClientIP, cfg.RateLimit.Static.KeyStrategy.Type)
		assert.Equal(t, RedisModeSingle, cfg.Redis.Mode)
		assert.Equal(t, LogLevelInfo, cfg.Logging.Level)
		assert.Equal(t, LogFormatJSON, cfg.Logging.Format)
		assert.Equal(t, TLSVersion13, cfg.Server.TLS.MinVersion)
	})

	t.Run("normalizes TLS version aliases", func(t *testing.T) {
		for _, input := range []string{"1.2", "tls12", "TLS1.2"} {
			assert.Equal(t, "1.2", normalizeTLSVersion(input), "input: %s", input)
		}
		for _, input := range []string{"1.3", "tls13", "TLS1.3"} {
			assert.Equal(t, "1.3", normalizeTLSVersion(input), "input: %s", input)
		}
	})
}

func TestValidate(t *testing.T) {
	t.Run("valid minimal config", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		assert.NoError(t, Validate(cfg))
	})

	t.Run("missing backend URL without external RL", func(t *testing.T) {
		cfg := Defaults()
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rate_limit.static.backend_url is required when external RL is not enabled")
	})

	t.Run("missing backend URL with external RL enabled", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "http://external-rl:8080/limits"
		cfg.RateLimit.External.Fallback.BackendURL = "http://fallback-backend:8080"
		cfg.RateLimit.External.Fallback.KeyStrategy.Type = KeyStrategyClientIP
		cfg.RateLimit.External.Fallback.Average = 10
		assert.NoError(t, Validate(cfg))
	})

	t.Run("invalid server timeout", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Server.ReadTimeout = "not-a-duration"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "server.read_timeout")
	})

	t.Run("TLS enabled without cert", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Server.TLS.Enabled = true
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cert_file")
	})

	t.Run("HTTP3 enabled without TLS", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Server.TLS.HTTP3Enabled = true
		cfg.Server.TLS.Enabled = false
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "http3_enabled requires server.tls.enabled")
	})

	t.Run("HTTP3 enabled with TLS is valid", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Server.TLS.Enabled = true
		cfg.Server.TLS.CertFile = "/path/to/cert.pem"
		cfg.Server.TLS.KeyFile = "/path/to/key.pem"
		cfg.Server.TLS.HTTP3Enabled = true
		assert.NoError(t, Validate(cfg))
	})

	t.Run("invalid TLS min_version", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Server.TLS.MinVersion = "bogus"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "min_version")
	})

	t.Run("auth enabled without endpoint", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Auth.Enabled = true
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "auth.http.url or auth.grpc.address")
	})

	t.Run("auth enabled with HTTP URL is valid", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = "http://auth:8080/check"
		assert.NoError(t, Validate(cfg))
	})

	t.Run("negative average", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = -1
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "average must be >= 0")
	})

	t.Run("invalid failure policy", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.FailurePolicy = "invalid"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failure_policy")
	})

	t.Run("invalid failure code", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.FailureCode = 200
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failure_code")
	})

	t.Run("header key strategy without header name", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.KeyStrategy.Type = KeyStrategyHeader
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "header_name")
	})

	t.Run("invalid redis mode", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Mode = "invalid"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "redis.mode")
	})

	t.Run("sentinel mode without master name", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Mode = RedisModeSentinel
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "master_name")
	})

	t.Run("single mode with multiple endpoints", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Mode = RedisModeSingle
		cfg.Redis.Endpoints = []string{"redis1:6379", "redis2:6379"}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "single mode requires exactly one endpoint")
	})

	t.Run("replication mode with one endpoint", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Mode = RedisModeReplication
		cfg.Redis.Endpoints = []string{"redis1:6379"}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "replication mode requires at least 2")
	})

	t.Run("empty redis endpoints", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Redis.Endpoints = []string{}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "at least one endpoint")
	})

	t.Run("invalid logging level", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Logging.Level = "trace"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "logging.level")
	})

	t.Run("invalid logging format", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Logging.Format = "xml"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "logging.format")
	})

	t.Run("tracing enabled without endpoint", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.Tracing.Enabled = true
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tracing.endpoint")
	})

	t.Run("external ratelimit enabled without endpoint", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.External.Enabled = true
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rate_limit.external")
	})

	t.Run("global key strategy is valid", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.KeyStrategy.Type = KeyStrategyGlobal
		assert.NoError(t, Validate(cfg))
	})

	t.Run("global key strategy does not require header_name", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.KeyStrategy = KeyStrategyConfig{Type: KeyStrategyGlobal}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("external RL requires fallback key strategy", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "http://limits:8080/limits"
		cfg.RateLimit.External.Fallback.BackendURL = "http://fallback-backend:8080"
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fallback.key_strategy is required")
	})

	t.Run("external RL requires fallback average > 0", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "http://limits:8080/limits"
		cfg.RateLimit.External.Fallback.BackendURL = "http://fallback-backend:8080"
		cfg.RateLimit.External.Fallback.KeyStrategy.Type = KeyStrategyGlobal
		cfg.RateLimit.External.Fallback.Average = 0
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fallback.average must be > 0")
	})

	t.Run("external RL with valid fallback passes", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "http://limits:8080/limits"
		cfg.RateLimit.External.Fallback = FallbackConfig{
			BackendURL: "http://fallback-backend:8080",
			Average:    5000,
			Burst:      2000,
			Period:     "1s",
			KeyStrategy: KeyStrategyConfig{
				Type:      KeyStrategyGlobal,
				GlobalKey: "fe-assets",
			},
		}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("fallback burst defaults to 1 when 0", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "http://limits:8080/limits"
		cfg.RateLimit.External.Fallback = FallbackConfig{
			BackendURL:  "http://fallback-backend:8080",
			Average:     5000,
			Burst:       0,
			Period:      "1s",
			KeyStrategy: KeyStrategyConfig{Type: KeyStrategyGlobal},
		}
		err := Validate(cfg)
		require.NoError(t, err)
		assert.Equal(t, int64(1), cfg.RateLimit.External.Fallback.Burst)
	})

	t.Run("burst defaults to 1 when 0", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Burst = 0
		err := Validate(cfg)
		require.NoError(t, err)
		assert.Equal(t, int64(1), cfg.RateLimit.Static.Burst)
	})
}

func TestParseDuration(t *testing.T) {
	t.Run("parses valid duration", func(t *testing.T) {
		d, err := ParseDuration("5s", 0)
		require.NoError(t, err)
		assert.Equal(t, 5_000_000_000, int(d))
	})

	t.Run("returns default for empty string", func(t *testing.T) {
		d, err := ParseDuration("", 10_000_000_000)
		require.NoError(t, err)
		assert.Equal(t, 10_000_000_000, int(d))
	})

	t.Run("returns error for invalid duration", func(t *testing.T) {
		_, err := ParseDuration("nope", 0)
		assert.Error(t, err)
	})
}

func TestCacheRedisConfig(t *testing.T) {
	t.Run("returns main redis when cache_redis is nil", func(t *testing.T) {
		cfg := Defaults()
		assert.Nil(t, cfg.CacheRedis)

		resolved := cfg.CacheRedisConfig()
		assert.Equal(t, cfg.Redis.Endpoints, resolved.Endpoints)
		assert.Equal(t, cfg.Redis.Mode, resolved.Mode)
	})

	t.Run("returns main redis when cache_redis has no endpoints", func(t *testing.T) {
		cfg := Defaults()
		cfg.CacheRedis = &RedisConfig{Mode: RedisModeSingle}

		resolved := cfg.CacheRedisConfig()
		assert.Equal(t, cfg.Redis.Endpoints, resolved.Endpoints)
	})

	t.Run("returns dedicated config when endpoints are set", func(t *testing.T) {
		cfg := Defaults()
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"cache-redis:6379"},
			Mode:      RedisModeSingle,
			PoolSize:  20,
		}

		resolved := cfg.CacheRedisConfig()
		assert.Equal(t, []string{"cache-redis:6379"}, resolved.Endpoints)
		assert.Equal(t, 20, resolved.PoolSize)
	})
}

func TestParseMaxBodySize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
	}{
		{"empty defaults to 1MB", "", 1 << 20},
		{"explicit 1MB", "1MB", 1 << 20},
		{"2MB", "2MB", 2 << 20},
		{"512KB", "512KB", 512 << 10},
		{"1GB", "1GB", 1 << 30},
		{"plain number (bytes)", "4096", 4096},
		{"case insensitive", "2mb", 2 << 20},
		{"with spaces", " 5 MB ", 5 << 20},
		{"invalid defaults to 1MB", "abc", 1 << 20},
		{"zero defaults to 1MB", "0MB", 1 << 20},
		{"negative defaults to 1MB", "-1MB", 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CacheConfig{MaxBodySize: tt.input}
			assert.Equal(t, tt.expected, cfg.ParseMaxBodySize())
		})
	}
}

func TestResponseCacheRedisConfig(t *testing.T) {
	t.Run("uses dedicated config when present", func(t *testing.T) {
		cfg := &Config{
			Redis:              RedisConfig{Endpoints: []string{"main:6379"}, Mode: RedisModeSingle},
			ResponseCacheRedis: &RedisConfig{Endpoints: []string{"resp-cache:6379"}, Mode: RedisModeSingle},
		}
		rc := cfg.ResponseCacheRedisConfig()
		assert.Equal(t, []string{"resp-cache:6379"}, rc.Endpoints)
	})

	t.Run("falls back to cache_redis when no response_cache_redis", func(t *testing.T) {
		cfg := &Config{
			Redis:      RedisConfig{Endpoints: []string{"main:6379"}, Mode: RedisModeSingle},
			CacheRedis: &RedisConfig{Endpoints: []string{"cache:6379"}, Mode: RedisModeSingle},
		}
		rc := cfg.ResponseCacheRedisConfig()
		assert.Equal(t, []string{"cache:6379"}, rc.Endpoints)
	})

	t.Run("falls back to main redis when no cache_redis", func(t *testing.T) {
		cfg := &Config{
			Redis: RedisConfig{Endpoints: []string{"main:6379"}, Mode: RedisModeSingle},
		}
		rc := cfg.ResponseCacheRedisConfig()
		assert.Equal(t, []string{"main:6379"}, rc.Endpoints)
	})
}

func TestRemovedCacheKeyHeadersAndCacheTTL(t *testing.T) {
	// Loading a config with the old cache_key_headers and cache_ttl fields
	// should simply ignore them (no error). This verifies we don't break
	// on leftover YAML.
	yaml := `
redis:
  endpoints: ["localhost:6379"]
  mode: single
rate_limit:
  static:
    average: 100
    backend_url: "http://backend:8080"
  external:
    enabled: true
    timeout: "5s"
    http:
      url: "http://rl:8080"
    fallback:
      average: 50
      backend_url: "http://backend:8080"
      key_strategy:
        type: clientip
`
	// This should parse without error - old fields are simply not mapped
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(yaml), 0o644))

	t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.RateLimit.External.Enabled)
}

func TestValidateCacheRedis(t *testing.T) {
	t.Run("nil cache_redis passes validation", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		assert.NoError(t, Validate(cfg))
	})

	t.Run("empty endpoints cache_redis passes validation", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("valid single mode cache_redis passes", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"cache:6379"},
			Mode:      RedisModeSingle,
		}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("valid replication mode cache_redis passes", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"cache1:6379", "cache2:6379"},
			Mode:      RedisModeReplication,
		}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("invalid cache_redis mode", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"cache:6379"},
			Mode:      "invalid",
		}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cache_redis.mode")
	})

	t.Run("cache_redis single mode with multiple endpoints", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"c1:6379", "c2:6379"},
			Mode:      RedisModeSingle,
		}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cache_redis.endpoints")
	})

	t.Run("cache_redis sentinel without master_name", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"s1:26379"},
			Mode:      RedisModeSentinel,
		}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cache_redis.master_name")
	})

	t.Run("cache_redis replication with one endpoint", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"c1:6379"},
			Mode:      RedisModeReplication,
		}
		err := Validate(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cache_redis.endpoints")
	})

	t.Run("cache_redis defaults mode to single when not set", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.CacheRedis = &RedisConfig{
			Endpoints: []string{"cache:6379"},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
		assert.Equal(t, RedisModeSingle, cfg.CacheRedis.Mode)
	})
}

func TestCacheRedisYAML(t *testing.T) {
	t.Run("parses cache_redis from YAML", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "http://backend:8080"
redis:
  endpoints:
    - "ratelimit-redis:6379"
  mode: "single"
cache_redis:
  endpoints:
    - "cache-primary:6379"
    - "cache-replica:6379"
  mode: "replication"
  pool_size: 20
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

		cfg, err := Load()
		require.NoError(t, err)

		require.NotNil(t, cfg.CacheRedis)
		assert.Equal(t, []string{"cache-primary:6379", "cache-replica:6379"}, cfg.CacheRedis.Endpoints)
		assert.Equal(t, RedisModeReplication, cfg.CacheRedis.Mode)
		assert.Equal(t, 20, cfg.CacheRedis.PoolSize)

		// Main redis is different.
		assert.Equal(t, []string{"ratelimit-redis:6379"}, cfg.Redis.Endpoints)
		assert.Equal(t, RedisModeSingle, cfg.Redis.Mode)
	})

	t.Run("cache_redis absent in YAML leaves it nil", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "http://backend:8080"
redis:
  endpoints:
    - "redis:6379"
  mode: "single"
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)

		cfg, err := Load()
		require.NoError(t, err)
		assert.Nil(t, cfg.CacheRedis)
	})
}

func TestCacheRedisEnvOverride(t *testing.T) {
	t.Run("env vars create cache_redis when not in YAML", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://backend:8080")
		t.Setenv("EDGEQUOTA_CACHE_REDIS_ENDPOINTS", "env-cache:6379")
		t.Setenv("EDGEQUOTA_CACHE_REDIS_MODE", "single")
		t.Setenv("EDGEQUOTA_CACHE_REDIS_POOL_SIZE", "25")

		cfg, err := Load()
		require.NoError(t, err)

		require.NotNil(t, cfg.CacheRedis)
		assert.Equal(t, []string{"env-cache:6379"}, cfg.CacheRedis.Endpoints)
		assert.Equal(t, RedisModeSingle, cfg.CacheRedis.Mode)
		assert.Equal(t, 25, cfg.CacheRedis.PoolSize)
	})

	t.Run("env vars override YAML cache_redis", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "http://backend:8080"
cache_redis:
  endpoints:
    - "yaml-cache:6379"
  mode: "single"
  pool_size: 10
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)
		t.Setenv("EDGEQUOTA_CACHE_REDIS_POOL_SIZE", "50")

		cfg, err := Load()
		require.NoError(t, err)

		require.NotNil(t, cfg.CacheRedis)
		assert.Equal(t, []string{"yaml-cache:6379"}, cfg.CacheRedis.Endpoints)
		assert.Equal(t, 50, cfg.CacheRedis.PoolSize) // env overrode YAML
	})

	t.Run("no cache_redis env vars leaves pointer nil", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "http://backend:8080")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Nil(t, cfg.CacheRedis)
	})
}

func TestNormalizeURL(t *testing.T) {
	t.Run("adds default port 80 for http", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend"
		err := Validate(cfg)
		require.NoError(t, err)
		assert.Equal(t, "http://backend:80", cfg.RateLimit.Static.BackendURL)
	})

	t.Run("adds default port 443 for https", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "https://backend"
		err := Validate(cfg)
		require.NoError(t, err)
		assert.Equal(t, "https://backend:443", cfg.RateLimit.Static.BackendURL)
	})

	t.Run("preserves explicit port", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:3000"
		err := Validate(cfg)
		require.NoError(t, err)
		assert.Equal(t, "http://backend:3000", cfg.RateLimit.Static.BackendURL)
	})
}

func TestEnumValid(t *testing.T) {
	t.Run("FailurePolicy", func(t *testing.T) {
		assert.True(t, FailurePolicyPassThrough.Valid())
		assert.True(t, FailurePolicyFailClosed.Valid())
		assert.True(t, FailurePolicyInMemoryFallback.Valid())
		assert.False(t, FailurePolicy("bogus").Valid())
	})

	t.Run("KeyStrategyType", func(t *testing.T) {
		assert.True(t, KeyStrategyClientIP.Valid())
		assert.True(t, KeyStrategyHeader.Valid())
		assert.True(t, KeyStrategyComposite.Valid())
		assert.True(t, KeyStrategyGlobal.Valid())
		assert.False(t, KeyStrategyType("bogus").Valid())
	})

	t.Run("RedisMode", func(t *testing.T) {
		assert.True(t, RedisModeSingle.Valid())
		assert.True(t, RedisModeReplication.Valid())
		assert.True(t, RedisModeSentinel.Valid())
		assert.True(t, RedisModeCluster.Valid())
		assert.False(t, RedisMode("bogus").Valid())
	})

	t.Run("LogLevel", func(t *testing.T) {
		assert.True(t, LogLevelDebug.Valid())
		assert.True(t, LogLevelInfo.Valid())
		assert.True(t, LogLevelWarn.Valid())
		assert.True(t, LogLevelError.Valid())
		assert.False(t, LogLevel("trace").Valid())
	})

	t.Run("LogFormat", func(t *testing.T) {
		assert.True(t, LogFormatJSON.Valid())
		assert.True(t, LogFormatText.Valid())
		assert.False(t, LogFormat("xml").Valid())
	})

	t.Run("TLSVersion", func(t *testing.T) {
		assert.True(t, TLSVersion12.Valid())
		assert.True(t, TLSVersion13.Valid())
		assert.True(t, TLSVersion("").Valid()) // empty = use default
		assert.False(t, TLSVersion("1.1").Valid())
	})
}

func TestRedactedString(t *testing.T) {
	secret := RedactedString("super-secret-password")

	t.Run("Value exposes secret", func(t *testing.T) {
		assert.Equal(t, "super-secret-password", secret.Value())
	})

	t.Run("String masks non-empty", func(t *testing.T) {
		assert.Equal(t, "[REDACTED]", secret.String())
	})

	t.Run("String returns empty for empty", func(t *testing.T) {
		assert.Equal(t, "", RedactedString("").String())
	})

	t.Run("GoString masks same as String", func(t *testing.T) {
		assert.Equal(t, "[REDACTED]", secret.GoString())
		assert.Equal(t, "", RedactedString("").GoString())
	})

	t.Run("MarshalJSON masks non-empty", func(t *testing.T) {
		data, err := json.Marshal(secret)
		require.NoError(t, err)
		assert.Equal(t, `"[REDACTED]"`, string(data))
	})

	t.Run("MarshalJSON preserves empty", func(t *testing.T) {
		data, err := json.Marshal(RedactedString(""))
		require.NoError(t, err)
		assert.Equal(t, `""`, string(data))
	})

	t.Run("Sprintf uses String", func(t *testing.T) {
		assert.Equal(t, "[REDACTED]", fmt.Sprintf("%s", secret))
		assert.Equal(t, "[REDACTED]", fmt.Sprintf("%#v", secret))
	})
}

func TestDenyPrivateNetworksEnabled(t *testing.T) {
	t.Run("defaults to true when nil", func(t *testing.T) {
		p := BackendURLPolicy{}
		assert.True(t, p.DenyPrivateNetworksEnabled())
	})

	t.Run("returns explicit true", func(t *testing.T) {
		v := true
		p := BackendURLPolicy{DenyPrivateNetworks: &v}
		assert.True(t, p.DenyPrivateNetworksEnabled())
	})

	t.Run("returns explicit false", func(t *testing.T) {
		v := false
		p := BackendURLPolicy{DenyPrivateNetworks: &v}
		assert.False(t, p.DenyPrivateNetworksEnabled())
	})
}

func TestResolveServerName(t *testing.T) {
	t.Run("explicit server name takes precedence", func(t *testing.T) {
		c := GRPCTLSConfig{ServerName: "override.example.com"}
		assert.Equal(t, "override.example.com", c.ResolveServerName("host:443"))
	})

	t.Run("extracts host from host:port", func(t *testing.T) {
		c := GRPCTLSConfig{}
		assert.Equal(t, "grpc.example.com", c.ResolveServerName("grpc.example.com:443"))
	})

	t.Run("returns bare hostname when no port", func(t *testing.T) {
		c := GRPCTLSConfig{}
		assert.Equal(t, "bare-host", c.ResolveServerName("bare-host"))
	})
}

func TestBuildFilteredHeaderMap(t *testing.T) {
	hf := NewHeaderFilter(HeaderFilterConfig{
		DenyList: []string{"Authorization", "Cookie"},
	})

	h := http.Header{
		"Authorization": {"Bearer token"},
		"Content-Type":  {"application/json"},
		"Cookie":        {"session=abc"},
		"X-Request-Id":  {"123"},
	}

	got := hf.BuildFilteredHeaderMap(h)
	assert.Equal(t, "application/json", got["Content-Type"])
	assert.Equal(t, "123", got["X-Request-Id"])
	assert.NotContains(t, got, "Authorization")
	assert.NotContains(t, got, "Cookie")
}

func TestMustParseDuration(t *testing.T) {
	t.Run("parses valid duration", func(t *testing.T) {
		assert.Equal(t, 5e9, float64(MustParseDuration("5s", 0)))
	})

	t.Run("returns default on empty", func(t *testing.T) {
		assert.Equal(t, 10e9, float64(MustParseDuration("", 10e9)))
	})

	t.Run("returns default on invalid", func(t *testing.T) {
		assert.Equal(t, 3e9, float64(MustParseDuration("not-a-duration", 3e9)))
	})
}

func TestRequiresRestart(t *testing.T) {
	base := &Config{
		Server: ServerConfig{Address: ":8080", TLS: ServerTLSConfig{Enabled: false}},
		Admin:  AdminConfig{Address: ":9090"},
		Redis:  RedisConfig{Mode: RedisModeSingle},
	}

	t.Run("nil old returns nil", func(t *testing.T) {
		cfg := &Config{}
		assert.Nil(t, cfg.RequiresRestart(nil))
	})

	t.Run("identical configs require no restart", func(t *testing.T) {
		same := *base
		assert.Empty(t, base.RequiresRestart(&same))
	})

	t.Run("server address change", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Server.Address = ":8081"
		fields := cfg.RequiresRestart(&old)
		assert.Contains(t, fields, "server.address")
	})

	t.Run("admin address change", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Admin.Address = ":9091"
		fields := cfg.RequiresRestart(&old)
		assert.Contains(t, fields, "admin.address")
	})

	t.Run("redis mode change", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Redis.Mode = RedisModeCluster
		fields := cfg.RequiresRestart(&old)
		assert.Contains(t, fields, "redis.mode")
	})

	t.Run("TLS enabled change", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Server.TLS.Enabled = true
		fields := cfg.RequiresRestart(&old)
		assert.Contains(t, fields, "server.tls.enabled")
	})

	t.Run("HTTP3 enabled change", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Server.TLS.HTTP3Enabled = true
		fields := cfg.RequiresRestart(&old)
		assert.Contains(t, fields, "server.tls.http3_enabled")
	})

	t.Run("multiple changes reported", func(t *testing.T) {
		old := *base
		cfg := *base
		cfg.Server.Address = ":9999"
		cfg.Redis.Mode = RedisModeSentinel
		fields := cfg.RequiresRestart(&old)
		assert.Len(t, fields, 2)
	})
}

func TestAccessLogEnabled(t *testing.T) {
	t.Run("defaults to true when nil", func(t *testing.T) {
		l := LoggingConfig{}
		assert.True(t, l.AccessLogEnabled())
	})

	t.Run("returns explicit true", func(t *testing.T) {
		v := true
		l := LoggingConfig{AccessLog: &v}
		assert.True(t, l.AccessLogEnabled())
	})

	t.Run("returns explicit false", func(t *testing.T) {
		v := false
		l := LoggingConfig{AccessLog: &v}
		assert.False(t, l.AccessLogEnabled())
	})
}

func TestBackendProtocol(t *testing.T) {
	t.Run("valid values pass validation", func(t *testing.T) {
		for _, v := range []string{"", "auto", "h1", "h2", "h3"} {
			tc := TransportConfig{BackendProtocol: v}
			assert.NoError(t, tc.ValidateBackendProtocol(), "value: %q", v)
		}
	})

	t.Run("invalid value rejected", func(t *testing.T) {
		tc := TransportConfig{BackendProtocol: "h4"}
		assert.Error(t, tc.ValidateBackendProtocol())
	})

	t.Run("resolved normalizes empty to auto", func(t *testing.T) {
		assert.Equal(t, "auto", TransportConfig{}.ResolvedBackendProtocol())
		assert.Equal(t, "h2", TransportConfig{BackendProtocol: "h2"}.ResolvedBackendProtocol())
	})
}

func TestH3UDPBufferConfig(t *testing.T) {
	t.Run("defaults to zero", func(t *testing.T) {
		tc := TransportConfig{}
		assert.Equal(t, 0, tc.H3UDPReceiveBufferSize)
		assert.Equal(t, 0, tc.H3UDPSendBufferSize)
	})

	t.Run("parses from YAML", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "https://backend:443"
backend:
  transport:
    backend_protocol: "h3"
    h3_udp_receive_buffer_size: 7500000
    h3_udp_send_buffer_size: 7500000
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)
		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "h3", cfg.Backend.Transport.BackendProtocol)
		assert.Equal(t, 7500000, cfg.Backend.Transport.H3UDPReceiveBufferSize)
		assert.Equal(t, 7500000, cfg.Backend.Transport.H3UDPSendBufferSize)
	})

	t.Run("env overrides YAML", func(t *testing.T) {
		yamlContent := `
rate_limit:
  static:
    backend_url: "https://backend:443"
backend:
  transport:
    h3_udp_receive_buffer_size: 1000
    h3_udp_send_buffer_size: 2000
`
		tmpDir := t.TempDir()
		cfgFile := filepath.Join(tmpDir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgFile, []byte(yamlContent), 0o644))

		t.Setenv("EDGEQUOTA_CONFIG_FILE", cfgFile)
		t.Setenv("EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_RECEIVE_BUFFER_SIZE", "8000000")
		t.Setenv("EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_SEND_BUFFER_SIZE", "9000000")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, 8000000, cfg.Backend.Transport.H3UDPReceiveBufferSize)
		assert.Equal(t, 9000000, cfg.Backend.Transport.H3UDPSendBufferSize)
	})

	t.Run("env sets buffer sizes without YAML", func(t *testing.T) {
		t.Setenv("EDGEQUOTA_CONFIG_FILE", "/nonexistent/config.yaml")
		t.Setenv("EDGEQUOTA_RATE_LIMIT_STATIC_BACKEND_URL", "https://backend:443")
		t.Setenv("EDGEQUOTA_BACKEND_TRANSPORT_H3_UDP_RECEIVE_BUFFER_SIZE", "7340032")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, 7340032, cfg.Backend.Transport.H3UDPReceiveBufferSize)
		assert.Equal(t, 0, cfg.Backend.Transport.H3UDPSendBufferSize)
	})

	t.Run("zero values are valid and mean use quic-go defaults", func(t *testing.T) {
		tc := TransportConfig{
			BackendProtocol:        "h3",
			H3UDPReceiveBufferSize: 0,
			H3UDPSendBufferSize:    0,
		}
		assert.NoError(t, tc.ValidateBackendProtocol())
	})

	t.Run("round-trips through JSON for API serialization", func(t *testing.T) {
		tc := TransportConfig{
			BackendProtocol:        "h3",
			H3UDPReceiveBufferSize: 7500000,
			H3UDPSendBufferSize:    7500000,
		}
		data, err := json.Marshal(tc)
		require.NoError(t, err)

		var decoded TransportConfig
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Equal(t, tc.H3UDPReceiveBufferSize, decoded.H3UDPReceiveBufferSize)
		assert.Equal(t, tc.H3UDPSendBufferSize, decoded.H3UDPSendBufferSize)
	})
}

func TestSanitizedConfig(t *testing.T) {
	t.Run("omits Redis endpoints and auth URLs", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "https://api.backend.internal:443/v1"
		cfg.Redis.Endpoints = []string{"redis-0:6379", "redis-1:6379"}
		cfg.Redis.Password = RedactedString("s3cret")
		cfg.Auth.Enabled = true
		cfg.Auth.HTTP.URL = "https://auth.internal/check"
		cfg.RateLimit.Static.Average = 500
		cfg.RateLimit.External.Enabled = true
		cfg.RateLimit.External.HTTP.URL = "https://rl.internal/limits"

		s := cfg.Sanitized()

		assert.Equal(t, int64(500), s.RateLimit.Average)
		assert.True(t, s.RateLimit.ExternalEnabled)

		data, err := json.Marshal(s)
		require.NoError(t, err)
		raw := string(data)
		assert.NotContains(t, raw, "redis-0")
		assert.NotContains(t, raw, "s3cret")
		assert.NotContains(t, raw, "auth.internal")
		assert.NotContains(t, raw, "rl.internal")
	})

	t.Run("strips backend URL to scheme://host", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "https://my-api.example.com:8443/api/v2"

		_ = cfg.Sanitized()
	})

	t.Run("preserves operational fields", func(t *testing.T) {
		cfg := Defaults()
		cfg.RateLimit.Static.BackendURL = "http://backend:8080"
		cfg.RateLimit.Static.Average = 100
		cfg.RateLimit.Static.Burst = 50
		cfg.RateLimit.Static.Period = "1s"
		cfg.RateLimit.FailurePolicy = FailurePolicyInMemoryFallback
		cfg.Logging.Level = "debug"

		s := cfg.Sanitized()
		assert.Equal(t, int64(100), s.RateLimit.Average)
		assert.Equal(t, int64(50), s.RateLimit.Burst)
		assert.Equal(t, "1s", s.RateLimit.Period)
		assert.Equal(t, FailurePolicyInMemoryFallback, s.RateLimit.FailurePolicy)
		assert.Equal(t, LogLevel("debug"), s.Logging.Level)
	})
}

func TestBackendConfigEqual(t *testing.T) {
	base := func() BackendConfig {
		return BackendConfig{
			Timeout:            "30s",
			MaxIdleConns:       100,
			IdleConnTimeout:    "90s",
			MaxRequestBodySize: 1 << 20,
			Transport:          TransportConfig{BackendProtocol: "auto"},
		}
	}

	t.Run("identical configs are equal", func(t *testing.T) {
		a, b := base(), base()
		assert.True(t, a.Equal(b))
	})

	t.Run("timeout difference", func(t *testing.T) {
		a, b := base(), base()
		b.Timeout = "60s"
		assert.False(t, a.Equal(b))
	})

	t.Run("max_idle_conns difference", func(t *testing.T) {
		a, b := base(), base()
		b.MaxIdleConns = 200
		assert.False(t, a.Equal(b))
	})

	t.Run("tls_insecure_verify difference", func(t *testing.T) {
		a, b := base(), base()
		b.TLSInsecureVerify = true
		assert.False(t, a.Equal(b))
	})

	t.Run("transport backend_protocol difference", func(t *testing.T) {
		a, b := base(), base()
		b.Transport.BackendProtocol = "h2"
		assert.False(t, a.Equal(b))
	})

	t.Run("URLPolicy deny_private_networks difference", func(t *testing.T) {
		a, b := base(), base()
		deny := false
		b.URLPolicy.DenyPrivateNetworks = &deny
		assert.False(t, a.Equal(b))
	})

	t.Run("URLPolicy allowed_schemes difference", func(t *testing.T) {
		a, b := base(), base()
		a.URLPolicy.AllowedSchemes = []string{"http"}
		b.URLPolicy.AllowedSchemes = []string{"http", "https"}
		assert.False(t, a.Equal(b))
	})

	t.Run("URLPolicy allowed_hosts difference", func(t *testing.T) {
		a, b := base(), base()
		a.URLPolicy.AllowedHosts = []string{"a.com"}
		b.URLPolicy.AllowedHosts = []string{"b.com"}
		assert.False(t, a.Equal(b))
	})

	t.Run("same allowed_hosts are equal", func(t *testing.T) {
		a, b := base(), base()
		a.URLPolicy.AllowedHosts = []string{"a.com", "b.com"}
		b.URLPolicy.AllowedHosts = []string{"a.com", "b.com"}
		assert.True(t, a.Equal(b))
	})
}

func TestValidateEventsHeaders(t *testing.T) {
	base := func() *Config {
		c := Defaults()
		c.RateLimit.Static.BackendURL = "http://backend:8080"
		c.Events.Enabled = true
		c.Events.HTTP.URL = "http://events-receiver:9000"
		return c
	}

	t.Run("accepts valid custom headers", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Authorization": "Bearer tok",
			"X-Api-Key":     "key-123",
			"X-Destination": "analytics",
		}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("rejects Host header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Host": "evil.com",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Host")
		assert.Contains(t, err.Error(), "restricted")
	})

	t.Run("rejects Content-Length header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Content-Length": "999",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Content-Length")
	})

	t.Run("rejects Content-Type header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Content-Type": "text/xml",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Content-Type")
	})

	t.Run("rejects Connection header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Connection": "close",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Connection")
	})

	t.Run("rejects Transfer-Encoding header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Transfer-Encoding": "chunked",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Transfer-Encoding")
	})

	t.Run("rejects empty header name", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"": "value",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty header name")
	})

	t.Run("accepts empty headers map", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = nil
		assert.NoError(t, Validate(cfg))
	})

	t.Run("migrates deprecated auth_token to headers", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.AuthToken = "old-tok"
		assert.NoError(t, Validate(cfg))
		assert.Equal(t, "Bearer old-tok", cfg.Events.HTTP.Headers["Authorization"].Value())
	})

	t.Run("migrates deprecated auth_token with custom auth_header", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.AuthToken = "key-val"
		cfg.Events.HTTP.AuthHeader = "X-Api-Key"
		assert.NoError(t, Validate(cfg))
		assert.Equal(t, "Bearer key-val", cfg.Events.HTTP.Headers["X-Api-Key"].Value())
	})

	t.Run("explicit headers take precedence over deprecated auth_token", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.AuthToken = "old-tok"
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"Authorization": "Bearer new-tok",
		}
		assert.NoError(t, Validate(cfg))
		assert.Equal(t, "Bearer new-tok", cfg.Events.HTTP.Headers["Authorization"].Value(),
			"explicit headers must take precedence over deprecated auth_token")
	})

	t.Run("case-insensitive deny-list check", func(t *testing.T) {
		cfg := base()
		cfg.Events.HTTP.Headers = map[string]RedactedString{
			"connection": "keep-alive",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "restricted")
	})
}
