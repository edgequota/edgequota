package ratelimit

import (
	"context"
	"net"
	"testing"

	ratelimitv1 "github.com/edgequota/edgequota/api/gen/grpc/edgequota/ratelimit/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
)

// newGRPCTestCacheRedis is a helper for gRPC tests that need Redis caching.
// Re-uses the same factory as the HTTP external tests.
var newGRPCTestCacheRedis = newTestCacheRedis

// testRateLimitServer implements the generated RateLimitServiceServer interface.
type testRateLimitServer struct {
	ratelimitv1.UnimplementedRateLimitServiceServer
	average      int64
	burst        int64
	period       string
	cacheMaxAge  *int64
	cacheNoStore bool
	callCount    int
}

func (s *testRateLimitServer) GetLimits(_ context.Context, _ *ratelimitv1.GetLimitsRequest) (*ratelimitv1.GetLimitsResponse, error) {
	s.callCount++
	resp := &ratelimitv1.GetLimitsResponse{
		Average:      s.average,
		Burst:        s.burst,
		Period:       s.period,
		CacheNoStore: s.cacheNoStore,
	}
	if s.cacheMaxAge != nil {
		resp.CacheMaxAgeSeconds = s.cacheMaxAge
	}
	return resp, nil
}

func TestNewExternalClientGRPC(t *testing.T) {
	t.Run("creates client with gRPC address", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Enabled: true,
			Timeout: "5s",
			GRPC: config.ExternalGRPCConfig{
				Address: "localhost:0",
			},
		}
		ec, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		assert.NotNil(t, ec)
		assert.NotNil(t, ec.grpcClient)
		require.NoError(t, ec.Close())
	})

	t.Run("returns error for invalid TLS CA file", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Enabled: true,
			Timeout: "5s",
			GRPC: config.ExternalGRPCConfig{
				Address: "localhost:50051",
				TLS: config.GRPCTLSConfig{
					Enabled: true,
					CAFile:  "/nonexistent/ca.pem",
				},
			},
		}
		_, err := NewExternalClient(cfg, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "external ratelimit grpc tls")
	})
}

func TestGetLimitsGRPC(t *testing.T) {
	t.Run("fetches limits via gRPC", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := grpc.NewServer()
		ratelimitv1.RegisterRateLimitServiceServer(srv, &testRateLimitServer{
			average: 100, burst: 50, period: "1s",
		})

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.ExternalRLConfig{
			Enabled: true,
			Timeout: "5s",
			GRPC: config.ExternalGRPCConfig{
				Address: lis.Addr().String(),
			},
		}
		ec, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		defer ec.Close()

		limits, err := ec.GetLimits(context.Background(), &ExternalRequest{
			Key:    "test-key",
			Method: "GET",
			Path:   "/api",
		})
		require.NoError(t, err)
		assert.Equal(t, int64(100), limits.Average)
		assert.Equal(t, int64(50), limits.Burst)
		assert.Equal(t, "1s", limits.Period)
	})

	t.Run("caches gRPC results in Redis using body cache fields", func(t *testing.T) {
		redisClient, mr := newGRPCTestCacheRedis(t)

		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		cacheAge := int64(300)
		rlSvc := &testRateLimitServer{
			average: 10, burst: 5, period: "1s", cacheMaxAge: &cacheAge,
		}
		srv := grpc.NewServer()
		ratelimitv1.RegisterRateLimitServiceServer(srv, rlSvc)

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.ExternalRLConfig{
			Enabled:  true,
			Timeout:  "5s",
			CacheTTL: "10s",
			GRPC:     config.ExternalGRPCConfig{Address: lis.Addr().String()},
		}
		ec, err := NewExternalClient(cfg, redisClient)
		require.NoError(t, err)
		defer ec.Close()

		req := &ExternalRequest{Key: "cache-key", Method: "GET", Path: "/"}

		// First call should hit the server.
		_, err = ec.GetLimits(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, 1, rlSvc.callCount)

		// Verify Redis TTL comes from body field (300s), not default (10s).
		ttl := mr.TTL(cacheKeyPrefix + "cache-key")
		assert.True(t, ttl.Seconds() > 200, "expected TTL ~300s from body field, got %v", ttl)

		// Second call should use Redis cache.
		_, err = ec.GetLimits(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, 1, rlSvc.callCount) // Still 1 — Redis cache hit.
	})

	t.Run("gRPC cache_no_store prevents caching", func(t *testing.T) {
		redisClient, _ := newGRPCTestCacheRedis(t)

		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		rlSvc := &testRateLimitServer{
			average: 10, burst: 5, period: "1s", cacheNoStore: true,
		}
		srv := grpc.NewServer()
		ratelimitv1.RegisterRateLimitServiceServer(srv, rlSvc)

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.ExternalRLConfig{
			Enabled:  true,
			Timeout:  "5s",
			CacheTTL: "60s",
			GRPC:     config.ExternalGRPCConfig{Address: lis.Addr().String()},
		}
		ec, err := NewExternalClient(cfg, redisClient)
		require.NoError(t, err)
		defer ec.Close()

		req := &ExternalRequest{Key: "no-store-key", Method: "GET", Path: "/"}

		_, err = ec.GetLimits(context.Background(), req)
		require.NoError(t, err)
		_, err = ec.GetLimits(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, 2, rlSvc.callCount) // Both calls hit server — not cached.
	})

	t.Run("returns error when gRPC server is down", func(t *testing.T) {
		cfg := config.ExternalRLConfig{
			Enabled: true,
			Timeout: "100ms",
			GRPC:    config.ExternalGRPCConfig{Address: "127.0.0.1:1"},
		}
		ec, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		defer ec.Close()

		_, err = ec.GetLimits(context.Background(), &ExternalRequest{Key: "k"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "grpc get limits")
	})
}

func TestExternalClientCloseGRPC(t *testing.T) {
	t.Run("closes gRPC connection", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := grpc.NewServer()
		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.ExternalRLConfig{
			Enabled: true,
			Timeout: "5s",
			GRPC:    config.ExternalGRPCConfig{Address: lis.Addr().String()},
		}
		ec, err := NewExternalClient(cfg, nil)
		require.NoError(t, err)
		assert.NoError(t, ec.Close())
	})
}

func TestGetLimitsNoServiceConfigured(t *testing.T) {
	t.Run("returns error when no service configured", func(t *testing.T) {
		ec := &ExternalClient{
			fetchSem:       semaphore.NewWeighted(defaultMaxConcurrentFetches),
			maxBreakers:    defaultMaxCircuitBreakers,
			cbThreshold:    defaultCBThreshold,
			cbResetTimeout: defaultCBResetTimeout,
			done:           make(chan struct{}),
		}
		_, err := ec.GetLimits(context.Background(), &ExternalRequest{Key: "k"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no external rate limit service configured")
	})
}
