// Package config handles loading and validation of EdgeQuota configuration
// from YAML files and environment variables. Environment variables always
// override file-based values. Env var names follow the struct path with an
// EDGEQUOTA_ prefix:
//
//	server.address → EDGEQUOTA_SERVER_ADDRESS
//	rate_limit.key_strategy.header_name → EDGEQUOTA_RATE_LIMIT_KEY_STRATEGY_HEADER_NAME
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

// defaultConfigFile is the default path for the YAML configuration file.
// Override via EDGEQUOTA_CONFIG_FILE environment variable.
const defaultConfigFile = "/etc/edgequota/config.yaml"

// ---------------------------------------------------------------------------
// Enum types — typed string constants replace scattered hard-coded values.
// All canonical forms are lowercase; Load() normalizes before validation.
// ---------------------------------------------------------------------------

// FailurePolicy controls behavior when Redis is unreachable.
type FailurePolicy string

const (
	FailurePolicyPassThrough      FailurePolicy = "passthrough"
	FailurePolicyFailClosed       FailurePolicy = "failclosed"
	FailurePolicyInMemoryFallback FailurePolicy = "inmemoryfallback"
)

func (fp FailurePolicy) Valid() bool {
	switch fp {
	case FailurePolicyPassThrough, FailurePolicyFailClosed, FailurePolicyInMemoryFallback:
		return true
	}
	return false
}

// KeyStrategyType defines how a per-client rate-limit key is derived.
type KeyStrategyType string

const (
	KeyStrategyClientIP  KeyStrategyType = "clientip"
	KeyStrategyHeader    KeyStrategyType = "header"
	KeyStrategyComposite KeyStrategyType = "composite"
)

func (k KeyStrategyType) Valid() bool {
	switch k {
	case KeyStrategyClientIP, KeyStrategyHeader, KeyStrategyComposite:
		return true
	}
	return false
}

// RedisMode identifies the Redis deployment topology.
type RedisMode string

const (
	RedisModeSingle      RedisMode = "single"
	RedisModeReplication RedisMode = "replication"
	RedisModeSentinel    RedisMode = "sentinel"
	RedisModeCluster     RedisMode = "cluster"
)

func (m RedisMode) Valid() bool {
	switch m {
	case RedisModeSingle, RedisModeReplication, RedisModeSentinel, RedisModeCluster:
		return true
	}
	return false
}

// LogLevel controls the minimum severity for structured log output.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

func (l LogLevel) Valid() bool {
	switch l {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	}
	return false
}

// LogFormat selects the structured log encoding.
type LogFormat string

const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

func (f LogFormat) Valid() bool {
	switch f {
	case LogFormatJSON, LogFormatText:
		return true
	}
	return false
}

// TLSVersion selects the minimum TLS protocol version.
type TLSVersion string

const (
	TLSVersion12 TLSVersion = "1.2"
	TLSVersion13 TLSVersion = "1.3"
)

func (v TLSVersion) Valid() bool {
	switch v {
	case TLSVersion12, TLSVersion13, "":
		return true
	}
	return false
}

// AuthFailurePolicy controls behavior when the auth service is unreachable.
type AuthFailurePolicy string

const (
	AuthFailurePolicyFailClosed AuthFailurePolicy = "failclosed"
	AuthFailurePolicyFailOpen   AuthFailurePolicy = "failopen"
)

func (p AuthFailurePolicy) Valid() bool {
	switch p {
	case AuthFailurePolicyFailClosed, AuthFailurePolicyFailOpen, "":
		return true
	}
	return false
}

// Config is the top-level EdgeQuota configuration.
type Config struct {
	Server     ServerConfig    `yaml:"server"      envPrefix:"SERVER_"`
	Admin      AdminConfig     `yaml:"admin"       envPrefix:"ADMIN_"`
	Backend    BackendConfig   `yaml:"backend"     envPrefix:"BACKEND_"`
	Auth       AuthConfig      `yaml:"auth"        envPrefix:"AUTH_"`
	RateLimit  RateLimitConfig `yaml:"rate_limit"  envPrefix:"RATE_LIMIT_"`
	Redis      RedisConfig     `yaml:"redis"       envPrefix:"REDIS_"`
	CacheRedis *RedisConfig    `yaml:"cache_redis" envPrefix:"CACHE_REDIS_"`
	Events     EventsConfig    `yaml:"events"      envPrefix:"EVENTS_"`
	Logging    LoggingConfig   `yaml:"logging"     envPrefix:"LOGGING_"`
	Tracing    TracingConfig   `yaml:"tracing"     envPrefix:"TRACING_"`
}

// EventsConfig holds optional usage event emission settings.
// When enabled, EdgeQuota emits rate-limit decisions as usage events
// to an external HTTP/gRPC service (webhook pattern).
type EventsConfig struct {
	Enabled       bool               `yaml:"enabled"        env:"ENABLED"`
	HTTP          EventsHTTPConfig   `yaml:"http"           envPrefix:"HTTP_"`
	GRPC          EventsGRPCConfig   `yaml:"grpc"           envPrefix:"GRPC_"`
	BatchSize     int                `yaml:"batch_size"     env:"BATCH_SIZE"`
	FlushInterval string             `yaml:"flush_interval" env:"FLUSH_INTERVAL"`
	BufferSize    int                `yaml:"buffer_size"    env:"BUFFER_SIZE"`
	HeaderFilter  HeaderFilterConfig `yaml:"header_filter"  envPrefix:"HEADER_FILTER_"`
}

