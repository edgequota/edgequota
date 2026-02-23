// Package main is a multi-protocol test backend for EdgeQuota e2e tests.
// It serves HTTP, gRPC (over h2c), SSE, and WebSocket on port 8080 (cleartext),
// and HTTPS + HTTP/3 (QUIC) on port 8443 with self-signed TLS certificates.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
)

// jsonCodec is a gRPC codec that uses JSON for serialization, allowing clients
// to call RPC methods with application/grpc+json content-type.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return "json" }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}

func main() {
	mux := http.NewServeMux()

	// HTTP: simple echo/health endpoint.
	mux.HandleFunc("/", handleHTTP)
	mux.HandleFunc("/health", handleHealth)

	// SSE: stream of events.
	mux.HandleFunc("/sse", handleSSE)

	// WebSocket: echo server.
	mux.Handle("/ws", websocket.Handler(handleWebSocket))

	// Cache-test endpoints: each returns a unique call counter so the
	// e2e tests can distinguish cached vs. fresh responses.
	mux.HandleFunc("/cache/static", handleCacheStatic)
	mux.HandleFunc("/cache/no-store", handleCacheNoStore)
	mux.HandleFunc("/cache/tagged", handleCacheTagged)
	mux.HandleFunc("/cache/large", handleCacheLarge)

	// gRPC: register an echo service.
	grpcServer := grpc.NewServer()
	RegisterEchoServer(grpcServer)

	// Route gRPC vs HTTP based on Content-Type.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	})

	// ---------------------------------------------------------------------------
	// Cleartext server (HTTP/1.1 + HTTP/2 h2c + gRPC + SSE + WebSocket) on :8080
	// ---------------------------------------------------------------------------
	h2s := &http2.Server{}
	cleartextServer := &http.Server{
		Addr:    ":8080",
		Handler: h2c.NewHandler(handler, h2s),
	}

	// ---------------------------------------------------------------------------
	// TLS + HTTP/3 server on :8443
	// ---------------------------------------------------------------------------
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("generate TLS cert: %v", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h3", "h2", "http/1.1"},
	}

	tlsServer := &http.Server{
		Addr:      ":8443",
		Handler:   handler,
		TLSConfig: tlsCfg,
	}

	h3Server := &http3.Server{
		Addr:      ":8443",
		Handler:   handler,
		TLSConfig: http3.ConfigureTLSConfig(tlsCfg),
	}

	// Set Alt-Svc header on TLS responses so clients discover HTTP/3.
	origHandler := tlsServer.Handler
	tlsServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 3 {
			if setErr := h3Server.SetQUICHeaders(w.Header()); setErr != nil {
				log.Printf("set Alt-Svc: %v", setErr)
			}
		}
		origHandler.ServeHTTP(w, r)
	})

	// ---------------------------------------------------------------------------
	// Start all servers
	// ---------------------------------------------------------------------------
	log.Printf("testbackend: cleartext on :8080 (HTTP/1.1 + h2c + gRPC + SSE + WS)")
	log.Printf("testbackend: TLS on :8443 (HTTPS + HTTP/2 + HTTP/3)")

	errCh := make(chan error, 3)

	go func() {
		if srvErr := cleartextServer.ListenAndServe(); srvErr != nil && srvErr != http.ErrServerClosed {
			errCh <- fmt.Errorf("cleartext: %w", srvErr)
		}
	}()

	go func() {
		ln, listenErr := tls.Listen("tcp", ":8443", tlsCfg)
		if listenErr != nil {
			errCh <- fmt.Errorf("tls listen: %w", listenErr)
			return
		}
		if srvErr := tlsServer.Serve(ln); srvErr != nil && srvErr != http.ErrServerClosed {
			errCh <- fmt.Errorf("tls: %w", srvErr)
		}
	}()

	go func() {
		if srvErr := h3Server.ListenAndServe(); srvErr != nil && srvErr != http.ErrServerClosed {
			errCh <- fmt.Errorf("http3: %w", srvErr)
		}
	}()

	log.Fatalf("server: %v", <-errCh)
}

// ---------------------------------------------------------------------------
// Cache-test handlers — each has an atomic counter to verify backend hits
// ---------------------------------------------------------------------------

