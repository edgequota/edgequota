// Package redis provides a client factory for connecting to Redis in various
// topologies: single, replication (auto-discovery via ROLE), sentinel, and cluster.
// The Client interface is kept minimal — only the operations needed by the
// rate limiter — to simplify testing and keep the coupling surface small.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	goredis "github.com/redis/go-redis/v9"
)

// Client is the interface EdgeQuota needs from Redis.
// go-redis *redis.Client and *redis.ClusterClient both satisfy this.
type Client interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *goredis.StatusCmd
	Ping(ctx context.Context) *goredis.StatusCmd
	Close() error
}

// NewClient creates the appropriate go-redis client for the configured topology.
func NewClient(cfg config.RedisConfig) (Client, error) {
	opts, err := parseOptions(cfg)
	if err != nil {
		return nil, err
	}

	switch opts.mode {
	case config.RedisModeSingle:
		return newSingle(opts)
	case config.RedisModeReplication:
		return newReplication(opts)
	case config.RedisModeSentinel:
		return newSentinel(opts)
	case config.RedisModeCluster:
		return newCluster(opts)
	default:
		return nil, fmt.Errorf("unknown redis mode: %s", opts.mode)
	}
}

// IsNoScriptErr reports whether the error is a NOSCRIPT error from Redis.
func IsNoScriptErr(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT")
}

// IsReadOnlyErr reports whether the error is a READONLY replica error.
func IsReadOnlyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "READONLY")
}

// IsConnectivityErr classifies errors as connectivity-class (unreachable, timeout, EOF).
// READONLY and context.Canceled are NOT connectivity errors.
func IsConnectivityErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	msg := err.Error()
	for _, s := range []string{
		"connection refused", "connection reset", "broken pipe",
		"EOF", "no such host", "no route to host",
		"network is unreachable", "i/o timeout",
		"deadline exceeded", "CLUSTERDOWN", "LOADING",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// Internal options parsing
// ---------------------------------------------------------------------------

type options struct {
	endpoints        []string
	mode             config.RedisMode
	masterName       string
	username         string
	password         string
	db               int
	poolSize         int
	dialTimeout      time.Duration
	readTimeout      time.Duration
	writeTimeout     time.Duration
	tlsEnabled       bool
	tlsSkipVerify    bool
	sentinelUsername string
	sentinelPassword string
}

func parseOptions(cfg config.RedisConfig) (*options, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = config.RedisModeSingle
	}

	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = 10
	}

	dialTimeout, err := parseDur(cfg.DialTimeout, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid dial_timeout: %w", err)
	}

	readTimeout, err := parseDur(cfg.ReadTimeout, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid read_timeout: %w", err)
	}

	writeTimeout, err := parseDur(cfg.WriteTimeout, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid write_timeout: %w", err)
	}

	return &options{
		endpoints:        cfg.Endpoints,
		mode:             mode,
		masterName:       cfg.MasterName,
		username:         cfg.Username,
		password:         cfg.Password.Value(),
		db:               cfg.DB,
		poolSize:         poolSize,
		dialTimeout:      dialTimeout,
		readTimeout:      readTimeout,
		writeTimeout:     writeTimeout,
		tlsEnabled:       cfg.TLS.Enabled,
		tlsSkipVerify:    cfg.TLS.InsecureSkipVerify,
		sentinelUsername: cfg.SentinelUsername,
		sentinelPassword: cfg.SentinelPassword.Value(),
	}, nil
}