// EventsHTTPConfig holds HTTP event receiver settings.
type EventsHTTPConfig struct {
	URL string `yaml:"url" env:"URL"`
}

// EventsGRPCConfig holds gRPC event receiver settings.
type EventsGRPCConfig struct {
	Address string        `yaml:"address" env:"ADDRESS"`
	TLS     GRPCTLSConfig `yaml:"tls"     envPrefix:"TLS_"`
}

// ServerConfig holds the main proxy server settings.
type ServerConfig struct {
	Address        string          `yaml:"address"         env:"ADDRESS"`
	ReadTimeout    string          `yaml:"read_timeout"    env:"READ_TIMEOUT"`
	WriteTimeout   string          `yaml:"write_timeout"   env:"WRITE_TIMEOUT"`
	IdleTimeout    string          `yaml:"idle_timeout"    env:"IDLE_TIMEOUT"`
	DrainTimeout   string          `yaml:"drain_timeout"   env:"DRAIN_TIMEOUT"`
	RequestTimeout string          `yaml:"request_timeout" env:"REQUEST_TIMEOUT"`
	TLS            ServerTLSConfig `yaml:"tls"             envPrefix:"TLS_"`

	// MaxWebSocketConnsPerKey limits the number of concurrent WebSocket
	// connections per rate-limit key (client/tenant). 0 means unlimited.
	MaxWebSocketConnsPerKey int64 `yaml:"max_websocket_conns_per_key" env:"MAX_WEBSOCKET_CONNS_PER_KEY"`

	// MaxWebSocketTransferBytes limits the total bytes transferred per
	// direction on a single WebSocket connection. 0 means unlimited.
	MaxWebSocketTransferBytes int64 `yaml:"max_websocket_transfer_bytes" env:"MAX_WEBSOCKET_TRANSFER_BYTES"`

	// WebSocketIdleTimeout closes WebSocket connections that have no data
	// transfer in either direction for this duration. 0 means unlimited.
	// Default: "5m".
	WebSocketIdleTimeout string `yaml:"websocket_idle_timeout" env:"WEBSOCKET_IDLE_TIMEOUT"`
}

// ServerTLSConfig holds optional TLS termination settings.
type ServerTLSConfig struct {
	Enabled      bool       `yaml:"enabled"       env:"ENABLED"`
	CertFile     string     `yaml:"cert_file"     env:"CERT_FILE"`
	KeyFile      string     `yaml:"key_file"      env:"KEY_FILE"`
	HTTP3Enabled bool       `yaml:"http3_enabled" env:"HTTP3_ENABLED"`
	MinVersion   TLSVersion `yaml:"min_version"   env:"MIN_VERSION"`
}

// AdminConfig holds the admin/observability server settings.
type AdminConfig struct {
	Address      string `yaml:"address"       env:"ADDRESS"`
	ReadTimeout  string `yaml:"read_timeout"  env:"READ_TIMEOUT"`
	WriteTimeout string `yaml:"write_timeout" env:"WRITE_TIMEOUT"`
	IdleTimeout  string `yaml:"idle_timeout"  env:"IDLE_TIMEOUT"`
}

// BackendConfig defines the upstream backend to proxy requests to.
type BackendConfig struct {
	URL                string           `yaml:"url"                      env:"URL"`
	Timeout            string           `yaml:"timeout"                  env:"TIMEOUT"`
	MaxIdleConns       int              `yaml:"max_idle_conns"           env:"MAX_IDLE_CONNS"`
	IdleConnTimeout    string           `yaml:"idle_conn_timeout"        env:"IDLE_CONN_TIMEOUT"`
	TLSInsecureVerify  bool             `yaml:"tls_insecure_skip_verify" env:"TLS_INSECURE_SKIP_VERIFY"`
	MaxRequestBodySize int64            `yaml:"max_request_body_size"    env:"MAX_REQUEST_BODY_SIZE"` // bytes; 0=unlimited
	Transport          TransportConfig  `yaml:"transport"                envPrefix:"TRANSPORT_"`
	URLPolicy          BackendURLPolicy `yaml:"url_policy"               envPrefix:"URL_POLICY_"`
}

// BackendURLPolicy controls which dynamic backend URLs (from the external
// rate-limit service) are allowed. Prevents SSRF via backend_url injection.
type BackendURLPolicy struct {
	// AllowedSchemes restricts the URL scheme. Default: ["http", "https"].
	AllowedSchemes []string `yaml:"allowed_schemes" env:"ALLOWED_SCHEMES" envSeparator:","`
	// DenyPrivateNetworks blocks RFC 1918, loopback, link-local, and cloud
	// metadata IPs when true. Default: true.
	DenyPrivateNetworks *bool `yaml:"deny_private_networks" env:"DENY_PRIVATE_NETWORKS"`
	// AllowedHosts is an optional allowlist. When non-empty, only these
	// hosts (exact match, case-insensitive) are permitted.
	AllowedHosts []string `yaml:"allowed_hosts" env:"ALLOWED_HOSTS" envSeparator:","`
}

// DenyPrivateNetworksEnabled returns whether private networks should be blocked.
// Defaults to true when not explicitly configured.
func (p BackendURLPolicy) DenyPrivateNetworksEnabled() bool {
	if p.DenyPrivateNetworks == nil {
		return true
	}
	return *p.DenyPrivateNetworks
}

