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
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/edgequota/edgequota/internal/config"
	goredis "github.com/redis/go-redis/v9"
)

// slogRedisLogger adapts slog.Logger to the go-redis internal.Logging interface.
// go-redis logs connection pool errors, retry attempts, and failover events
// through this adapter instead of the default log.Printf.
type slogRedisLogger struct {
	logger *slog.Logger
}

func (l *slogRedisLogger) Printf(ctx context.Context, format string, v ...interface{}) {
	l.logger.WarnContext(ctx, fmt.Sprintf(format, v...), "component", "go-redis")
}

// InitLogger redirects go-redis internal logs to the given slog.Logger.
// Call once at startup before any Redis client is created.
func InitLogger(logger *slog.Logger) {
	goredis.SetLogger(&slogRedisLogger{logger: logger})
}

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

// NewClient creates the appropriate go-redis client for the configured topology
// and verifies connectivity with an initial Ping.
func NewClient(cfg config.RedisConfig) (Client, error) {
	return newClient(cfg, true)
}

// NewClientWithoutPing creates a Redis client without an initial health check.
// The client is ready to use but may not be connected yet. Used by the
// recovery loop to create a long-lived client that auto-reconnects via
// go-redis internal retries (MaxRetries=-1).
func NewClientWithoutPing(cfg config.RedisConfig) (Client, error) {
	return newClient(cfg, false)
}