func parseDur(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

func makeTLSConfig(opts *options) *tls.Config {
	if !opts.tlsEnabled {
		return nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if opts.tlsSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

// WarnInsecureRedis logs a prominent warning if Redis TLS skip verify is enabled.
// Called at startup from the server package.
func WarnInsecureRedis(cfgTLS config.RedisTLSConfig, logger interface{ Warn(string, ...any) }) {
	if cfgTLS.InsecureSkipVerify {
		logger.Warn("SECURITY WARNING: Redis TLS certificate verification is DISABLED (insecure_skip_verify=true). " +
			"This should NEVER be used in production — it exposes Redis traffic to man-in-the-middle attacks.")
	}
}

// ---------------------------------------------------------------------------
// Single
// ---------------------------------------------------------------------------

func newSingle(opts *options) (Client, error) {
	c := goredis.NewClient(&goredis.Options{
		Addr:         opts.endpoints[0],
		Username:     opts.username,
		Password:     opts.password,
		DB:           opts.db,
		PoolSize:     opts.poolSize,
		DialTimeout:  opts.dialTimeout,
		ReadTimeout:  opts.readTimeout,
		WriteTimeout: opts.writeTimeout,
		TLSConfig:    makeTLSConfig(opts),
	})

	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("single: connect to %s: %w", opts.endpoints[0], err)
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// Sentinel
// ---------------------------------------------------------------------------

func newSentinel(opts *options) (Client, error) {
	c := goredis.NewFailoverClient(&goredis.FailoverOptions{
		MasterName:       opts.masterName,
		SentinelAddrs:    opts.endpoints,
		SentinelUsername: opts.sentinelUsername,
		SentinelPassword: opts.sentinelPassword,
		Username:         opts.username,
		Password:         opts.password,
		DB:               opts.db,
		PoolSize:         opts.poolSize,
		DialTimeout:      opts.dialTimeout,
		ReadTimeout:      opts.readTimeout,
		WriteTimeout:     opts.writeTimeout,
		TLSConfig:        makeTLSConfig(opts),
	})

	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sentinel: connect via %v for master %q: %w", opts.endpoints, opts.masterName, err)
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------

func newCluster(opts *options) (Client, error) {
	c := goredis.NewClusterClient(&goredis.ClusterOptions{
		Addrs:        opts.endpoints,
		Username:     opts.username,
		Password:     opts.password,
		PoolSize:     opts.poolSize,
		DialTimeout:  opts.dialTimeout,
		ReadTimeout:  opts.readTimeout,
		WriteTimeout: opts.writeTimeout,
		TLSConfig:    makeTLSConfig(opts),
	})

	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cluster: connect to seeds %v: %w", opts.endpoints, err)
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// Replication — master discovery via ROLE command
// ---------------------------------------------------------------------------

const masterCacheTTL = 30 * time.Second

// ReplicationClient discovers the primary among configured endpoints by issuing
// ROLE to each and selecting the one that reports "master". Caches the master
// address for 30 seconds. On READONLY, invalidates the cache and retries once.
type ReplicationClient struct {
	opts       *options
	mu         sync.RWMutex
	masterAddr string
	master     *goredis.Client
	lastCheck  time.Time
}

func newReplication(opts *options) (*ReplicationClient, error) {
	rc := &ReplicationClient{opts: opts}
	if err := rc.refreshMaster(); err != nil {
		return nil, fmt.Errorf("replication: initial master discovery: %w", err)
	}
	return rc, nil
}

func (r *ReplicationClient) discoverMaster() (string, error) {
	discoveryTimeout := r.opts.dialTimeout * 2
	if discoveryTimeout <= 0 {
		discoveryTimeout = 2 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	for _, addr := range r.opts.endpoints {
		c := goredis.NewClient(&goredis.Options{
			Addr:         addr,
			Username:     r.opts.username,
			Password:     r.opts.password,
			DB:           r.opts.db,
			DialTimeout:  r.opts.dialTimeout,
			ReadTimeout:  r.opts.readTimeout,
			WriteTimeout: r.opts.writeTimeout,
			TLSConfig:    makeTLSConfig(r.opts),
		})

		result, err := c.Do(ctx, "ROLE").Slice()
		_ = c.Close()

		if err != nil || len(result) < 1 {
			continue
		}

		role := strings.ToLower(fmt.Sprint(result[0]))
		if role == "master" {
			return addr, nil
		}
	}

	return "", fmt.Errorf("no master found among endpoints %v", r.opts.endpoints)
}

func (r *ReplicationClient) getMaster() (*goredis.Client, error) {
	r.mu.RLock()
	if r.master != nil && time.Since(r.lastCheck) < masterCacheTTL {
		c := r.master
		r.mu.RUnlock()
		return c, nil
	}
	r.mu.RUnlock()

	if err := r.refreshMaster(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	c := r.master
	r.mu.RUnlock()
	return c, nil
}

func (r *ReplicationClient) refreshMaster() error {
	// Discover master outside the lock to avoid blocking all requests during
	// N network round-trips. Only take the lock to swap the client.
	addr, err := r.discoverMaster()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if addr != r.masterAddr {
		if r.master != nil {
			_ = r.master.Close()
		}

		r.master = goredis.NewClient(&goredis.Options{
			Addr:         addr,
			Username:     r.opts.username,
			Password:     r.opts.password,
			DB:           r.opts.db,
			PoolSize:     r.opts.poolSize,
			DialTimeout:  r.opts.dialTimeout,
			ReadTimeout:  r.opts.readTimeout,
			WriteTimeout: r.opts.writeTimeout,
			TLSConfig:    makeTLSConfig(r.opts),
		})
		r.masterAddr = addr
	}

	r.lastCheck = time.Now()
	return nil
}

func (r *ReplicationClient) invalidateMaster() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCheck = time.Time{}
}

// Eval implements Client; retries once on READONLY after re-discovering master.
func (r *ReplicationClient) Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	for attempt := 0; attempt < 2; attempt++ {
		master, err := r.getMaster()
		if err != nil {
			cmd := goredis.NewCmd(ctx)
			cmd.SetErr(err)
			return cmd
		}

		cmd := master.Eval(ctx, script, keys, args...)
		if cmd.Err() != nil && IsReadOnlyErr(cmd.Err()) && attempt == 0 {
			r.invalidateMaster()
			continue
		}
		return cmd
	}

	cmd := goredis.NewCmd(ctx)
	cmd.SetErr(fmt.Errorf("replication: READONLY retry exhausted"))
	return cmd
}

// EvalSha implements Client; retries once on READONLY after re-discovering master.
func (r *ReplicationClient) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd {
	for attempt := 0; attempt < 2; attempt++ {
		master, err := r.getMaster()
		if err != nil {
			cmd := goredis.NewCmd(ctx)
			cmd.SetErr(err)
			return cmd
		}

		cmd := master.EvalSha(ctx, sha1, keys, args...)
		if cmd.Err() != nil && IsReadOnlyErr(cmd.Err()) && attempt == 0 {
			r.invalidateMaster()
			continue
		}
		return cmd
	}

	cmd := goredis.NewCmd(ctx)
	cmd.SetErr(fmt.Errorf("replication: READONLY retry exhausted"))
	return cmd
}

// Get implements Client.
func (r *ReplicationClient) Get(ctx context.Context, key string) *goredis.StringCmd {
	master, err := r.getMaster()
	if err != nil {
		cmd := goredis.NewStringCmd(ctx)
		cmd.SetErr(err)
		return cmd
	}
	return master.Get(ctx, key)
}

// Set implements Client.
func (r *ReplicationClient) Set(ctx context.Context, key string, value any, expiration time.Duration) *goredis.StatusCmd {
	for attempt := 0; attempt < 2; attempt++ {
		master, err := r.getMaster()
		if err != nil {
			cmd := goredis.NewStatusCmd(ctx)
			cmd.SetErr(err)
			return cmd
		}

		cmd := master.Set(ctx, key, value, expiration)
		if cmd.Err() != nil && IsReadOnlyErr(cmd.Err()) && attempt == 0 {
			r.invalidateMaster()
			continue
		}
		return cmd
	}

	cmd := goredis.NewStatusCmd(ctx)
	cmd.SetErr(fmt.Errorf("replication: READONLY retry exhausted"))
	return cmd
}

// Ping implements Client.
func (r *ReplicationClient) Ping(ctx context.Context) *goredis.StatusCmd {
	master, err := r.getMaster()
	if err != nil {
		cmd := goredis.NewStatusCmd(ctx)
		cmd.SetErr(err)
		return cmd
	}
	return master.Ping(ctx)
}

// Close implements Client.
func (r *ReplicationClient) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.master != nil {
		return r.master.Close()
	}
	return nil
}
