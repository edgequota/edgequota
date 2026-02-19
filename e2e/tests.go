package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	grpcstatus "google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Test framework
// ---------------------------------------------------------------------------

type testResult struct {
	name   string
	passed bool
	detail string
}

type testCase struct {
	name     string
	scenario string // EdgeQuota deployment scenario name (for log collection)
	fn       func() testResult
}

// scenarioNodePorts maps scenario names to their Kubernetes NodePort.
// With QEMU + socket_vmnet, the minikube VM has a routable IP, so we
// access services directly at <minikubeIP>:<nodePort>.
var scenarioNodePorts = map[string]int{
	"single-pt":       30101,
	"single-fc":       30102,
	"single-fb":       30103,
	"repl-basic":      30104,
	"sentinel-basic":  30105,
	"cluster-basic":   30106,
	"key-header":      30107,
	"key-composite":   30108,
	"burst-test":      30109,
	"no-limit":        30110,
	"protocol":        30111,
	"protocol-rl":     30112,
	"protocol-h3":     30214, // TLS NodePort (TCP + UDP/QUIC)
	"config-reload":   30115,
	"dynamic-backend": 30116,
}

// runAllTests resolves the minikube IP, builds base URLs for every scenario,
// runs all tests locally against the cluster, and writes a report file.
func runAllTests() bool {
	runStart := time.Now()
	minikubeIP := getMinikubeIP()
	info("Minikube IP: %s", minikubeIP)

	// Build base URL map using the routable minikube IP + NodePort.
	baseURLs := make(map[string]string, len(scenarioNodePorts))
	for scenario, port := range scenarioNodePorts {
		scheme := "http"
		if scenario == "protocol-h3" {
			scheme = "https"
		}
		baseURLs[scenario] = fmt.Sprintf("%s://%s:%d", scheme, minikubeIP, port)
	}

	// Wait for all EdgeQuota instances to become reachable.
	info("Waiting for EdgeQuota instances to become reachable...")
	for scenario, base := range baseURLs {
		if err := waitForEdgeQuota(base, 60*time.Second); err != nil {
			fatal("EdgeQuota %q not reachable at %s: %v", scenario, base, err)
		}
		info("  ✓ %s reachable", scenario)
	}

	cases := allTestCases(baseURLs)
	entries := make([]TestEntry, 0, len(cases))
	passCount, failCount := 0, 0

	for i, tc := range cases {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(cases), tc.name)

		tStart := time.Now()
		r := tc.fn()
		elapsed := time.Since(tStart)

		entry := TestEntry{
			Index:         i + 1,
			Name:          tc.name,
			TestID:        r.name,
			Passed:        r.passed,
			Detail:        r.detail,
			Duration:      elapsed,
			DurationHuman: elapsed.Round(time.Millisecond).String(),
		}
		entries = append(entries, entry)

		if r.passed {
			passCount++
			fmt.Printf("  ✅ PASS: %s (%s)\n", r.detail, entry.DurationHuman)
		} else {
			failCount++
			fmt.Printf("  ❌ FAIL: %s (%s)\n", r.detail, entry.DurationHuman)
		}
	}

	allPassed := failCount == 0

	// Summary.
	fmt.Printf("\n%s\n", strings.Repeat("-", 60))
	fmt.Printf("Results: %d passed, %d failed, %d total\n", passCount, failCount, len(entries))
	fmt.Printf("%s\n", strings.Repeat("-", 60))

	for _, e := range entries {
		mark := "✅"
		if !e.Passed {
			mark = "❌"
		}
		fmt.Printf("  %s %s\n", mark, e.TestID)
	}

	// Collect pod logs when there are failures.
	var podLogs []PodLog
	if !allPassed {
		info("Collecting EdgeQuota pod logs for failure diagnostics...")
		podLogs = collectEdgeQuotaLogs()
	}

	// Build and write report.
	report := &Report{
		Timestamp:  runStart,
		Duration:   time.Since(runStart),
		MinikubeIP: minikubeIP,
		PassCount:  passCount,
		FailCount:  failCount,
		TotalCount: len(entries),
		AllPassed:  allPassed,
		Tests:      entries,
		PodLogs:    podLogs,
	}

	reportPath := writeReport(report)
	if reportPath != "" {
		fmt.Printf("\n📄 Report: %s\n", reportPath)
	}

	return allPassed
}

// ---------------------------------------------------------------------------
// Test definitions
// ---------------------------------------------------------------------------

