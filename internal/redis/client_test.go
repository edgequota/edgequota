package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClientSingle(t *testing.T) {
	t.Run("connects to valid single instance", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := config.RedisConfig{
			Endpoints: []string{mr.Addr()},
			Mode:      config.RedisModeSingle,
		}
		client, err := NewClient(cfg)
		require.NoError(t, err)
		defer client.Close()

		assert.NoError(t, client.Ping(context.Background()).Err())
	})

	t.Run("returns error for unreachable address", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints:   []string{"127.0.0.1:1"},
			Mode:        config.RedisModeSingle,
			DialTimeout: "100ms",
		}
		_, err := NewClient(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "single: connect")
	})
}

func TestNewClientCluster(t *testing.T) {
	t.Run("returns error for unreachable cluster", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints:   []string{"127.0.0.1:1", "127.0.0.1:2"},
			Mode:        config.RedisModeCluster,
			DialTimeout: "100ms",
		}
		_, err := NewClient(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cluster: connect")
	})
}

func TestNewClientUnknownMode(t *testing.T) {
	t.Run("returns error for unknown mode", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints: []string{"redis:6379"},
			Mode:      "magic", // deliberately invalid
		}
		_, err := NewClient(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown redis mode")
	})
}

func TestParseOptions(t *testing.T) {
	t.Run("applies defaults for empty timeouts", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints: []string{"redis:6379"},
			Mode:      config.RedisModeSingle,
		}
		opts, err := parseOptions(cfg)
		require.NoError(t, err)

		assert.Equal(t, 10, opts.poolSize)
		assert.Equal(t, "5s", opts.dialTimeout.String())
		assert.Equal(t, "3s", opts.readTimeout.String())
		assert.Equal(t, "3s", opts.writeTimeout.String())
	})

	t.Run("parses custom timeouts", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints:    []string{"redis:6379"},
			Mode:         config.RedisModeSingle,
			PoolSize:     20,
			DialTimeout:  "10s",
			ReadTimeout:  "5s",
			WriteTimeout: "5s",
		}
		opts, err := parseOptions(cfg)
		require.NoError(t, err)

		assert.Equal(t, 20, opts.poolSize)
		assert.Equal(t, "10s", opts.dialTimeout.String())
	})

	t.Run("returns error for invalid dial timeout", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints:   []string{"redis:6379"},
			Mode:        config.RedisModeSingle,
			DialTimeout: "invalid",
		}
		_, err := parseOptions(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dial_timeout")
	})

	t.Run("defaults mode to single when empty", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints: []string{"redis:6379"},
		}
		opts, err := parseOptions(cfg)
		require.NoError(t, err)
		assert.Equal(t, config.RedisModeSingle, opts.mode)
	})
}

func TestIsNoScriptErr(t *testing.T) {
	t.Run("returns true for NOSCRIPT error", func(t *testing.T) {
		assert.True(t, IsNoScriptErr(fmt.Errorf("NOSCRIPT No matching script")))
	})

	t.Run("returns false for nil", func(t *testing.T) {
		assert.False(t, IsNoScriptErr(nil))
	})

	t.Run("returns false for other errors", func(t *testing.T) {
		assert.False(t, IsNoScriptErr(fmt.Errorf("some other error")))
	})
}

func TestIsReadOnlyErr(t *testing.T) {
	t.Run("returns true for READONLY error", func(t *testing.T) {
		assert.True(t, IsReadOnlyErr(fmt.Errorf("READONLY You can't write against a read only replica")))
	})

	t.Run("returns false for nil", func(t *testing.T) {
		assert.False(t, IsReadOnlyErr(nil))
	})

	t.Run("returns false for other errors", func(t *testing.T) {
		assert.False(t, IsReadOnlyErr(fmt.Errorf("connection refused")))
	})
}

func TestIsConnectivityErr(t *testing.T) {
	t.Run("nil is not connectivity error", func(t *testing.T) {
		assert.False(t, IsConnectivityErr(nil))
	})

	t.Run("context.Canceled is not connectivity error", func(t *testing.T) {
		assert.False(t, IsConnectivityErr(context.Canceled))
	})

	t.Run("context.DeadlineExceeded is connectivity error", func(t *testing.T) {
		assert.True(t, IsConnectivityErr(context.DeadlineExceeded))
	})

	t.Run("connection refused is connectivity error", func(t *testing.T) {
		assert.True(t, IsConnectivityErr(fmt.Errorf("dial tcp: connection refused")))
	})

	t.Run("EOF is connectivity error", func(t *testing.T) {
		assert.True(t, IsConnectivityErr(fmt.Errorf("read tcp: EOF")))
	})

	t.Run("CLUSTERDOWN is connectivity error", func(t *testing.T) {
		assert.True(t, IsConnectivityErr(fmt.Errorf("CLUSTERDOWN The cluster is down")))
	})

	t.Run("LOADING is connectivity error", func(t *testing.T) {
		assert.True(t, IsConnectivityErr(fmt.Errorf("LOADING Redis is loading the dataset in memory")))
	})

	t.Run("net.OpError is connectivity error", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("test")}
		assert.True(t, IsConnectivityErr(err))
	})

	t.Run("READONLY is NOT connectivity error", func(t *testing.T) {
		assert.False(t, IsConnectivityErr(fmt.Errorf("READONLY You can't write")))
	})

	t.Run("random error is not connectivity error", func(t *testing.T) {
		assert.False(t, IsConnectivityErr(fmt.Errorf("some random error")))
	})
}

func TestMakeTLSConfig(t *testing.T) {
	t.Run("returns nil when TLS disabled", func(t *testing.T) {
		opts := &options{tlsEnabled: false}
		assert.Nil(t, makeTLSConfig(opts))
	})

	t.Run("returns config when TLS enabled", func(t *testing.T) {
		opts := &options{tlsEnabled: true, tlsSkipVerify: true}
		cfg := makeTLSConfig(opts)
		require.NotNil(t, cfg)
		assert.True(t, cfg.InsecureSkipVerify)
	})
}
