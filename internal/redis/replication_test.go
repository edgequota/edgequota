package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerRoleAsMaster registers a custom ROLE command handler on a miniredis
// instance so it responds as a Redis master.
func registerRoleAsMaster(mr *miniredis.Miniredis) {
	mr.Server().Register("ROLE", func(c *server.Peer, cmd string, args []string) {
		c.WriteLen(3)
		c.WriteBulk("master")
		c.WriteInt(0)
		c.WriteLen(0) // no replicas
	})
}

// registerRoleAsReplica registers a custom ROLE command handler on a miniredis
// instance so it responds as a Redis replica.
func registerRoleAsReplica(mr *miniredis.Miniredis) {
	mr.Server().Register("ROLE", func(c *server.Peer, cmd string, args []string) {
		c.WriteLen(5)
		c.WriteBulk("slave")
		c.WriteBulk("127.0.0.1")
		c.WriteInt(6379)
		c.WriteBulk("connected")
		c.WriteInt(0)
	})
}

func TestNewReplication(t *testing.T) {
	t.Run("discovers master from single endpoint", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		assert.NotNil(t, rc)
		assert.Equal(t, mr.Addr(), rc.masterAddr)
		require.NoError(t, rc.Close())
	})

	t.Run("discovers master among replicas", func(t *testing.T) {
		replica := miniredis.RunT(t)
		registerRoleAsReplica(replica)

		master := miniredis.RunT(t)
		registerRoleAsMaster(master)

		opts := &options{
			endpoints:    []string{replica.Addr(), master.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		assert.Equal(t, master.Addr(), rc.masterAddr)
		require.NoError(t, rc.Close())
	})

	t.Run("returns error when no master discoverable", func(t *testing.T) {
		opts := &options{
			endpoints:    []string{"127.0.0.1:1", "127.0.0.1:2"},
			dialTimeout:  100 * time.Millisecond,
			readTimeout:  100 * time.Millisecond,
			writeTimeout: 100 * time.Millisecond,
		}
		_, err := newReplication(opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "initial master discovery")
	})
}

func TestReplicationClientEval(t *testing.T) {
	t.Run("runs eval on discovered master", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		cmd := rc.Eval(context.Background(), "return 1", []string{})
		require.NoError(t, cmd.Err())
		result, err := cmd.Int()
		require.NoError(t, err)
		assert.Equal(t, 1, result)
	})

	t.Run("returns error when master unavailable", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		cmd := rc.Eval(context.Background(), "return 1", []string{})
		assert.Error(t, cmd.Err())
	})

	t.Run("rediscovers master after invalidation", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		// Invalidate master to force rediscovery on next call.
		rc.invalidateMaster()

		cmd := rc.Eval(context.Background(), "return 1", []string{})
		require.NoError(t, cmd.Err())
		result, err := cmd.Int()
		require.NoError(t, err)
		assert.Equal(t, 1, result)
	})
}

func TestReplicationClientEvalSha(t *testing.T) {
	t.Run("runs evalsha on discovered master", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		// Load a script first to get its SHA.
		script := "return 42"
		sha := rc.Eval(context.Background(), script, []string{})
		require.NoError(t, sha.Err())

		// evalsha with a bad SHA will return NOSCRIPT, but that exercises the code path.
		cmd := rc.EvalSha(context.Background(), "nonexistent-sha", []string{})
		// NOSCRIPT is expected.
		assert.Error(t, cmd.Err())
	})

	t.Run("returns error when master unavailable", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		cmd := rc.EvalSha(context.Background(), "sha1", []string{})
		assert.Error(t, cmd.Err())
	})
}

func TestReplicationClientPing(t *testing.T) {
	t.Run("pings discovered master", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		cmd := rc.Ping(context.Background())
		require.NoError(t, cmd.Err())
		assert.Equal(t, "PONG", cmd.Val())
	})

	t.Run("returns error when master unavailable", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		cmd := rc.Ping(context.Background())
		assert.Error(t, cmd.Err())
	})
}

func TestReplicationClientGetMaster(t *testing.T) {
	t.Run("returns cached master within TTL", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		// First call sets master.
		c1, err := rc.getMaster()
		require.NoError(t, err)
		assert.NotNil(t, c1)

		// Second call should return same cached master.
		c2, err := rc.getMaster()
		require.NoError(t, err)
		assert.Equal(t, c1, c2)
	})

	t.Run("refreshes master when TTL expired", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		// Force expired TTL.
		rc.mu.Lock()
		rc.lastCheck = time.Now().Add(-2 * masterCacheTTL)
		rc.mu.Unlock()

		// Should refresh and still return a valid master.
		c, err := rc.getMaster()
		require.NoError(t, err)
		assert.NotNil(t, c)
	})

	t.Run("returns error when no endpoints reachable", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1", "127.0.0.1:2"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		_, err := rc.getMaster()
		assert.Error(t, err)
	})
}