func allTestCases(urls map[string]string) []testCase {
	return []testCase{
		// Topology: single
		{"Single mode — passThrough (happy path)", "single-pt", func() testResult { return testSinglePassThrough(urls["single-pt"]) }},
		{"Single mode — failClosed (Redis down)", "single-fc", func() testResult { return testSingleFailClosed(urls["single-fc"]) }},
		{"Single mode — inMemoryFallback (Redis down)", "single-fb", func() testResult { return testSingleFallback(urls["single-fb"]) }},

		// Topology: replication
		{"Replication mode — basic rate limiting", "repl-basic", func() testResult { return testReplicationBasic(urls["repl-basic"]) }},

		// Topology: sentinel
		{"Sentinel mode — basic master discovery", "sentinel-basic", func() testResult { return testSentinelBasic(urls["sentinel-basic"]) }},

		// Topology: cluster
		{"Cluster mode — basic with MOVED redirects", "cluster-basic", func() testResult { return testClusterBasic(urls["cluster-basic"]) }},

		// Key strategies
		{"Key strategy — header-based", "key-header", func() testResult { return testKeyHeader(urls["key-header"]) }},
		{"Key strategy — composite (header + path)", "key-composite", func() testResult { return testKeyComposite(urls["key-composite"]) }},

		// Rate limiting behavior
		{"Burst exhaustion — 429 after burst", "burst-test", func() testResult { return testBurstExhaustion(urls["burst-test"]) }},
		{"No limit — average=0 passes all", "no-limit", func() testResult { return testNoLimit(urls["no-limit"]) }},
		{"Retry-After header present on 429", "burst-test", func() testResult { return testRetryAfterHeader(urls["burst-test"]) }},

		// Failure injection
		{"Failure injection — kill single Redis, passThrough", "single-pt", func() testResult { return testKillSinglePassThrough(urls["single-pt"]) }},
		{"Failure injection — kill single Redis, inMemoryFallback", "single-fb", func() testResult { return testKillSingleFallback(urls["single-fb"]) }},

		// Recovery
		{"Recovery — Redis killed then restarted, limiting resumes", "burst-test", func() testResult { return testRecoveryAfterRestart(urls["burst-test"]) }},

		// Concurrency
		{"Concurrent burst — no 500s under load", "single-pt", func() testResult { return testConcurrentBurst(urls["single-pt"]) }},

		// Host/domain forwarding — ensures upstream (e.g. Traefik) gateway routing works
		{"Forwarding — Host header preserved for upstream routing", "protocol", func() testResult { return testHostHeaderForwarding(urls["protocol"]) }},

		// Protocol tests — no rate limit
		{"Protocol — HTTP/1.1 proxies correctly", "protocol", func() testResult { return testProtocolHTTP(urls["protocol"]) }},
		{"Protocol — HTTP/2 (h2c) proxies correctly", "protocol", func() testResult { return testProtocolHTTP2(urls["protocol"]) }},
		{"Protocol — gRPC unary call", "protocol", func() testResult { return testProtocolGRPC(urls["protocol"]) }},
		{"Protocol — SSE event stream", "protocol", func() testResult { return testProtocolSSE(urls["protocol"]) }},
		{"Protocol — WebSocket echo", "protocol", func() testResult { return testProtocolWebSocket(urls["protocol"]) }},

		// HTTP/3 (QUIC) — tested locally via the routable minikube IP + UDP NodePort
		{"Protocol — HTTP/3 (QUIC)", "protocol-h3", func() testResult { return testProtocolHTTP3(urls["protocol-h3"]) }},
		{"Protocol — HTTPS (HTTP/2 over TLS)", "protocol-h3", func() testResult { return testProtocolHTTPS(urls["protocol-h3"]) }},

		// Protocol tests — with rate limiting (SSE/WS first since they each use 1
		// token; gRPC-RL last because it intentionally drains the burst bucket).
		{"Protocol+RL — SSE connects within burst", "protocol-rl", func() testResult { return testProtocolSSERateLimited(urls["protocol-rl"]) }},
		{"Protocol+RL — WebSocket connects within burst", "protocol-rl", func() testResult { return testProtocolWebSocketRateLimited(urls["protocol-rl"]) }},
		{"Protocol+RL — gRPC gets rate limited", "protocol-rl", func() testResult { return testProtocolGRPCRateLimited(urls["protocol-rl"]) }},

		// Config hot-reload
		{"Config reload — rate limit changes via ConfigMap", "config-reload", func() testResult { return testConfigReload(urls["config-reload"]) }},
		{"Config reload — TLS cert rotation", "protocol-h3", func() testResult { return testCertReload(urls["protocol-h3"]) }},

		// Dynamic backend URL (tenant-aware routing)
		{"Dynamic backend — tenant-a routes to whoami", "dynamic-backend", func() testResult { return testDynamicBackendTenantA(urls["dynamic-backend"]) }},
		{"Dynamic backend — tenant-b routes to testbackend", "dynamic-backend", func() testResult { return testDynamicBackendTenantB(urls["dynamic-backend"]) }},
		{"Dynamic backend — unknown tenant uses default backend", "dynamic-backend", func() testResult { return testDynamicBackendDefault(urls["dynamic-backend"]) }},
	}
}

// ---------------------------------------------------------------------------
// Individual tests
// ---------------------------------------------------------------------------

func testSinglePassThrough(base string) testResult {
	ok200, ok429 := sendBurst(base, nil, 5)

	if ok200 >= 3 {
		return pass("single-pt", "%d/5 allowed (within burst)", ok200)
	}

	return fail("single-pt", "expected ≥3 allowed, got %d (429s: %d)", ok200, ok429)
}