// TransportConfig holds low-level HTTP transport tuning for the proxy.
type TransportConfig struct {
	DialTimeout           string `yaml:"dial_timeout"            env:"DIAL_TIMEOUT"`
	DialKeepAlive         string `yaml:"dial_keep_alive"         env:"DIAL_KEEP_ALIVE"`
	TLSHandshakeTimeout   string `yaml:"tls_handshake_timeout"   env:"TLS_HANDSHAKE_TIMEOUT"`
	ExpectContinueTimeout string `yaml:"expect_continue_timeout" env:"EXPECT_CONTINUE_TIMEOUT"`
	H2ReadIdleTimeout     string `yaml:"h2_read_idle_timeout"    env:"H2_READ_IDLE_TIMEOUT"`
	H2PingTimeout         string `yaml:"h2_ping_timeout"         env:"H2_PING_TIMEOUT"`
	WebSocketDialTimeout  string `yaml:"websocket_dial_timeout"  env:"WEBSOCKET_DIAL_TIMEOUT"`
}

// HeaderFilterConfig controls which request headers are forwarded to external
// services (auth and rate-limit). When AllowList is non-empty only those
// headers are forwarded. When DenyList is non-empty those headers are stripped.
// If both are empty, DefaultSensitiveHeaders are denied automatically. Header
// names are compared case-insensitively.
type HeaderFilterConfig struct {
	// AllowList, when non-empty, is the exclusive set of headers forwarded.
	// All other headers are dropped. Takes precedence over DenyList.
	AllowList []string `yaml:"allow_list" env:"ALLOW_LIST" envSeparator:","`

	// DenyList is a set of headers that are always stripped before forwarding.
	// Ignored when AllowList is non-empty.
	DenyList []string `yaml:"deny_list" env:"DENY_LIST" envSeparator:","`
}

// DefaultSensitiveHeaders are stripped from requests forwarded to external
// auth and rate-limit services unless explicitly allowed. These represent
// credentials and session data that external services should not receive
// by default.
var DefaultSensitiveHeaders = []string{
	"Authorization",
	"Cookie",
	"Set-Cookie",
	"Proxy-Authorization",
	"Proxy-Authenticate",
	"X-Api-Key",
	"X-Csrf-Token",
	"X-Xsrf-Token",
}

// HeaderFilter is the compiled, ready-to-use form of HeaderFilterConfig.
// Use NewHeaderFilter to construct one.
type HeaderFilter struct {
	allowSet map[string]struct{} // canonical (lower) names; nil = no allow list
	denySet  map[string]struct{} // canonical (lower) names; nil = no deny list
}

// NewHeaderFilter compiles a HeaderFilterConfig into an efficient runtime
// filter. When the config has no explicit allow or deny list, the
// DefaultSensitiveHeaders deny list is applied automatically.
func NewHeaderFilter(cfg HeaderFilterConfig) *HeaderFilter {
	hf := &HeaderFilter{}

	if len(cfg.AllowList) > 0 {
		hf.allowSet = make(map[string]struct{}, len(cfg.AllowList))
		for _, h := range cfg.AllowList {
			hf.allowSet[strings.ToLower(h)] = struct{}{}
		}
		return hf // Allow-list takes precedence; deny list is ignored.
	}

	deny := cfg.DenyList
	if len(deny) == 0 {
		deny = DefaultSensitiveHeaders
	}
	hf.denySet = make(map[string]struct{}, len(deny))
	for _, h := range deny {
		hf.denySet[strings.ToLower(h)] = struct{}{}
	}

	return hf
}

// Allowed returns true if the header name should be forwarded.
func (hf *HeaderFilter) Allowed(name string) bool {
	lower := strings.ToLower(name)

	if hf.allowSet != nil {
		_, ok := hf.allowSet[lower]
		return ok
	}

	if hf.denySet != nil {
		_, denied := hf.denySet[lower]
		return !denied
	}

	return true
}

// FilterHeaders returns a new map containing only the headers that pass the
// filter. The original map is not modified.
func (hf *HeaderFilter) FilterHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if hf.Allowed(k) {
			out[k] = v
		}
	}
	return out
}

// BuildFilteredHeaderMap builds a single-value header map from an
// http.Header, applying the filter in a single pass. This avoids
// building the full map and then deleting entries.
func (hf *HeaderFilter) BuildFilteredHeaderMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 && hf.Allowed(k) {
			m[k] = v[0]
		}
	}
	return m
}

// AuthConfig holds optional external authentication service settings.
type AuthConfig struct {
	Enabled       bool               `yaml:"enabled"        env:"ENABLED"`
	Timeout       string             `yaml:"timeout"        env:"TIMEOUT"`
	FailurePolicy AuthFailurePolicy  `yaml:"failure_policy" env:"FAILURE_POLICY"`
	HTTP          AuthHTTPConfig     `yaml:"http"           envPrefix:"HTTP_"`
	GRPC          AuthGRPCConfig     `yaml:"grpc"           envPrefix:"GRPC_"`
	HeaderFilter  HeaderFilterConfig `yaml:"header_filter"  envPrefix:"HEADER_FILTER_"`

	// PropagateRequestBody controls whether the request body is included in
	// the auth check request. Off by default to avoid buffering overhead.
	// When enabled, the body is read up to MaxAuthBodySize bytes and included
	// in the auth request. The original body is replaced so the proxy can
	// still forward it to the backend.
	PropagateRequestBody bool  `yaml:"propagate_request_body" env:"PROPAGATE_REQUEST_BODY"`
	MaxAuthBodySize      int64 `yaml:"max_auth_body_size"     env:"MAX_AUTH_BODY_SIZE"` // bytes; 0 = use default (64 KiB)

	// CircuitBreaker configures the auth service circuit breaker.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker" envPrefix:"CIRCUIT_BREAKER_"`
}