func newClient(cfg config.RedisConfig, ping bool) (Client, error) {
	opts, err := parseOptions(cfg)
	if err != nil {
		return nil, err
	}

	var c Client
	var label string

	switch opts.mode {
	case config.RedisModeSingle:
		c = goredis.NewClient(opts.singleOptions())
		label = fmt.Sprintf("single: connect to %s", opts.endpoints[0])
	case config.RedisModeReplication:
		return newReplication(opts)
	case config.RedisModeSentinel:
		c = goredis.NewFailoverClient(opts.failoverOptions())
		label = fmt.Sprintf("sentinel: connect via %v for master %q", opts.endpoints, opts.masterName)
	case config.RedisModeCluster:
		c = goredis.NewClusterClient(opts.clusterOptions())
		label = fmt.Sprintf("cluster: connect to seeds %v", opts.endpoints)
	default:
		return nil, fmt.Errorf("unknown redis mode: %s", opts.mode)
	}

	if ping {
		if err := c.Ping(context.Background()).Err(); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	}

	return c, nil
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
// Internal options parsing and go-redis option builders
// ---------------------------------------------------------------------------

// Retry constants shared by all topologies. go-redis retries transparently
// within each command; -1 means unlimited retries (bounded by the context
// deadline or server timeout).
const (
	defaultMaxRetries      = -1
	defaultMinRetryBackoff = 100 * time.Millisecond
	defaultMaxRetryBackoff = 5 * time.Second
)

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

// singleOptions builds goredis.Options for a single-instance or discovery client.
func (o *options) singleOptions() *goredis.Options {
	return &goredis.Options{
		Addr:            o.endpoints[0],
		Username:        o.username,
		Password:        o.password,
		DB:              o.db,
		PoolSize:        o.poolSize,
		DialTimeout:     o.dialTimeout,
		ReadTimeout:     o.readTimeout,
		WriteTimeout:    o.writeTimeout,
		MaxRetries:      defaultMaxRetries,
		MinRetryBackoff: defaultMinRetryBackoff,
		MaxRetryBackoff: defaultMaxRetryBackoff,
		TLSConfig:       o.tlsConfig(),
	}
}

// singleOptionsForAddr builds goredis.Options for an arbitrary address,
// used by the replication client for master discovery and connection.
func (o *options) singleOptionsForAddr(addr string) *goredis.Options {
	opts := o.singleOptions()
	opts.Addr = addr
	return opts
}

// failoverOptions builds goredis.FailoverOptions for sentinel mode.
func (o *options) failoverOptions() *goredis.FailoverOptions {
	return &goredis.FailoverOptions{
		MasterName:       o.masterName,
		SentinelAddrs:    o.endpoints,
		SentinelUsername: o.sentinelUsername,
		SentinelPassword: o.sentinelPassword,
		Username:         o.username,
		Password:         o.password,
		DB:               o.db,
		PoolSize:         o.poolSize,
		DialTimeout:      o.dialTimeout,
		ReadTimeout:      o.readTimeout,
		WriteTimeout:     o.writeTimeout,
		MaxRetries:       defaultMaxRetries,
		MinRetryBackoff:  defaultMinRetryBackoff,
		MaxRetryBackoff:  defaultMaxRetryBackoff,
		TLSConfig:        o.tlsConfig(),
	}
}

// clusterOptions builds goredis.ClusterOptions for cluster mode.
func (o *options) clusterOptions() *goredis.ClusterOptions {
	return &goredis.ClusterOptions{
		Addrs:           o.endpoints,
		Username:        o.username,
		Password:        o.password,
		PoolSize:        o.poolSize,
		DialTimeout:     o.dialTimeout,
		ReadTimeout:     o.readTimeout,
		WriteTimeout:    o.writeTimeout,
		MaxRetries:      defaultMaxRetries,
		MinRetryBackoff: defaultMinRetryBackoff,
		MaxRetryBackoff: defaultMaxRetryBackoff,
		TLSConfig:       o.tlsConfig(),
	}
}

// tlsConfig returns the TLS configuration, or nil when TLS is disabled.
func (o *options) tlsConfig() *tls.Config {
	if !o.tlsEnabled {
		return nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if o.tlsSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	return cfg
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

// WarnInsecureRedis logs a prominent warning if Redis TLS skip verify is enabled.
// Called at startup from the server package.
func WarnInsecureRedis(cfgTLS config.RedisTLSConfig, logger interface{ Warn(string, ...any) }) {
	if cfgTLS.InsecureSkipVerify {
		logger.Warn("SECURITY WARNING: Redis TLS certificate verification is DISABLED (insecure_skip_verify=true). " +
			"This should NEVER be used in production — it exposes Redis traffic to man-in-the-middle attacks.")
	}
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
		discoveryOpts := r.opts.singleOptionsForAddr(addr)
		discoveryOpts.PoolSize = 1
		discoveryOpts.MaxRetries = 0

		c := goredis.NewClient(discoveryOpts)
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
		r.master = goredis.NewClient(r.opts.singleOptionsForAddr(addr))
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

// withReadOnlyRetry executes fn up to twice, invalidating the master cache and
// retrying once if the first attempt returns a READONLY error.
func (r *ReplicationClient) withReadOnlyRetry(fn func(*goredis.Client) error) error {
	for attempt := 0; attempt < 2; attempt++ {
		master, err := r.getMaster()
		if err != nil {
			return err
		}
		if err = fn(master); err != nil && IsReadOnlyErr(err) && attempt == 0 {
			r.invalidateMaster()
			continue
		}
		return err
	}
	return fmt.Errorf("replication: READONLY retry exhausted")
}

// Eval implements Client; retries once on READONLY after re-discovering master.
func (r *ReplicationClient) Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	var result *goredis.Cmd
	err := r.withReadOnlyRetry(func(master *goredis.Client) error {
		result = master.Eval(ctx, script, keys, args...)
		return result.Err()
	})
	if result == nil {
		result = goredis.NewCmd(ctx)
		result.SetErr(err)
	}
	return result
}

// EvalSha implements Client; retries once on READONLY after re-discovering master.
func (r *ReplicationClient) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd {
	var result *goredis.Cmd
	err := r.withReadOnlyRetry(func(master *goredis.Client) error {
		result = master.EvalSha(ctx, sha1, keys, args...)
		return result.Err()
	})
	if result == nil {
		result = goredis.NewCmd(ctx)
		result.SetErr(err)
	}
	return result
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
	var result *goredis.StatusCmd
	err := r.withReadOnlyRetry(func(master *goredis.Client) error {
		result = master.Set(ctx, key, value, expiration)
		return result.Err()
	})
	if result == nil {
		result = goredis.NewStatusCmd(ctx)
		result.SetErr(err)
	}
	return result
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