func testSingleFailClosed(base string) testResult {
	// First verify it works normally.
	ok200, _ := sendBurst(base, nil, 3)
	if ok200 < 1 {
		return fail("single-fc", "expected ≥1 allowed before failure injection, got 0")
	}

	// Scale Redis deployment to 0 so that the Deployment controller does
	// not immediately restart the pod. Simply deleting the pod causes the
	// Deployment to schedule a replacement, which can become Ready before
	// the proxy detects the outage — defeating the test.
	info("  Scaling Redis single deployment to 0 replicas...")

	if _, err := kubectl("scale", "deployment", "redis-single", "-n", namespace, "--replicas=0"); err != nil {
		return fail("single-fc", "could not scale Redis to 0: %v", err)
	}

	// Wait for the proxy to detect Redis is down. Poll until we see failClosed
	// behavior (429s) instead of using a fixed sleep.
	info("  Waiting for EdgeQuota to detect Redis failure...")
	if err := pollUntil(30*time.Second, "EdgeQuota detects Redis down (failClosed 429s)", func() bool {
		_, ok429 := sendBurst(base, nil, 5)
		return ok429 > 0
	}); err != nil {
		return fail("single-fc", "EdgeQuota did not detect Redis failure: %v", err)
	}

	// Requests should now get 429 (failClosed).
	_, _, codes := sendBurstDetailed(base, nil, 5)
	has429 := false

	for _, c := range codes {
		if c == 429 {
			has429 = true
			break
		}
	}

	// Restore Redis: scale back to 1 and wait for it to become Ready.
	info("  Restoring Redis single deployment to 1 replica...")
	kubectl("scale", "deployment", "redis-single", "-n", namespace, "--replicas=1")
	waitForPods("app=redis-single", 60*time.Second)

	if has429 {
		return pass("single-fc", "got 429 after Redis killed (failClosed working)")
	}

	return fail("single-fc", "expected 429 after kill, got codes: %v", codes)
}

func testSingleFallback(base string) testResult {
	// Verify normal operation.
	ok200Pre, _ := sendBurst(base, nil, 3)
	if ok200Pre < 1 {
		return fail("single-fb", "expected ≥1 allowed before kill, got 0")
	}

	// Kill Redis.
	info("  Killing Redis single pod for fallback test...")

	if err := deletePod("app=redis-single"); err != nil {
		return fail("single-fb", "could not kill Redis pod: %v", err)
	}

	// Poll until in-memory fallback kicks in (we see both 200s and 429s).
	info("  Waiting for EdgeQuota to fall back to in-memory limiter...")
	var ok200, ok429 int
	if err := pollUntil(30*time.Second, "in-memory fallback active", func() bool {
		ok200, ok429 = sendBurst(base, nil, 10)
		return ok200 > 0 && ok429 > 0
	}); err != nil {
		// If poll fails, continue with last values for the assertion below.
		info("  Poll timed out, checking last burst results")
	}

	// Restore Redis.
	waitForPods("app=redis-single", 60*time.Second)

	if ok200 > 0 && ok429 > 0 {
		return pass("single-fb", "fallback active: %d allowed, %d limited", ok200, ok429)
	}

	if ok200 > 0 {
		return pass("single-fb", "fallback allowed %d requests (burst not exhausted)", ok200)
	}

	return fail("single-fb", "expected some allowed via fallback, got 200s=%d 429s=%d", ok200, ok429)
}

func testReplicationBasic(base string) testResult {
	ok200, _ := sendBurst(base, nil, 5)

	if ok200 >= 3 {
		return pass("repl-basic", "%d/5 allowed via replication mode", ok200)
	}

	return fail("repl-basic", "expected ≥3 allowed, got %d", ok200)
}

func testSentinelBasic(base string) testResult {
	ok200, _ := sendBurst(base, nil, 5)

	if ok200 >= 3 {
		return pass("sentinel-basic", "%d/5 allowed via sentinel-discovered master", ok200)
	}

	return fail("sentinel-basic", "expected ≥3 allowed, got %d", ok200)
}

func testClusterBasic(base string) testResult {
	ok200, _ := sendBurst(base, nil, 5)

	if ok200 >= 3 {
		return pass("cluster-basic", "%d/5 allowed via cluster mode", ok200)
	}

	return fail("cluster-basic", "expected ≥3 allowed, got %d", ok200)
}

func testKeyHeader(base string) testResult {
	headers := map[string]string{"X-Tenant-Id": "tenant-e2e-test"}

	ok200, _ := sendBurst(base, headers, 5)
	if ok200 < 3 {
		return fail("key-header", "expected ≥3 allowed with header key, got %d", ok200)
	}

	// Requests without the header should fail (500 — missing required key).
	_, _, codes := sendBurstDetailed(base, nil, 2)
	has500 := false

	for _, c := range codes {
		if c == 500 {
			has500 = true
			break
		}
	}

	if !has500 {
		return fail("key-header", "expected 500 for missing header, got %v", codes)
	}

	return pass("key-header", "%d/5 allowed with header, 500 without header", ok200)
}

func testKeyComposite(base string) testResult {
	headers := map[string]string{"X-Tenant-Id": "tenant-composite"}

	ok200, _ := sendBurst(base, headers, 5)

	if ok200 >= 3 {
		return pass("key-composite", "%d/5 allowed with composite key", ok200)
	}

	return fail("key-composite", "expected ≥3 allowed, got %d", ok200)
}

func testBurstExhaustion(base string) testResult {
	// burst-test has average=2, burst=3. Send 10 fast requests.
	ok200, ok429 := sendBurst(base, nil, 10)

	if ok429 > 0 {
		return pass("burst-test", "%d allowed, %d limited (burst exhausted)", ok200, ok429)
	}

	return fail("burst-test", "expected some 429s, got 200s=%d 429s=%d", ok200, ok429)
}

