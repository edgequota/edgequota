package auth

import (
	"context"
	"net"
	"testing"
	"time"

	authv1 "github.com/edgequota/edgequota/api/gen/grpc/edgequota/auth/v1"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// testAuthServer implements the generated AuthServiceServer interface.
type testAuthServer struct {
	authv1.UnimplementedAuthServiceServer
	allowAll           bool
	requestHeaders     map[string]string    // headers to inject on allow
	lastReq            *authv1.CheckRequest // captured for assertion
	cacheMaxAgeSeconds *int64               // cache TTL to return, nil = not set
	cacheNoStore       bool
}

func (s *testAuthServer) Check(_ context.Context, req *authv1.CheckRequest) (*authv1.CheckResponse, error) {
	s.lastReq = req
	if s.allowAll {
		return &authv1.CheckResponse{
			Allowed:            true,
			StatusCode:         200,
			RequestHeaders:     s.requestHeaders,
			CacheMaxAgeSeconds: s.cacheMaxAgeSeconds,
			CacheNoStore:       s.cacheNoStore,
		}, nil
	}
	return &authv1.CheckResponse{
		Allowed:    false,
		StatusCode: 403,
		DenyBody:   "forbidden by test",
	}, nil
}

func TestNewClientGRPC(t *testing.T) {
	t.Run("creates client with gRPC address", func(t *testing.T) {
		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC: config.AuthGRPCConfig{
				Address: "localhost:0",
			},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.NotNil(t, c.grpcClient)
		require.NoError(t, c.Close())
	})

	t.Run("returns error for invalid TLS CA file", func(t *testing.T) {
		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC: config.AuthGRPCConfig{
				Address: "localhost:50051",
				TLS: config.GRPCTLSConfig{
					Enabled: true,
					CAFile:  "/nonexistent/ca.pem",
				},
			},
		}
		_, err := NewClient(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "auth grpc tls")
	})
}

func TestCheckGRPC(t *testing.T) {
	t.Run("allows request via gRPC", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, &testAuthServer{allowAll: true})

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC: config.AuthGRPCConfig{
				Address: lis.Addr().String(),
			},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1/resource",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Equal(t, 200, resp.StatusCode)
	})

	t.Run("denies request via gRPC", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, &testAuthServer{allowAll: false})

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC: config.AuthGRPCConfig{
				Address: lis.Addr().String(),
			},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/admin",
		})
		require.NoError(t, err)
		assert.False(t, resp.Allowed)
		assert.Equal(t, 403, resp.StatusCode)
		assert.Equal(t, "forbidden by test", resp.DenyBody)
	})

	t.Run("returns error when gRPC server is down", func(t *testing.T) {
		cfg := config.AuthConfig{
			Timeout: "100ms",
			GRPC: config.AuthGRPCConfig{
				Address: "127.0.0.1:1",
			},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		_, err = c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "auth grpc check")
	})
}

func TestClientCloseGRPC(t *testing.T) {
	t.Run("closes gRPC connection", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := grpc.NewServer()
		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC: config.AuthGRPCConfig{
				Address: lis.Addr().String(),
			},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		assert.NoError(t, c.Close())
	})
}

func TestCheckGRPCRequestHeaders(t *testing.T) {
	t.Run("returns request_headers on allow", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		authSvc := &testAuthServer{
			allowAll: true,
			requestHeaders: map[string]string{
				"X-Tenant-Id": "acme-corp",
				"X-Plan-Tier": "premium",
			},
		}
		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, authSvc)

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC:    config.AuthGRPCConfig{Address: lis.Addr().String()},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1/resource",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Equal(t, "acme-corp", resp.RequestHeaders["X-Tenant-Id"])
		assert.Equal(t, "premium", resp.RequestHeaders["X-Plan-Tier"])
	})

	t.Run("returns nil request_headers when not set", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		authSvc := &testAuthServer{allowAll: true}
		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, authSvc)

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC:    config.AuthGRPCConfig{Address: lis.Addr().String()},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		resp, err := c.Check(context.Background(), &CheckRequest{
			Method: "GET",
			Path:   "/api/v1/resource",
		})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		assert.Nil(t, resp.RequestHeaders)
	})
}

func TestCheckGRPCPreservesRequestData(t *testing.T) {
	t.Run("sends full request data via gRPC", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		authSvc := &testAuthServer{allowAll: true}
		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, authSvc)

		go srv.Serve(lis)
		defer srv.Stop()

		cfg := config.AuthConfig{
			Timeout: "5s",
			GRPC:    config.AuthGRPCConfig{Address: lis.Addr().String()},
		}
		c, err := NewClient(cfg)
		require.NoError(t, err)
		defer c.Close()

		_, err = c.Check(context.Background(), &CheckRequest{
			Method:     "POST",
			Path:       "/api/data",
			Headers:    map[string]string{"Authorization": "Bearer tok"},
			RemoteAddr: "10.0.0.1:12345",
		})
		require.NoError(t, err)
		assert.Equal(t, "POST", authSvc.lastReq.GetMethod())
		assert.Equal(t, "/api/data", authSvc.lastReq.GetPath())
		assert.Equal(t, "Bearer tok", authSvc.lastReq.GetHeaders()["Authorization"])
		assert.Equal(t, "10.0.0.1:12345", authSvc.lastReq.GetRemoteAddr())
	})
}

func TestCheckGRPCCacheFields(t *testing.T) {
	startGRPC := func(t *testing.T, svc *testAuthServer) (*Client, func()) {
		t.Helper()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		srv := grpc.NewServer()
		authv1.RegisterAuthServiceServer(srv, svc)
		go srv.Serve(lis)

		c, err := NewClient(config.AuthConfig{
			Timeout: "5s",
			GRPC:    config.AuthGRPCConfig{Address: lis.Addr().String()},
		})
		require.NoError(t, err)
		return c, func() { _ = c.Close(); srv.Stop() }
	}

	t.Run("returns cache_max_age_seconds from gRPC response", func(t *testing.T) {
		ttl := int64(300)
		svc := &testAuthServer{allowAll: true, cacheMaxAgeSeconds: &ttl}
		c, cleanup := startGRPC(t, svc)
		defer cleanup()

		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.True(t, resp.Allowed)
		require.NotNil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, int64(300), *resp.CacheMaxAgeSec)
		assert.Equal(t, 300*time.Second, resp.ResolveCacheTTL())
	})

	t.Run("cache_no_store=true returns zero TTL", func(t *testing.T) {
		ttl := int64(300)
		svc := &testAuthServer{allowAll: true, cacheMaxAgeSeconds: &ttl, cacheNoStore: true}
		c, cleanup := startGRPC(t, svc)
		defer cleanup()

		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.True(t, resp.CacheNoStore)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})

	t.Run("nil cache_max_age_seconds returns zero TTL", func(t *testing.T) {
		svc := &testAuthServer{allowAll: true}
		c, cleanup := startGRPC(t, svc)
		defer cleanup()

		resp, err := c.Check(context.Background(), &CheckRequest{Method: "GET", Path: "/api"})
		require.NoError(t, err)
		assert.Nil(t, resp.CacheMaxAgeSec)
		assert.Equal(t, time.Duration(0), resp.ResolveCacheTTL())
	})
}