var (
	cacheStaticCounter  atomic.Int64
	cacheNoStoreCounter atomic.Int64
	cacheTaggedCounter  atomic.Int64
	cacheLargeCounter   atomic.Int64
)

func handleCacheStatic(w http.ResponseWriter, _ *http.Request) {
	n := cacheStaticCounter.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", "testbackend")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"call": n, "endpoint": "cache-static"})
}

func handleCacheNoStore(w http.ResponseWriter, _ *http.Request) {
	n := cacheNoStoreCounter.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", "testbackend")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"call": n, "endpoint": "cache-no-store"})
}

func handleCacheTagged(w http.ResponseWriter, _ *http.Request) {
	n := cacheTaggedCounter.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", "testbackend")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Surrogate-Key", "product-123 category-electronics")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"call": n, "endpoint": "cache-tagged"})
}

func handleCacheLarge(w http.ResponseWriter, _ *http.Request) {
	n := cacheLargeCounter.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", "testbackend")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	// ~2 MB payload: well above the default 1 MB max_body_size.
	padding := strings.Repeat("X", 2*1024*1024)
	json.NewEncoder(w).Encode(map[string]any{"call": n, "endpoint": "cache-large", "padding": padding})
}

// ---------------------------------------------------------------------------
// Self-signed certificate generation
// ---------------------------------------------------------------------------

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "testbackend"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"testbackend", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", "testbackend")
	w.WriteHeader(http.StatusOK)
	resp := map[string]string{
		"protocol":          r.Proto,
		"method":            r.Method,
		"path":              r.URL.Path,
		"host":              r.Host,
		"remote":            r.RemoteAddr,
		"x-forwarded-host":  r.Header.Get("X-Forwarded-Host"),
		"x-forwarded-for":   r.Header.Get("X-Forwarded-For"),
		"x-forwarded-proto": r.Header.Get("X-Forwarded-Proto"),
	}
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// ---------------------------------------------------------------------------
// SSE handler
// ---------------------------------------------------------------------------

func handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Backend", "testbackend")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Send 5 events then close.
	for i := 1; i <= 5; i++ {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		fmt.Fprintf(w, "id: %d\nevent: ping\ndata: {\"seq\":%d}\n\n", i, i)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// WebSocket handler (echo)
// ---------------------------------------------------------------------------

func handleWebSocket(ws *websocket.Conn) {
	defer ws.Close()
	for {
		var msg string
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			if err != io.EOF {
				log.Printf("ws recv: %v", err)
			}
			return
		}
		reply := "echo:" + msg
		if err := websocket.Message.Send(ws, reply); err != nil {
			log.Printf("ws send: %v", err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// gRPC echo service (manual registration, no protoc needed)
// ---------------------------------------------------------------------------

const echoServiceName = "echo.v1.EchoService"

// echoServiceServer is the interface that HandlerType requires for gRPC registration.
// gRPC's RegisterService checks via reflect.Type.Implements, which requires an interface type.
type echoServiceServer interface{}

type echoServer struct{}

// RegisterEchoServer registers a unary Echo RPC on the gRPC server.
// The RPC expects a JSON-encoded request body with a "message" field and
// returns JSON with "message" echoed back.
func RegisterEchoServer(s *grpc.Server) {
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: echoServiceName,
		HandlerType: (*echoServiceServer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Echo",
				Handler:    echoHandler,
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "echo.proto",
	}, &echoServer{})
}

func echoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	var req echoRequest
	if err := dec(&req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode: %v", err)
	}
	return &echoResponse{Message: "echo:" + req.Message}, nil
}

// echoRequest / echoResponse use a JSON codec so we don't need proto generation.
type echoRequest struct {
	Message string `json:"message"`
}

type echoResponse struct {
	Message string `json:"message"`
}

// Reset / String / ProtoMessage satisfy the proto.Message interface minimally.
func (r *echoRequest) Reset()         {}
func (r *echoRequest) String() string { return r.Message }
func (r *echoRequest) ProtoMessage()  {}

func (r *echoResponse) Reset()         {}
func (r *echoResponse) String() string { return r.Message }
func (r *echoResponse) ProtoMessage()  {}