func testNoLimit(base string) testResult {
	ok200, ok429 := sendBurst(base, nil, 20)

	if ok200 == 20 && ok429 == 0 {
		return pass("no-limit", "all 20 requests allowed (no rate limit)")
	}

	return fail("no-limit", "expected all 20 allowed, got 200s=%d 429s=%d", ok200, ok429)
}

func testRetryAfterHeader(base string) testResult {
	// Exhaust burst on burst-test (average=2, burst=3).
	sendBurst(base, nil, 5)

	time.Sleep(100 * time.Millisecond)

	// Next request should be 429 with Retry-After.
	resp, err := doHTTPRequest(base, nil)
	if err != nil {
		return fail("retry-after", "request error: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		ra := resp.Header.Get("Retry-After")
		if ra != "" {
			return pass("retry-after", "429 with Retry-After: %s", ra)
		}

		return fail("retry-after", "429 but no Retry-After header")
	}

	return fail("retry-after", "expected 429, got %d", resp.StatusCode)
}

func testKillSinglePassThrough(base string) testResult {
	info("  Killing Redis single pod...")

	if err := deletePod("app=redis-single"); err != nil {
		return fail("kill-pt", "could not kill Redis: %v", err)
	}

	// Poll until passthrough kicks in (all requests succeed).
	info("  Waiting for EdgeQuota to enter passthrough mode...")
	var ok200, ok429 int
	if err := pollUntil(30*time.Second, "passthrough mode (all 200s)", func() bool {
		ok200, ok429 = sendBurst(base, nil, 5)
		return ok200 == 5 && ok429 == 0
	}); err != nil {
		info("  Poll timed out, checking last burst results")
	}

	// Restore.
	waitForPods("app=redis-single", 60*time.Second)

	if ok200 >= 4 {
		return pass("kill-pt", "%d/5 passed through with Redis down", ok200)
	}

	return fail("kill-pt", "expected ≥4 passthrough, got 200s=%d 429s=%d", ok200, ok429)
}

func testKillSingleFallback(base string) testResult {
	info("  Killing Redis single pod...")

	if err := deletePod("app=redis-single"); err != nil {
		return fail("kill-fb", "could not kill Redis: %v", err)
	}

	// Poll until fallback limiter kicks in (mix of 200s and 429s).
	info("  Waiting for EdgeQuota to fall back to in-memory limiter...")
	var ok200, ok429 int
	if err := pollUntil(30*time.Second, "fallback limiter active", func() bool {
		ok200, ok429 = sendBurst(base, nil, 10)
		return ok200 > 0 && ok429 > 0
	}); err != nil {
		info("  Poll timed out, checking last burst results")
	}

	// Restore.
	waitForPods("app=redis-single", 60*time.Second)

	if ok200 > 0 {
		return pass("kill-fb", "fallback: %d allowed, %d limited", ok200, ok429)
	}

	return fail("kill-fb", "expected some allowed via fallback, got 200s=%d 429s=%d", ok200, ok429)
}

func testRecoveryAfterRestart(base string) testResult {
	// Step 1: Verify normal limiting is active (burst-test has average=2, burst=3).
	ok200, ok429 := sendBurst(base, nil, 10)
	if ok429 == 0 {
		return fail("recovery", "step 1: expected some 429s to prove Redis limiting, got 200s=%d 429s=%d", ok200, ok429)
	}

	// Wait briefly so token bucket replenishes before kill.
	time.Sleep(3 * time.Second)

	// Step 2: Kill Redis single pod.
	info("  Killing Redis single pod for recovery test...")

	if err := deletePod("app=redis-single"); err != nil {
		return fail("recovery", "could not kill Redis: %v", err)
	}

	// Step 3: Poll until passthrough mode kicks in.
	info("  Waiting for EdgeQuota to enter passthrough mode...")
	if err := pollUntil(30*time.Second, "passthrough after Redis kill", func() bool {
		ok200pt, _ := sendBurst(base, nil, 5)
		return ok200pt >= 4
	}); err != nil {
		return fail("recovery", "step 3: did not enter passthrough: %v", err)
	}

	// Step 4: Wait for Redis to come back (Deployment auto-recreates the pod).
	info("  Waiting for Redis single pod to restart...")
	if err := waitForPods("app=redis-single", 90*time.Second); err != nil {
		return fail("recovery", "Redis pod did not restart: %v", err)
	}

	// Step 5: Poll for recovery — the recovery loop reconnects with backoff.
	info("  Waiting for EdgeQuota to recover Redis connection...")
	if err := pollUntil(60*time.Second, "Redis-backed limiting recovered (429s)", func() bool {
		_, ok429r := sendBurst(base, nil, 15)
		return ok429r > 0
	}); err != nil {
		return fail("recovery", "step 5: EdgeQuota did not recover: %v", err)
	}

	ok200r, ok429r := sendBurst(base, nil, 15)
	return pass("recovery", "recovered: %d allowed, %d limited after Redis restart", ok200r, ok429r)
}

func testConcurrentBurst(base string) testResult {
	// Send 50 concurrent requests.
	var ok200, ok429, errors int64

	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			resp, err := doHTTPRequest(base, nil)
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}

			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			switch resp.StatusCode {
			case 200:
				atomic.AddInt64(&ok200, 1)
			case 429:
				atomic.AddInt64(&ok429, 1)
			default:
				atomic.AddInt64(&errors, 1)
			}
		}()
	}

	wg.Wait()

	if errors == 0 {
		return pass("concurrent", "50 concurrent: %d allowed, %d limited, 0 errors", ok200, ok429)
	}

	return fail("concurrent", "50 concurrent: %d allowed, %d limited, %d errors (500s)", ok200, ok429, errors)
}