// CircuitBreakerConfig holds circuit breaker tuning parameters.
type CircuitBreakerConfig struct {
	// Threshold is the number of consecutive failures before opening. 0 uses the default (5).
	Threshold int `yaml:"threshold" env:"THRESHOLD"`
	// ResetTimeout is the duration the circuit stays open before probing. 0 uses the default (30s).
	ResetTimeout string `yaml:"reset_timeout" env:"RESET_TIMEOUT"`
}

// AuthHTTPConfig holds HTTP-based auth backend settings.
type AuthHTTPConfig struct {
	URL                    string `yaml:"url"                      env:"URL"`
	ForwardOriginalHeaders bool   `yaml:"forward_original_headers" env:"FORWARD_ORIGINAL_HEADERS"`
}

// AuthGRPCConfig holds gRPC-based auth backend settings.
type AuthGRPCConfig struct {
	Address string        `yaml:"address" env:"ADDRESS"`
	TLS     GRPCTLSConfig `yaml:"tls"     envPrefix:"TLS_"`
}

// GRPCTLSConfig holds TLS settings for gRPC client connections.
type GRPCTLSConfig struct {
	Enabled    bool   `yaml:"enabled"     env:"ENABLED"`
	CAFile     string `yaml:"ca_file"     env:"CA_FILE"`
	ServerName string `yaml:"server_name" env:"SERVER_NAME"` // Override for TLS server name verification. When empty, the hostname from the gRPC address is used.
}

// ResolveServerName returns the TLS server name to verify. If ServerName is
// explicitly set, it is returned. Otherwise the hostname is extracted from the
// gRPC address (host:port). Returns empty string on parse failure.
func (c GRPCTLSConfig) ResolveServerName(grpcAddress string) string {
	if c.ServerName != "" {
		return c.ServerName
	}
	host, _, err := net.SplitHostPort(grpcAddress)
	if err != nil {
		return grpcAddress // bare hostname without port
	}
	return host
}

// RateLimitConfig holds rate limiting settings.
type RateLimitConfig struct {
	Average       int64             `yaml:"average"        env:"AVERAGE"`
	Burst         int64             `yaml:"burst"          env:"BURST"`
	Period        string            `yaml:"period"         env:"PERIOD"`
	FailurePolicy FailurePolicy     `yaml:"failure_policy" env:"FAILURE_POLICY"`
	FailureCode   int               `yaml:"failure_code"   env:"FAILURE_CODE"`
	KeyPrefix     string            `yaml:"key_prefix"     env:"KEY_PREFIX"`
	KeyStrategy   KeyStrategyConfig `yaml:"key_strategy"   envPrefix:"KEY_STRATEGY_"`
	External      ExternalRLConfig  `yaml:"external"       envPrefix:"EXTERNAL_"`

	// MinTTL sets a floor for the Redis key TTL (e.g. "10s"). When empty,
	// the TTL is computed automatically from the rate and period. Use this
	// knob to reduce EXPIRE churn for high-cardinality keys at high rates.
	MinTTL string `yaml:"min_ttl" env:"MIN_TTL"`

	// MaxTenantLabels caps the number of distinct tenant label values in
	// per-tenant Prometheus metrics to prevent cardinality explosions.
	// 0 uses the default of 1000.
	MaxTenantLabels int `yaml:"max_tenant_labels" env:"MAX_TENANT_LABELS"`

	// MaxRecoveryAttempts limits the number of Redis recovery attempts
	// before giving up. 0 means unlimited (retry forever, the default).
	MaxRecoveryAttempts int `yaml:"max_recovery_attempts" env:"MAX_RECOVERY_ATTEMPTS"`

	// GlobalPassthroughRPS is an optional global in-memory rate limit
	// (requests per second) applied as a safety valve when the failure
	// policy is "passthrough" and Redis is unavailable. 0 means no global
	// limit (all traffic passes through).
	GlobalPassthroughRPS float64 `yaml:"global_passthrough_rps" env:"GLOBAL_PASSTHROUGH_RPS"`
}

// KeyStrategyConfig defines how the per-client rate-limit key is extracted.
type KeyStrategyConfig struct {
	Type       KeyStrategyType `yaml:"type"        env:"TYPE"`
	HeaderName string          `yaml:"header_name" env:"HEADER_NAME"`
	PathPrefix bool            `yaml:"path_prefix" env:"PATH_PREFIX"`

	// TrustedProxies is a list of CIDR ranges whose X-Forwarded-For and
	// X-Real-IP headers are trusted. When empty, proxy headers are always
	// trusted (legacy behavior). When set, proxy headers are only honored
	// when RemoteAddr falls within one of these ranges.
	TrustedProxies []string `yaml:"trusted_proxies" env:"TRUSTED_PROXIES" envSeparator:","`

	// TrustedIPDepth controls which entry in X-Forwarded-For to use when
	// the request arrives through a trusted proxy chain. 0 (default) uses
	// the leftmost (client-provided) entry. A positive value N selects the
	// Nth entry from the right, which is the standard approach for multi-
	// proxy chains (e.g. 1 = last entry added by the nearest trusted proxy).
	TrustedIPDepth int `yaml:"trusted_ip_depth" env:"TRUSTED_IP_DEPTH"`
}