func TestRefreshMaster(t *testing.T) {
	t.Run("switches master when endpoints change", func(t *testing.T) {
		mr1 := miniredis.RunT(t)
		registerRoleAsMaster(mr1)

		opts := &options{
			endpoints:    []string{mr1.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()
		assert.Equal(t, mr1.Addr(), rc.masterAddr)

		// New master replaces mr1 in endpoints.
		mr2 := miniredis.RunT(t)
		registerRoleAsMaster(mr2)

		rc.opts.endpoints = []string{mr2.Addr()}

		err = rc.refreshMaster()
		require.NoError(t, err)
		assert.Equal(t, mr2.Addr(), rc.masterAddr)
	})

	t.Run("keeps same master when address unchanged", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		defer rc.Close()

		oldMaster := rc.master

		err = rc.refreshMaster()
		require.NoError(t, err)
		// Same master, should not have been replaced.
		assert.Equal(t, oldMaster, rc.master)
	})

	t.Run("returns error when no master discoverable", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		err := rc.refreshMaster()
		assert.Error(t, err)
	})
}

func TestReplicationClientClose(t *testing.T) {
	t.Run("closes master connection", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		opts := &options{
			endpoints:    []string{mr.Addr()},
			dialTimeout:  1 * time.Second,
			readTimeout:  1 * time.Second,
			writeTimeout: 1 * time.Second,
			poolSize:     5,
		}
		rc, err := newReplication(opts)
		require.NoError(t, err)
		assert.NoError(t, rc.Close())
	})

	t.Run("closes without error when no master", func(t *testing.T) {
		rc := &ReplicationClient{opts: &options{}}
		assert.NoError(t, rc.Close())
	})
}

func TestReplicationClientInvalidateMaster(t *testing.T) {
	t.Run("resets lastCheck to zero", func(t *testing.T) {
		rc := &ReplicationClient{
			opts:      &options{},
			lastCheck: time.Now(),
		}
		rc.invalidateMaster()
		assert.True(t, rc.lastCheck.IsZero())
	})
}

func TestDiscoverMaster(t *testing.T) {
	t.Run("finds master endpoint", func(t *testing.T) {
		mr := miniredis.RunT(t)
		registerRoleAsMaster(mr)

		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{mr.Addr()},
				dialTimeout:  1 * time.Second,
				readTimeout:  1 * time.Second,
				writeTimeout: 1 * time.Second,
			},
		}
		addr, err := rc.discoverMaster()
		require.NoError(t, err)
		assert.Equal(t, mr.Addr(), addr)
	})

	t.Run("skips replicas and finds master", func(t *testing.T) {
		replica := miniredis.RunT(t)
		registerRoleAsReplica(replica)

		master := miniredis.RunT(t)
		registerRoleAsMaster(master)

		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{replica.Addr(), master.Addr()},
				dialTimeout:  1 * time.Second,
				readTimeout:  1 * time.Second,
				writeTimeout: 1 * time.Second,
			},
		}
		addr, err := rc.discoverMaster()
		require.NoError(t, err)
		assert.Equal(t, master.Addr(), addr)
	})

	t.Run("returns error for all unreachable endpoints", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1", "127.0.0.1:2"},
				dialTimeout:  100 * time.Millisecond,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		_, err := rc.discoverMaster()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no master found")
	})

	t.Run("handles zero dialTimeout", func(t *testing.T) {
		rc := &ReplicationClient{
			opts: &options{
				endpoints:    []string{"127.0.0.1:1"},
				dialTimeout:  0,
				readTimeout:  100 * time.Millisecond,
				writeTimeout: 100 * time.Millisecond,
			},
		}
		_, err := rc.discoverMaster()
		assert.Error(t, err)
	})
}

func TestNewSentinel(t *testing.T) {
	t.Run("returns error for unreachable sentinels", func(t *testing.T) {
		cfg := config.RedisConfig{
			Endpoints:   []string{"127.0.0.1:1"},
			Mode:        config.RedisModeSentinel,
			MasterName:  "mymaster",
			DialTimeout: "100ms",
		}
		_, err := NewClient(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "sentinel")
	})
}