// ---------------------------------------------------------------------------
// Protocol tests — no rate limit
// ---------------------------------------------------------------------------

func testProtocolHTTP(base string) testResult {
	resp, err := doHTTPRequest(base, nil)
	if err != nil {
		return fail("proto-http", "request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("proto-http", "expected 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("X-Backend") != "testbackend" {
		return fail("proto-http", "X-Backend header missing — not reaching testbackend")
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fail("proto-http", "cannot decode JSON body: %v", err)
	}

	return pass("proto-http", "HTTP/1.1 proxied: method=%s path=%s proto=%s", body["method"], body["path"], body["protocol"])
}

func testProtocolHTTP2(base string) testResult {
	resp, err := doHTTP2Request(base)
	if err != nil {
		return fail("proto-http2", "request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("proto-http2", "expected 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("X-Backend") != "testbackend" {
		return fail("proto-http2", "X-Backend header missing — not reaching testbackend")
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fail("proto-http2", "cannot decode JSON body: %v", err)
	}

	// Verify the backend saw HTTP/2 (h2c).
	proto := body["protocol"]
	if !strings.HasPrefix(proto, "HTTP/2") {
		return fail("proto-http2", "backend saw %s, expected HTTP/2.x", proto)
	}

	return pass("proto-http2", "HTTP/2 (h2c) proxied: method=%s path=%s proto=%s", body["method"], body["path"], proto)
}

func testProtocolHTTP3(base string) testResult {
	// HTTP/3 client using QUIC — connects directly to the minikube VM IP + NodePort (UDP).
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // e2e self-signed certs
	h3Client := &http.Client{
		Transport: &http3.Transport{TLSClientConfig: tlsCfg},
		Timeout:   10 * time.Second,
	}

	resp, err := h3Client.Get(base + "/")
	if err != nil {
		return fail("proto-h3", "HTTP/3 request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fail("proto-h3", "expected 200, got %d, body: %s", resp.StatusCode, body)
	}

	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		return fail("proto-h3", "cannot decode response: %v", err)
	}

	return pass("proto-h3", "HTTP/3 (QUIC) proxied: proto=%s, status=%d", m["protocol"], resp.StatusCode)
}

func testProtocolHTTPS(base string) testResult {
	// HTTPS (HTTP/2 over TLS) client — connects directly via TCP.
	// http2.ConfigureTransport is required so the transport advertises "h2"
	// in the TLS ALPN extension; a bare http.Transport with a custom
	// TLSClientConfig does NOT negotiate HTTP/2 by default.
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // e2e self-signed certs
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	if err := http2.ConfigureTransport(tr); err != nil {
		return fail("proto-https", "failed to configure h2 transport: %v", err)
	}
	h2Client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}

	resp, err := h2Client.Get(base + "/")
	if err != nil {
		return fail("proto-https", "HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fail("proto-https", "expected 200, got %d", resp.StatusCode)
	}

	if !strings.HasPrefix(resp.Proto, "HTTP/2") {
		return fail("proto-https", "expected HTTP/2 via ALPN, got %s", resp.Proto)
	}

	altSvc := resp.Header.Get("Alt-Svc")
	detail := fmt.Sprintf("HTTPS proxied: proto=%s, status=%d", resp.Proto, resp.StatusCode)
	if altSvc != "" {
		detail += fmt.Sprintf(", Alt-Svc=%s", altSvc)
	}

	return pass("proto-https", "%s", detail)
}

func testProtocolGRPC(base string) testResult {
	// Extract host:port from base URL.
	addr := strings.TrimPrefix(base, "http://")

	resp, err := doGRPCEchoCall(addr, "hello-e2e")
	if err != nil {
		return fail("proto-grpc", "gRPC call failed: %v", err)
	}

	if resp != "echo:hello-e2e" {
		return fail("proto-grpc", "unexpected response: %q", resp)
	}

	return pass("proto-grpc", "gRPC echo: sent 'hello-e2e', got '%s'", resp)
}

func testProtocolSSE(base string) testResult {
	events, err := doSSERequest(base+"/sse", 5*time.Second)
	if err != nil {
		return fail("proto-sse", "SSE request failed: %v", err)
	}

	if len(events) < 3 {
		return fail("proto-sse", "expected ≥3 SSE events, got %d", len(events))
	}

	return pass("proto-sse", "received %d SSE events", len(events))
}

func testProtocolWebSocket(base string) testResult {
	wsURL := strings.Replace(base, "http://", "ws://", 1) + "/ws"

	reply, err := doWebSocketEcho(wsURL, base, "hello-ws-e2e")
	if err != nil {
		return fail("proto-ws", "WebSocket failed: %v", err)
	}

	if reply != "echo:hello-ws-e2e" {
		return fail("proto-ws", "unexpected reply: %q", reply)
	}

	return pass("proto-ws", "WebSocket echo: sent 'hello-ws-e2e', got '%s'", reply)
}

// ---------------------------------------------------------------------------
// Protocol tests — with rate limiting
// ---------------------------------------------------------------------------

func testProtocolGRPCRateLimited(base string) testResult {
	addr := strings.TrimPrefix(base, "http://")

	// Send requests until we exhaust the burst (average=2, burst=3).
	var okCount, limitedCount int
	for i := 0; i < 10; i++ {
		_, err := doGRPCEchoCall(addr, fmt.Sprintf("msg-%d", i))
		if err != nil {
			st, ok := grpcstatus.FromError(err)
			if ok && st.Code() == 14 { // Unavailable = rate limited
				limitedCount++
				continue
			}
			// Could also be ResourceExhausted.
			limitedCount++
			continue
		}
		okCount++
	}

	if limitedCount > 0 {
		return pass("proto-grpc-rl", "gRPC rate limited: %d ok, %d limited", okCount, limitedCount)
	}

	return fail("proto-grpc-rl", "expected some gRPC rate limiting, got %d ok, %d limited", okCount, limitedCount)
}

func testProtocolSSERateLimited(base string) testResult {
	// SSE should connect fine within burst.
	events, err := doSSERequest(base+"/sse", 5*time.Second)
	if err != nil {
		return fail("proto-sse-rl", "SSE request failed: %v", err)
	}

	if len(events) >= 1 {
		return pass("proto-sse-rl", "SSE worked within rate limit: %d events", len(events))
	}

	return fail("proto-sse-rl", "expected ≥1 SSE event, got %d", len(events))
}

func testProtocolWebSocketRateLimited(base string) testResult {
	wsURL := strings.Replace(base, "http://", "ws://", 1) + "/ws"

	// WebSocket should connect fine within burst.
	reply, err := doWebSocketEcho(wsURL, base, "ws-rl-test")
	if err != nil {
		return fail("proto-ws-rl", "WebSocket failed: %v", err)
	}

	if reply == "echo:ws-rl-test" {
		return pass("proto-ws-rl", "WebSocket worked within rate limit")
	}

	return fail("proto-ws-rl", "unexpected reply: %q", reply)
}

// ---------------------------------------------------------------------------
// gRPC helpers (JSON codec, no proto generation)
// ---------------------------------------------------------------------------

// jsonCodec is a gRPC codec that uses JSON for serialization.
// Implements encoding.Codec (legacy interface used by grpc.ForceCodec).
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return "json" }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}

type echoRequest struct {
	Message string `json:"message"`
}

type echoResponse struct {
	Message string `json:"message"`
}

func doGRPCEchoCall(addr, message string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(jsonCodec{})),
	)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	var resp echoResponse
	err = conn.Invoke(ctx, "/echo.v1.EchoService/Echo", &echoRequest{Message: message}, &resp)
	if err != nil {
		return "", err
	}

	return resp.Message, nil
}