// ExternalRLConfig holds optional external rate limit service settings.
type ExternalRLConfig struct {
	Enabled      bool               `yaml:"enabled"       env:"ENABLED"`
	Timeout      string             `yaml:"timeout"       env:"TIMEOUT"`
	CacheTTL     string             `yaml:"cache_ttl"     env:"CACHE_TTL"`
	HTTP         ExternalHTTPConfig `yaml:"http"          envPrefix:"HTTP_"`
	GRPC         ExternalGRPCConfig `yaml:"grpc"          envPrefix:"GRPC_"`
	HeaderFilter HeaderFilterConfig `yaml:"header_filter" envPrefix:"HEADER_FILTER_"`

	// MaxConcurrentRequests caps the number of simultaneous in-flight
	// requests to the external rate limit service. This protects the
	// control plane from thundering herd on a Redis cache flush. 0 uses
	// the default (50).
	MaxConcurrentRequests int `yaml:"max_concurrent_requests" env:"MAX_CONCURRENT_REQUESTS"`

	// MaxCircuitBreakers caps the number of per-tenant circuit breaker
	// entries. Prevents unbounded memory growth under high-cardinality
	// attacks. 0 uses the default (10000).
	MaxCircuitBreakers int `yaml:"max_circuit_breakers" env:"MAX_CIRCUIT_BREAKERS"`

	// MaxLatency sets the maximum acceptable latency for calls to the
	// external rate limit service. When exceeded, the call is abandoned
	// and stale cache is used if available. 0 or empty disables.
	MaxLatency string `yaml:"max_latency" env:"MAX_LATENCY"`

	// CircuitBreaker configures the per-tenant circuit breaker.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker" envPrefix:"CIRCUIT_BREAKER_"`
}

// ExternalHTTPConfig holds HTTP external rate limit service settings.
type ExternalHTTPConfig struct {
	URL string `yaml:"url" env:"URL"`
}

// ExternalGRPCConfig holds gRPC external rate limit service settings.
type ExternalGRPCConfig struct {
	Address string        `yaml:"address" env:"ADDRESS"`
	TLS     GRPCTLSConfig `yaml:"tls"     envPrefix:"TLS_"`
}

// RedisConfig holds Redis connection and topology settings.
type RedisConfig struct {
	Endpoints        []string       `yaml:"endpoints"         env:"ENDPOINTS" envSeparator:","`
	Mode             RedisMode      `yaml:"mode"              env:"MODE"`
	MasterName       string         `yaml:"master_name"       env:"MASTER_NAME"`
	Username         string         `yaml:"username"          env:"USERNAME"`
	Password         RedactedString `yaml:"password"          env:"PASSWORD"`
	DB               int            `yaml:"db"                env:"DB"`
	PoolSize         int            `yaml:"pool_size"         env:"POOL_SIZE"`
	DialTimeout      string         `yaml:"dial_timeout"      env:"DIAL_TIMEOUT"`
	ReadTimeout      string         `yaml:"read_timeout"      env:"READ_TIMEOUT"`
	WriteTimeout     string         `yaml:"write_timeout"     env:"WRITE_TIMEOUT"`
	TLS              RedisTLSConfig `yaml:"tls"               envPrefix:"TLS_"`
	SentinelUsername string         `yaml:"sentinel_username" env:"SENTINEL_USERNAME"`
	SentinelPassword RedactedString `yaml:"sentinel_password" env:"SENTINEL_PASSWORD"`
}

// RedactedString is a string that masks its value in String(), GoString(), and
// MarshalJSON() to prevent accidental leakage in logs or serialized output.
// Use .Value() to access the underlying secret.
type RedactedString string

const redactedPlaceholder = "[REDACTED]"

// Value returns the underlying secret string.
func (r RedactedString) Value() string { return string(r) }

// String implements fmt.Stringer — always returns a redacted placeholder.
func (r RedactedString) String() string {
	if r == "" {
		return ""
	}
	return redactedPlaceholder
}

// GoString implements fmt.GoStringer for %#v.
func (r RedactedString) GoString() string { return r.String() }

// MarshalJSON masks the value in JSON output. Uses json.Marshal to ensure
// the placeholder is always properly escaped, preventing injection if the
// constant is ever changed.
func (r RedactedString) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte(`""`), nil
	}
	return json.Marshal(redactedPlaceholder)
}

// RedisTLSConfig holds Redis TLS settings.
type RedisTLSConfig struct {
	Enabled            bool `yaml:"enabled"              env:"ENABLED"`
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" env:"INSECURE_SKIP_VERIFY"`
}

// LoggingConfig holds structured logging settings.
type LoggingConfig struct {
	Level  LogLevel  `yaml:"level"  env:"LEVEL"`
	Format LogFormat `yaml:"format" env:"FORMAT"`
}

