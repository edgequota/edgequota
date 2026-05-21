package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/edgequota/edgequota/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminPprofEndpointsReachable(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.Defaults()
	cfg.RateLimit.Static.BackendURL = "http://backend:8080"
	cfg.Redis.Endpoints = []string{mr.Addr()}

	srv, err := New(cfg, testLogger(), "test")
	require.NoError(t, err)
	defer srv.chain.Close()

	cases := []struct {
		path        string
		expectCT    string
		expectBody  string
		expectExact bool
	}{
		{path: "/debug/pprof/", expectCT: "text/html", expectBody: "Types of profiles available"},
		{path: "/debug/pprof/heap?debug=1", expectCT: "text/plain", expectBody: "heap profile"},
		{path: "/debug/pprof/goroutine?debug=1", expectCT: "text/plain", expectBody: "goroutine profile"},
		{path: "/debug/pprof/cmdline", expectCT: "text/plain", expectBody: ""},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			srv.adminServer.Handler.ServeHTTP(w, r)

			assert.Equal(t, http.StatusOK, w.Code, "%s should return 200", tc.path)
			assert.Contains(t, w.Header().Get("Content-Type"), tc.expectCT)
			if tc.expectBody != "" {
				assert.Contains(t, w.Body.String(), tc.expectBody)
			}
		})
	}
}