// ---------------------------------------------------------------------------
// SSE helpers
// ---------------------------------------------------------------------------

func doSSERequest(url string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0} // No client timeout — rely on ctx.
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("SSE returned %d", resp.StatusCode)
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}

	return events, nil
}

// ---------------------------------------------------------------------------
// WebSocket helpers
// ---------------------------------------------------------------------------

func doWebSocketEcho(wsURL, origin, message string) (string, error) {
	ws, err := websocket.Dial(wsURL, "", origin)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()

	if err := websocket.Message.Send(ws, message); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}

	var reply string
	if err := websocket.Message.Receive(ws, &reply); err != nil {
		return "", fmt.Errorf("receive: %w", err)
	}

	return reply, nil
}

// ---------------------------------------------------------------------------
// HTTP/2 helpers (cleartext h2c)
// ---------------------------------------------------------------------------

func doHTTP2Request(base string) (*http.Response, error) {
	// Use h2c (HTTP/2 over cleartext) transport — this is exactly what the
	// EdgeQuota proxy supports for non-TLS HTTP/2 connections.
	h2t := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			// Dial plain TCP (no TLS) for h2c.
			return net.DialTimeout(network, addr, 10*time.Second)
		},
	}
	client := &http.Client{
		Transport: h2t,
		Timeout:   10 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		return nil, err
	}

	return client.Do(req)
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func doHTTPRequest(base string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}

	return client.Do(req)
}