// TracingConfig holds OpenTelemetry tracing settings.
type TracingConfig struct {
	Enabled     bool    `yaml:"enabled"      env:"ENABLED"`
	Endpoint    string  `yaml:"endpoint"     env:"ENDPOINT"`
	ServiceName string  `yaml:"service_name" env:"SERVICE_NAME"`
	SampleRate  float64 `yaml:"sample_rate"  env:"SAMPLE_RATE"`
}

// Defaults returns a Config populated with sensible default values.
func Defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Address:        ":8080",
			ReadTimeout:    "30s",
			WriteTimeout:   "30s",
			IdleTimeout:    "120s",
			DrainTimeout:   "30s",
			RequestTimeout: "60s",
		},
		Admin: AdminConfig{
			Address:      ":9090",
			ReadTimeout:  "5s",
			WriteTimeout: "10s",
			IdleTimeout:  "30s",
		},
		Backend: BackendConfig{
			Timeout:            "30s",
			MaxIdleConns:       100,
			IdleConnTimeout:    "90s",
			MaxRequestBodySize: 10 << 20, // 10 MiB default
			Transport: TransportConfig{
				DialTimeout:           "30s",
				DialKeepAlive:         "30s",
				TLSHandshakeTimeout:   "10s",
				ExpectContinueTimeout: "1s",
				H2ReadIdleTimeout:     "30s",
				H2PingTimeout:         "15s",
				WebSocketDialTimeout:  "10s",
			},
		},
		Auth: AuthConfig{
			Timeout: "5s",
		},
		RateLimit: RateLimitConfig{
			Burst:         1,
			Period:        "1s",
			FailurePolicy: FailurePolicyPassThrough,
			FailureCode:   429,
			KeyStrategy: KeyStrategyConfig{
				Type: KeyStrategyClientIP,
			},
			External: ExternalRLConfig{
				Timeout:  "5s",
				CacheTTL: "60s",
			},
		},
		Redis: RedisConfig{
			Endpoints:    []string{"localhost:6379"},
			Mode:         RedisModeSingle,
			PoolSize:     10,
			DialTimeout:  "5s",
			ReadTimeout:  "3s",
			WriteTimeout: "3s",
		},
		Logging: LoggingConfig{
			Level:  LogLevelInfo,
			Format: LogFormatJSON,
		},
		Tracing: TracingConfig{
			ServiceName: "edgequota",
			SampleRate:  0.1,
		},
	}
}

// ConfigFilePath returns the resolved config file path (from env or default).
func ConfigFilePath() string {
	configFile := os.Getenv("EDGEQUOTA_CONFIG_FILE")
	if configFile == "" {
		configFile = defaultConfigFile
	}
	return configFile
}

// Load reads configuration from a YAML file and overlays environment variable
// overrides. The config file path defaults to /etc/edgequota/config.yaml and
// can be overridden via EDGEQUOTA_CONFIG_FILE.
func Load() (*Config, error) {
	return LoadFromPath(ConfigFilePath())
}

// LoadFromPath reads configuration from the given YAML file and overlays
// environment variable overrides. Used by the config watcher to reload.
func LoadFromPath(configFile string) (*Config, error) {
	cfg := Defaults()

	data, err := os.ReadFile(configFile) // config file path is intentionally user-provided.
	if err == nil {
		if yamlErr := yaml.Unmarshal(data, cfg); yamlErr != nil {
			return nil, fmt.Errorf("parsing config file %s: %w", configFile, yamlErr)
		}
	}
	// If the file doesn't exist, we continue with defaults + env overrides.

	// Pre-allocate CacheRedis so the env parser can populate it. If no
	// CACHE_REDIS_ env vars are set the pointer is reset to nil below.
	cacheRedisPresentInYAML := cfg.CacheRedis != nil
	if cfg.CacheRedis == nil {
		cfg.CacheRedis = &RedisConfig{}
	}

	if envErr := env.ParseWithOptions(cfg, env.Options{Prefix: "EDGEQUOTA_"}); envErr != nil {
		return nil, fmt.Errorf("parsing environment variables: %w", envErr)
	}

	// A CacheRedis with no endpoints is meaningless — reset to nil so
	// CacheRedisConfig() falls back to the main redis section.
	if len(cfg.CacheRedis.Endpoints) == 0 && !cacheRedisPresentInYAML {
		cfg.CacheRedis = nil
	}

	cfg.normalize()

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// normalize lowercases all enum fields so that YAML values like "passThrough"
// or env values like "PASSTHROUGH" match the canonical lowercase constants.
func (cfg *Config) normalize() {
	cfg.RateLimit.FailurePolicy = FailurePolicy(strings.ToLower(string(cfg.RateLimit.FailurePolicy)))
	cfg.RateLimit.KeyStrategy.Type = KeyStrategyType(strings.ToLower(string(cfg.RateLimit.KeyStrategy.Type)))
	cfg.Redis.Mode = RedisMode(strings.ToLower(string(cfg.Redis.Mode)))
	if cfg.CacheRedis != nil {
		cfg.CacheRedis.Mode = RedisMode(strings.ToLower(string(cfg.CacheRedis.Mode)))
	}
	cfg.Auth.FailurePolicy = AuthFailurePolicy(strings.ToLower(string(cfg.Auth.FailurePolicy)))
	cfg.Logging.Level = LogLevel(strings.ToLower(string(cfg.Logging.Level)))
	cfg.Logging.Format = LogFormat(strings.ToLower(string(cfg.Logging.Format)))
	cfg.Server.TLS.MinVersion = TLSVersion(normalizeTLSVersion(string(cfg.Server.TLS.MinVersion)))
}

// normalizeTLSVersion maps the various accepted spellings to canonical "1.2" / "1.3".
func normalizeTLSVersion(v string) string {
	switch strings.ToLower(v) {
	case "1.3", "tls13", "tls1.3":
		return string(TLSVersion13)
	case "1.2", "tls12", "tls1.2":
		return string(TLSVersion12)
	default:
		return v // leave as-is; validation will catch invalid values
	}
}

// Validate checks that the configuration is internally consistent.
func Validate(cfg *Config) error {
	if err := validateBackend(cfg); err != nil {
		return err
	}
	if err := validateDurations(cfg); err != nil {
		return err
	}
	if err := validateTLS(cfg); err != nil {
		return err
	}
	if err := validateAuth(cfg); err != nil {
		return err
	}
	if err := validateRateLimit(cfg); err != nil {
		return err
	}
	if err := validateRedis(cfg); err != nil {
		return err
	}
	if err := validateCacheRedis(cfg); err != nil {
		return err
	}
	if err := validateLogging(cfg); err != nil {
		return err
	}
	return validateTracing(cfg)
}

func validateBackend(cfg *Config) error {
	if cfg.Backend.URL == "" {
		// backend.url is optional when the external rate limit service is
		// enabled — the external service can provide per-tenant backend URLs.
		// Without either, there is no backend to proxy to.
		if !cfg.RateLimit.External.Enabled {
			return fmt.Errorf("backend.url is required when rate_limit.external is not enabled")
		}
		return nil
	}

	// Normalize the backend URL so that host always includes an explicit port.
	// This avoids port-guessing logic scattered across the codebase.
	normalized, err := normalizeURL(cfg.Backend.URL)
	if err != nil {
		return fmt.Errorf("invalid backend.url %q: %w", cfg.Backend.URL, err)
	}
	cfg.Backend.URL = normalized

	return nil
}

// normalizeURL parses a URL and ensures the host always has an explicit port.
// If no port is specified, the scheme-appropriate default is appended
// (80 for http/ws, 443 for https/wss).
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("scheme and host are required")
	}

	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "https", "wss":
			u.Host += ":443"
		default:
			u.Host += ":80"
		}
	}

	return u.String(), nil
}