func sendBurst(base string, headers map[string]string, count int) (ok200, ok429 int) {
	for i := 0; i < count; i++ {
		resp, err := doHTTPRequest(base, headers)
		if err != nil {
			continue
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		switch resp.StatusCode {
		case 200:
			ok200++
		case 429:
			ok429++
		}
	}

	return
}

func sendBurstDetailed(base string, headers map[string]string, count int) (ok200, ok429 int, codes []int) {
	codes = make([]int, 0, count)

	for i := 0; i < count; i++ {
		resp, err := doHTTPRequest(base, headers)
		if err != nil {
			codes = append(codes, 0)
			continue
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		codes = append(codes, resp.StatusCode)

		switch resp.StatusCode {
		case 200:
			ok200++
		case 429:
			ok429++
		}
	}

	return
}

func waitForEdgeQuota(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	// Use a TLS-insecure client for HTTPS endpoints with self-signed certs.
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // e2e self-signed certs
		},
	}

	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/")
		if err == nil {
			resp.Body.Close()
			// Any HTTP response (including 4xx/5xx) means the server is up.
			// A 500 from e.g. missing key-strategy header is an application
			// error, not a connectivity failure.
			return nil
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("EdgeQuota not responding at %s after %s", base, timeout)
}

// ---------------------------------------------------------------------------
// Result helpers
// ---------------------------------------------------------------------------

func pass(name, format string, args ...any) testResult {
	return testResult{name: name, passed: true, detail: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) testResult {
	return testResult{name: name, passed: false, detail: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// Host/domain forwarding test — Traefik gateway compatibility
// ---------------------------------------------------------------------------

// testHostHeaderForwarding verifies that when a request flows through
// EdgeQuota, the original Host header is preserved for the backend (e.g.
// Traefik) so that host-based routing (IngressRoute, Gateway API) works
// correctly. It also checks that X-Forwarded-Host and X-Forwarded-Proto
// are set.
func testHostHeaderForwarding(base string) testResult {
	const name = "host-fwd"

	// Send a request with a custom Host header simulating a real domain.
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		return fail(name, "create request: %v", err)
	}
	req.Host = "app.example.com"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, clientErr := client.Do(req)
	if clientErr != nil {
		return fail(name, "request error: %v", clientErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail(name, "expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return fail(name, "cannot decode JSON: %v", decErr)
	}

	// The backend must see the original Host header for upstream routing.
	backendHost := body["host"]
	if backendHost != "app.example.com" {
		return fail(name, "Host not preserved: backend saw %q, expected 'app.example.com'", backendHost)
	}

	// X-Forwarded-Host must be set to the original Host.
	xfh := body["x-forwarded-host"]
	if xfh != "app.example.com" {
		return fail(name, "X-Forwarded-Host incorrect: got %q, expected 'app.example.com'", xfh)
	}

	// X-Forwarded-Proto must be set.
	xfp := body["x-forwarded-proto"]
	if xfp == "" {
		return fail(name, "X-Forwarded-Proto not set")
	}

	return pass(name, "Host preserved='%s', X-Forwarded-Host='%s', X-Forwarded-Proto='%s'",
		backendHost, xfh, xfp)
}

// ---------------------------------------------------------------------------
// Dynamic backend URL tests (tenant-aware routing)
// ---------------------------------------------------------------------------

func testDynamicBackendTenantA(base string) testResult {
	// tenant-a should be routed to whoami (backend_a_url in the mock service).
	// whoami does NOT set X-Backend header and returns plaintext.
	headers := map[string]string{"X-Tenant-Id": "tenant-a"}

	resp, err := doHTTPRequest(base, headers)
	if err != nil {
		return fail("dyn-tenant-a", "request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("dyn-tenant-a", "expected 200, got %d", resp.StatusCode)
	}

	// whoami does not set X-Backend header.
	if resp.Header.Get("X-Backend") == "testbackend" {
		return fail("dyn-tenant-a", "routed to testbackend instead of whoami")
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// whoami returns plaintext containing "Hostname:" or "GET / HTTP/".
	if strings.Contains(bodyStr, "Hostname:") || strings.Contains(bodyStr, "GET") {
		return pass("dyn-tenant-a", "tenant-a routed to whoami backend")
	}

	return fail("dyn-tenant-a", "unexpected response body (not whoami): %s", truncate(bodyStr, 200))
}

func testDynamicBackendTenantB(base string) testResult {
	// tenant-b should be routed to testbackend (backend_b_url in the mock service).
	// testbackend sets X-Backend: testbackend and returns JSON.
	headers := map[string]string{"X-Tenant-Id": "tenant-b"}

	resp, err := doHTTPRequest(base, headers)
	if err != nil {
		return fail("dyn-tenant-b", "request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("dyn-tenant-b", "expected 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("X-Backend") != "testbackend" {
		return fail("dyn-tenant-b", "expected X-Backend: testbackend, got %q", resp.Header.Get("X-Backend"))
	}

	return pass("dyn-tenant-b", "tenant-b routed to testbackend")
}

func testDynamicBackendDefault(base string) testResult {
	// An unknown tenant should use the default backend (whoami).
	// The mock external RL service returns no backend_url for unknown keys.
	headers := map[string]string{"X-Tenant-Id": "tenant-unknown"}

	resp, err := doHTTPRequest(base, headers)
	if err != nil {
		return fail("dyn-default", "request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("dyn-default", "expected 200, got %d", resp.StatusCode)
	}

	// Default backend is whoami — no X-Backend: testbackend header.
	if resp.Header.Get("X-Backend") == "testbackend" {
		return fail("dyn-default", "unknown tenant was routed to testbackend instead of default (whoami)")
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if strings.Contains(bodyStr, "Hostname:") || strings.Contains(bodyStr, "GET") {
		return pass("dyn-default", "unknown tenant routed to default backend (whoami)")
	}

	return fail("dyn-default", "unexpected response body: %s", truncate(bodyStr, 200))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ---------------------------------------------------------------------------
// Config hot-reload tests
// ---------------------------------------------------------------------------

// testConfigReload verifies that rate-limit parameters change after the
// ConfigMap is patched and the file-watcher triggers a reload.
func testConfigReload(base string) testResult {
	const name = "config-reload"

	// 1. Verify initial limits work (average=100, burst=50 — should allow 50).
	ok200, _ := sendBurst(base, nil, 55)
	if ok200 < 40 {
		return fail(name, "pre-reload burst too low: %d/55 (expected >=40 with burst=50)", ok200)
	}

	// 2. Patch the ConfigMap to lower the limits drastically (average=5, burst=3).
	patchJSON := `{"data":{"config.yaml":"server:\n  address: \":8080\"\n  read_timeout: \"30s\"\n  write_timeout: \"30s\"\n  idle_timeout: \"120s\"\n  drain_timeout: \"5s\"\nadmin:\n  address: \":9090\"\nbackend:\n  url: \"http://whoami.edgequota-e2e.svc.cluster.local:80\"\n  timeout: \"10s\"\n  max_idle_conns: 50\n  idle_conn_timeout: \"60s\"\nrate_limit:\n  average: 5\n  burst: 3\n  period: \"1s\"\n  failure_policy: \"passThrough\"\n  key_prefix: \"config-reload\"\n  key_strategy:\n    type: \"clientIP\"\nredis:\n  endpoints:\n    - \"redis-single.edgequota-e2e.svc.cluster.local:6379\"\n  mode: \"single\"\n  pool_size: 5\n  dial_timeout: \"3s\"\n  read_timeout: \"2s\"\n  write_timeout: \"2s\"\nlogging:\n  level: \"debug\"\n  format: \"json\"\n"}}`

	_, err := kubectl("patch", "configmap", "edgequota-config-reload", "-n", namespace,
		"--type=merge", "-p", patchJSON)
	if err != nil {
		return fail(name, "failed to patch ConfigMap: %v", err)
	}

	// 3. Poll until the new rate limits take effect. Kubernetes ConfigMap
	//    volume propagation can take up to ~60s (syncFrequency + cache TTL),
	//    plus the watcher poll interval and debounce.
	deadline := time.Now().Add(90 * time.Second)
	var ok200After, ok429After int
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		ok200After, ok429After = sendBurst(base, nil, 10)
		if ok429After > 0 {
			break
		}
	}

	if ok429After == 0 {
		return fail(name, "post-reload: expected 429s but got 0 (ok200=%d)", ok200After)
	}
	if ok200After > 5 {
		return fail(name, "post-reload: too many allowed=%d (expected <=5 with burst=3)", ok200After)
	}

	return pass(name, "pre-reload: %d/55 allowed; post-reload: %d/10 allowed, %d/10 limited", ok200, ok200After, ok429After)
}

// testCertReload verifies that TLS certificates can be rotated without restart.
func testCertReload(base string) testResult {
	const name = "cert-reload"

	// 1. Connect and capture the current certificate serial.
	serial1, err := getTLSSerial(base)
	if err != nil {
		return fail(name, "initial TLS connect failed: %v", err)
	}

	// 2. Generate a new self-signed cert and update the TLS Secret.
	// Uses a two-step openssl flow (ecparam + req) that works across
	// OpenSSL 1.x and 3.x — the single-command "-newkey ec -pkeyopt"
	// form produces invalid ECDSA parameters on OpenSSL 3.x.
	ip := strings.Split(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), ":")[0]
	_, genErr := run("bash", "-c", fmt.Sprintf(
		`openssl ecparam -name prime256v1 -genkey -noout -out /tmp/e2e-newkey.pem && `+
			`openssl req -new -x509 -key /tmp/e2e-newkey.pem `+
			`-out /tmp/e2e-newcert.pem -days 1 `+
			`-subj '/CN=edgequota-e2e' -addext 'subjectAltName=DNS:localhost,IP:%s' 2>/dev/null`, ip,
	))
	if genErr != nil {
		return fail(name, "openssl cert generation failed: %v", genErr)
	}

	_, applyErr := run("bash", "-c",
		`kubectl create secret tls edgequota-tls -n `+namespace+
			` --cert=/tmp/e2e-newcert.pem --key=/tmp/e2e-newkey.pem `+
			`--dry-run=client -o yaml | kubectl apply -f -`)
	if applyErr != nil {
		return fail(name, "failed to update TLS secret: %v", applyErr)
	}

	// 3. Wait for Kubelet to propagate the Secret update and the cert
	//    watcher to detect the change. Kubelet syncs projected volumes
	//    periodically (default ~60s); the cert watcher polls every 2s.
	//    Use a poll loop with a generous timeout to avoid flakes.
	deadline := time.Now().Add(90 * time.Second)
	var serial2 *big.Int
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		serial2, err = getTLSSerial(base)
		if err != nil {
			continue // TLS handshake may briefly fail during reload.
		}
		if serial1.Cmp(serial2) != 0 {
			break
		}
	}

	if serial2 == nil {
		return fail(name, "post-reload TLS connect never succeeded within timeout")
	}
	if serial1.Cmp(serial2) == 0 {
		return fail(name, "certificate serial unchanged after secret update (serial=%s)", serial1.String())
	}

	return pass(name, "serial changed: %s → %s", serial1.String(), serial2.String())
}

// getTLSSerial connects to the given HTTPS URL and returns the server
// certificate's serial number.
func getTLSSerial(base string) (*big.Int, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	// Separate host:port
	tlsHost := host
	if !strings.Contains(tlsHost, ":") {
		tlsHost = tlsHost + ":443"
	}

	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp", tlsHost,
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // E2E test with self-signed certs.
	)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no peer certificates received")
	}
	return certs[0].SerialNumber, nil
}