func validateDurations(cfg *Config) error {
	durations := []struct {
		name, val string
	}{
		{"server.read_timeout", cfg.Server.ReadTimeout},
		{"server.write_timeout", cfg.Server.WriteTimeout},
		{"server.idle_timeout", cfg.Server.IdleTimeout},
		{"server.drain_timeout", cfg.Server.DrainTimeout},
		{"server.request_timeout", cfg.Server.RequestTimeout},
		{"backend.timeout", cfg.Backend.Timeout},
		{"backend.idle_conn_timeout", cfg.Backend.IdleConnTimeout},
		{"admin.read_timeout", cfg.Admin.ReadTimeout},
		{"admin.write_timeout", cfg.Admin.WriteTimeout},
		{"admin.idle_timeout", cfg.Admin.IdleTimeout},
		{"backend.transport.dial_timeout", cfg.Backend.Transport.DialTimeout},
		{"backend.transport.dial_keep_alive", cfg.Backend.Transport.DialKeepAlive},
		{"backend.transport.tls_handshake_timeout", cfg.Backend.Transport.TLSHandshakeTimeout},
		{"backend.transport.expect_continue_timeout", cfg.Backend.Transport.ExpectContinueTimeout},
		{"backend.transport.h2_read_idle_timeout", cfg.Backend.Transport.H2ReadIdleTimeout},
		{"backend.transport.h2_ping_timeout", cfg.Backend.Transport.H2PingTimeout},
		{"backend.transport.websocket_dial_timeout", cfg.Backend.Transport.WebSocketDialTimeout},
		{"rate_limit.min_ttl", cfg.RateLimit.MinTTL},
	}

	for _, d := range durations {
		if d.val == "" {
			continue
		}
		if _, err := time.ParseDuration(d.val); err != nil {
			return fmt.Errorf("invalid %s %q: %w", d.name, d.val, err)
		}
	}
	return nil
}

func validateTLS(cfg *Config) error {
	if cfg.Server.TLS.Enabled {
		if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
			return fmt.Errorf("server.tls.cert_file and server.tls.key_file are required when TLS is enabled")
		}
	}
	if cfg.Server.TLS.HTTP3Enabled && !cfg.Server.TLS.Enabled {
		return fmt.Errorf("server.tls.http3_enabled requires server.tls.enabled to be true (QUIC mandates TLS)")
	}
	if v := cfg.Server.TLS.MinVersion; v != "" && !v.Valid() {
		return fmt.Errorf("invalid server.tls.min_version %q: must be 1.2 or 1.3", v)
	}
	return nil
}

func validateAuth(cfg *Config) error {
	if cfg.Auth.Enabled {
		if cfg.Auth.HTTP.URL == "" && cfg.Auth.GRPC.Address == "" {
			return fmt.Errorf("auth.http.url or auth.grpc.address is required when auth is enabled")
		}
		if fp := cfg.Auth.FailurePolicy; fp != "" && !fp.Valid() {
			return fmt.Errorf("invalid auth.failure_policy %q: must be failclosed or failopen", fp)
		}
	}
	return nil
}

func validateRateLimit(cfg *Config) error {
	if cfg.RateLimit.Average < 0 {
		return fmt.Errorf("rate_limit.average must be >= 0")
	}
	if cfg.RateLimit.Burst < 1 {
		cfg.RateLimit.Burst = 1
	}
	if fp := cfg.RateLimit.FailurePolicy; fp != "" && !fp.Valid() {
		return fmt.Errorf("invalid rate_limit.failure_policy %q: must be passthrough, failclosed, or inmemoryfallback", fp)
	}
	if cfg.RateLimit.FailureCode != 0 && (cfg.RateLimit.FailureCode < 400 || cfg.RateLimit.FailureCode > 599) {
		return fmt.Errorf("invalid rate_limit.failure_code %d: must be 4xx or 5xx", cfg.RateLimit.FailureCode)
	}
	if err := validateKeyStrategy(cfg.RateLimit.KeyStrategy); err != nil {
		return err
	}
	if cfg.RateLimit.External.Enabled {
		if cfg.RateLimit.External.HTTP.URL == "" && cfg.RateLimit.External.GRPC.Address == "" {
			return fmt.Errorf("rate_limit.external requires http.url or grpc.address when enabled")
		}
	}
	return nil
}

func validateKeyStrategy(ks KeyStrategyConfig) error {
	if ks.Type != "" && !ks.Type.Valid() {
		return fmt.Errorf("unknown rate_limit.key_strategy.type %q", ks.Type)
	}
	if (ks.Type == KeyStrategyHeader || ks.Type == KeyStrategyComposite) && ks.HeaderName == "" {
		return fmt.Errorf("rate_limit.key_strategy.header_name is required when type is %q", ks.Type)
	}
	return nil
}

func validateRedis(cfg *Config) error {
	return validateRedisConfig(cfg.Redis, "redis")
}

func validateCacheRedis(cfg *Config) error {
	if cfg.CacheRedis == nil || len(cfg.CacheRedis.Endpoints) == 0 {
		return nil // not configured — will fall back to main redis
	}
	if cfg.CacheRedis.Mode == "" {
		cfg.CacheRedis.Mode = RedisModeSingle
	}
	return validateRedisConfig(*cfg.CacheRedis, "cache_redis")
}

func validateRedisConfig(rc RedisConfig, prefix string) error {
	if !rc.Mode.Valid() {
		return fmt.Errorf("invalid %s.mode %q", prefix, rc.Mode)
	}
	if len(rc.Endpoints) == 0 {
		return fmt.Errorf("%s.endpoints: at least one endpoint is required", prefix)
	}
	if rc.Mode == RedisModeSingle && len(rc.Endpoints) > 1 {
		return fmt.Errorf("%s.endpoints: single mode requires exactly one endpoint, got %d", prefix, len(rc.Endpoints))
	}
	if rc.Mode == RedisModeSentinel && rc.MasterName == "" {
		return fmt.Errorf("%s.master_name is required for sentinel mode", prefix)
	}
	if rc.Mode == RedisModeReplication && len(rc.Endpoints) < 2 {
		return fmt.Errorf("%s.endpoints: replication mode requires at least 2 endpoints, got %d", prefix, len(rc.Endpoints))
	}
	return nil
}

func validateLogging(cfg *Config) error {
	if !cfg.Logging.Level.Valid() {
		return fmt.Errorf("invalid logging.level %q", cfg.Logging.Level)
	}
	if !cfg.Logging.Format.Valid() {
		return fmt.Errorf("invalid logging.format %q", cfg.Logging.Format)
	}
	return nil
}

func validateTracing(cfg *Config) error {
	if cfg.Tracing.Enabled && cfg.Tracing.Endpoint == "" {
		return fmt.Errorf("tracing.endpoint is required when tracing is enabled")
	}
	return nil
}

// CacheRedisConfig returns the Redis configuration to use for caching.
// If a dedicated cache_redis section is configured, it is returned. Otherwise,
// the main redis config is returned so both share the same cluster.
func (c *Config) CacheRedisConfig() RedisConfig {
	if c.CacheRedis != nil && len(c.CacheRedis.Endpoints) > 0 {
		return *c.CacheRedis
	}
	return c.Redis
}

// ParseDuration parses a duration string, returning def if the string is empty.
func ParseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

// MustParseDuration parses a duration string, returning def on empty or error.
func MustParseDuration(s string, def time.Duration) time.Duration {
	d, err := ParseDuration(s, def)
	if err != nil {
		return def
	}
	return d
}

// RequiresRestart compares this config to old and returns a list of field
// paths that changed and require a process restart. An empty slice means
// the new config can be hot-reloaded safely.
func (c *Config) RequiresRestart(old *Config) []string {
	if old == nil {
		return nil
	}
	var fields []string
	if c.Server.Address != old.Server.Address {
		fields = append(fields, "server.address")
	}
	if c.Admin.Address != old.Admin.Address {
		fields = append(fields, "admin.address")
	}
	if c.Redis.Mode != old.Redis.Mode {
		fields = append(fields, "redis.mode")
	}
	if c.Server.TLS.Enabled != old.Server.TLS.Enabled {
		fields = append(fields, "server.tls.enabled")
	}
	if c.Server.TLS.HTTP3Enabled != old.Server.TLS.HTTP3Enabled {
		fields = append(fields, "server.tls.http3_enabled")
	}
	return fields
}
