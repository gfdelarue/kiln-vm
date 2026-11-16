package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/acme/autocert"

	"github.com/gfdelarue/kiln-vm/internal/auth"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/metrics"
	"github.com/gfdelarue/kiln-vm/internal/store"
	"github.com/gfdelarue/kiln-vm/internal/version"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func TestShutdownCancelsActiveRequests(t *testing.T) {
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	handlerStarted := make(chan struct{})
	handlerExited := make(chan struct{})

	srv := newHTTPServer(requestCtx, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-r.Context().Done()
		close(handlerExited)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	daemon := &Server{
		httpSrv:        srv,
		mgr:            &machine.Manager{},
		cancelRequests: cancelRequests,
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	if err := daemon.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := daemon.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	select {
	case <-handlerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after shutdown")
	}

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
}

func TestShutdownForceClosesConnectionsWhenGracefulDeadlineExpires(t *testing.T) {
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	srvHTTP := newHTTPServer(requestCtx, "", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(handlerStarted)
		<-releaseHandler
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	bound := &BoundListeners{management: listener}
	srv := &Server{
		httpSrv:        srvHTTP,
		httpListener:   listener,
		listeners:      bound,
		cancelRequests: cancelRequests,
	}
	startResult := make(chan error, 1)
	go func() { startResult <- srv.StartWithReady(nil) }()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		close(releaseHandler)
		t.Fatal("handler did not start")
	}

	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err = srv.Shutdown(shutdownCtx)
	if !errors.Is(err, context.Canceled) {
		close(releaseHandler)
		t.Fatalf("Shutdown error = %v, want context cancellation", err)
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		close(releaseHandler)
		t.Fatal("force close did not release the active client connection")
	}
	if conn, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond); dialErr == nil {
		conn.Close()
		close(releaseHandler)
		t.Fatal("listener remained reachable after timed-out shutdown")
	}
	if secondErr := srv.Shutdown(context.Background()); !errors.Is(secondErr, context.Canceled) {
		close(releaseHandler)
		t.Fatalf("second Shutdown error = %v, want memoized context cancellation", secondErr)
	}
	close(releaseHandler)
	select {
	case startErr := <-startResult:
		if startErr != nil {
			t.Fatalf("Start returned after shutdown: %v", startErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after forced shutdown")
	}
}

func TestBindListenersACMEFailureReleasesTLSListener(t *testing.T) {
	tlsAddr := reserveUnusedTCPAddr(t)
	occupiedACME, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy ACME listener: %v", err)
	}
	defer occupiedACME.Close()

	listeners, err := BindListeners(&config.Config{
		TLSEnabled:     true,
		TLSListenAddr:  tlsAddr,
		ACMEListenAddr: occupiedACME.Addr().String(),
	})
	if err == nil {
		listeners.Close()
		t.Fatal("BindListeners succeeded with an unavailable required ACME listener")
	}
	if !strings.Contains(err.Error(), "ACME") {
		t.Fatalf("BindListeners error = %v, want ACME bind failure", err)
	}

	rebound, err := net.Listen("tcp", tlsAddr)
	if err != nil {
		t.Fatalf("TLS listener remained bound after ACME failure: %v", err)
	}
	rebound.Close()
}

func TestBindListenersRejectsRemotePlaintextWithoutExplicitConsent(t *testing.T) {
	listeners, err := BindListeners(&config.Config{ListenAddr: "0.0.0.0:0"})
	if err == nil {
		listeners.Close()
		t.Fatal("BindListeners accepted remote plaintext management without explicit consent")
	}
	if !errors.Is(err, ErrInvalidManagementTransport) {
		t.Fatalf("BindListeners error = %v, want invalid-management-transport classification", err)
	}
	if !strings.Contains(err.Error(), "KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true") {
		t.Fatalf("BindListeners error = %v, want explicit-consent remediation", err)
	}

	listeners, err = BindListeners(&config.Config{
		ListenAddr:              "0.0.0.0:0",
		AllowInsecureRemoteHTTP: true,
	})
	if err != nil {
		t.Fatalf("BindListeners with explicit remote plaintext consent: %v", err)
	}
	listeners.Close()
}

func TestStartTreatsConfiguredListenersAsOneFailureDomain(t *testing.T) {
	tlsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TLS: %v", err)
	}
	failingACME := &failingListener{
		addr: tlsListener.Addr(),
		err:  errors.New("synthetic ACME accept failure"),
	}
	tlsServer := newHTTPServer(context.Background(), tlsListener.Addr().String(), http.NotFoundHandler())
	tlsServer.TLSConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, errors.New("certificate unavailable in listener supervision test")
		},
	}
	srv := &Server{
		tlsSrv:       tlsServer,
		tlsListener:  tlsListener,
		acmeSrv:      newHTTPServer(context.Background(), "127.0.0.1:0", http.NotFoundHandler()),
		acmeListener: failingACME,
		listeners: &BoundListeners{
			management: tlsListener,
			acme:       failingACME,
		},
	}

	err = srv.StartWithReady(nil)
	if err == nil || !strings.Contains(err.Error(), "ACME server failed") {
		t.Fatalf("Start error = %v, want fatal ACME serve failure", err)
	}
	conn, dialErr := net.DialTimeout("tcp", tlsListener.Addr().String(), 100*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatal("TLS sibling listener remained reachable after ACME failure")
	}
}

func TestStartReadinessFailureTearsDownListenerGroup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	bound := &BoundListeners{management: listener}
	srv := &Server{
		httpSrv:      newHTTPServer(context.Background(), listener.Addr().String(), http.NotFoundHandler()),
		httpListener: listener,
		listeners:    bound,
	}
	readyErr := errors.New("synthetic readiness failure")
	err = srv.StartWithReady(func() error { return readyErr })
	if !errors.Is(err, readyErr) {
		t.Fatalf("StartWithReady error = %v, want readiness failure", err)
	}
	if conn, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond); dialErr == nil {
		conn.Close()
		t.Fatal("listener remained reachable after readiness failure")
	}
}

func TestHTTPSRedirectHandlerUsesConfiguredTLSPortAndNeverServesAPI(t *testing.T) {
	handler := authorizedHTTPSRedirectHandler(autocert.HostWhitelist("api.example.test"), "127.0.0.1:18443")
	req := httptest.NewRequest(http.MethodGet, "http://api.example.test:18080/api?generation=2", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "https://api.example.test:18443/api?generation=2" {
		t.Fatalf("redirect Location = %q", got)
	}
	if strings.Contains(rec.Body.String(), "api_contract") {
		t.Fatalf("plaintext redirect disclosed API response: %s", rec.Body.String())
	}

	postReq := httptest.NewRequest(http.MethodPost, "http://api.example.test:18080/api", nil)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusBadRequest {
		t.Fatalf("plaintext POST status = %d, want %d", postRec.Code, http.StatusBadRequest)
	}

	unknownReq := httptest.NewRequest(http.MethodGet, "http://attacker.example/api", nil)
	unknownRec := httptest.NewRecorder()
	handler.ServeHTTP(unknownRec, unknownReq)
	if unknownRec.Code != http.StatusNotFound {
		t.Fatalf("unknown-host redirect status = %d, want %d", unknownRec.Code, http.StatusNotFound)
	}
	if location := unknownRec.Header().Get("Location"); location != "" {
		t.Fatalf("unknown-host redirect Location = %q, want empty", location)
	}
}

func TestTLSModeServesAuthenticatedManagementOnlyOverTLS(t *testing.T) {
	domain := "api.kiln-vm.test"
	certDir := t.TempDir()
	rootCert := seedAutocertCache(t, certDir, domain)
	tlsAddr := reserveUnusedTCPAddr(t)
	acmeAddr := reserveUnusedTCPAddr(t)
	plaintextAddr := reserveUnusedTCPAddr(t)
	cfg := &config.Config{
		ListenAddr:     plaintextAddr,
		TLSEnabled:     true,
		TLSDomain:      domain,
		TLSCertDir:     certDir,
		TLSListenAddr:  tlsAddr,
		ACMEListenAddr: acmeAddr,
	}
	listeners, err := BindListeners(cfg)
	if err != nil {
		t.Fatalf("bind TLS listeners: %v", err)
	}

	const token = "tls-test-token"
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv, err := newManagementTransport(cfg, listeners, requestCtx, cancelRequests, apiHandler)
	if err != nil {
		listeners.Close()
		t.Fatalf("construct production TLS transport: %v", err)
	}
	startResult := make(chan error, 1)
	go func() { startResult <- srv.StartWithReady(nil) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case err := <-startResult:
			if err != nil {
				t.Errorf("TLS server returned during cleanup: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("TLS server did not stop during cleanup")
		}
	})

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootCert) {
		t.Fatal("append test root certificate")
	}
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: domain,
	}}}
	tlsReq, err := http.NewRequest(http.MethodGet, "https://"+tlsAddr+"/api", nil)
	if err != nil {
		t.Fatalf("new TLS request: %v", err)
	}
	tlsReq.Host = domain
	tlsReq.Header.Set("Authorization", "Bearer "+token)
	tlsResp, err := tlsClient.Do(tlsReq)
	if err != nil {
		t.Fatalf("TLS management request: %v", err)
	}
	defer tlsResp.Body.Close()
	if tlsResp.StatusCode != http.StatusOK {
		t.Fatalf("TLS management status = %d, want %d", tlsResp.StatusCode, http.StatusOK)
	}

	mismatchReq, err := http.NewRequest(http.MethodGet, "https://"+tlsAddr+"/api", nil)
	if err != nil {
		t.Fatalf("new mismatched TLS identity request: %v", err)
	}
	mismatchReq.Host = "other.kiln-vm.test"
	mismatchReq.Header.Set("Authorization", "Bearer "+token)
	mismatchResp, err := tlsClient.Do(mismatchReq)
	if err != nil {
		t.Fatalf("mismatched TLS identity request: %v", err)
	}
	defer mismatchResp.Body.Close()
	if mismatchResp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("mismatched TLS identity status = %d, want %d", mismatchResp.StatusCode, http.StatusMisdirectedRequest)
	}

	acmeClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	acmeReq, err := http.NewRequest(http.MethodGet, "http://"+acmeAddr+"/api", nil)
	if err != nil {
		t.Fatalf("new ACME request: %v", err)
	}
	acmeReq.Host = domain
	acmeReq.Header.Set("Authorization", "Bearer "+token)
	acmeResp, err := acmeClient.Do(acmeReq)
	if err != nil {
		t.Fatalf("ACME redirect request: %v", err)
	}
	defer acmeResp.Body.Close()
	if acmeResp.StatusCode != http.StatusFound {
		t.Fatalf("ACME plaintext status = %d, want redirect %d", acmeResp.StatusCode, http.StatusFound)
	}
	_, tlsPort, err := net.SplitHostPort(tlsAddr)
	if err != nil {
		t.Fatalf("parse TLS address: %v", err)
	}
	if got := acmeResp.Header.Get("Location"); got != "https://"+net.JoinHostPort(domain, tlsPort)+"/api" {
		t.Fatalf("ACME redirect Location = %q", got)
	}

	unauthorizedReq, err := http.NewRequest(http.MethodGet, "http://"+acmeAddr+"/api", nil)
	if err != nil {
		t.Fatalf("new unauthorized ACME fallback request: %v", err)
	}
	unauthorizedReq.Host = "attacker.example"
	unauthorizedResp, err := acmeClient.Do(unauthorizedReq)
	if err != nil {
		t.Fatalf("unauthorized ACME fallback request: %v", err)
	}
	defer unauthorizedResp.Body.Close()
	if unauthorizedResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthorized ACME fallback status = %d, want %d", unauthorizedResp.StatusCode, http.StatusNotFound)
	}

	conn, dialErr := net.DialTimeout("tcp", plaintextAddr, 100*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatal("TLS mode also exposed the configured plaintext management address")
	}
}

func TestTLSManagementListenerFailureDoesNotFallBackToPlaintext(t *testing.T) {
	occupiedTLS, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy TLS listener: %v", err)
	}
	defer occupiedTLS.Close()

	plaintextAddr := reserveUnusedTCPAddr(t)
	cfg := &config.Config{
		ListenAddr:     plaintextAddr,
		TLSEnabled:     true,
		TLSListenAddr:  occupiedTLS.Addr().String(),
		ACMEListenAddr: "127.0.0.1:0",
	}

	listeners, err := BindListeners(cfg)
	if err == nil {
		listeners.Close()
		t.Fatal("BindListeners succeeded after the TLS management listener failed")
	}
	if !strings.Contains(err.Error(), "TLS management") {
		t.Fatalf("BindListeners error = %v, want TLS management failure", err)
	}

	conn, dialErr := net.DialTimeout("tcp", plaintextAddr, 100*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatal("TLS management listener failure left the plaintext management listener running")
	}
}

func reserveUnusedTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved TCP address: %v", err)
	}
	return addr
}

type failingListener struct {
	addr net.Addr
	err  error
}

func (l *failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *failingListener) Close() error              { return nil }
func (l *failingListener) Addr() net.Addr            { return l.addr }

func seedAutocertCache(t *testing.T, certDir, domain string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate certificate serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domain},
		DNSNames:              []string{domain},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal certificate key: %v", err)
	}
	var cache bytes.Buffer
	if err := pem.Encode(&cache, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode certificate key: %v", err)
	}
	if err := pem.Encode(&cache, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encode certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, domain), cache.Bytes(), 0600); err != nil {
		t.Fatalf("seed autocert cache: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestWriteBootstrapTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "private", "bootstrap-token")

	if err := writeBootstrapTokenFile(tokenPath, "kiln_vm_test_token"); err != nil {
		t.Fatalf("writeBootstrapTokenFile: %v", err)
	}

	content, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if string(content) != "kiln_vm_test_token\n" {
		t.Fatalf("token content = %q", string(content))
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestHealthReportsVersion(t *testing.T) {
	router := chi.NewRouter()
	registerRoutes(router, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
	if body.Version != version.Version {
		t.Fatalf("version = %q, want %q", body.Version, version.Version)
	}
}

func TestMetricsRoutesAreRegisteredBehindAuth(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	authService := auth.NewService(db)
	token, err := authService.CreateToken("test", nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	cfg := &config.Config{
		DataDir:          dataDir,
		BridgeCIDR:       "172.16.0.0/16",
		MaxRunningVMs:    8,
		MaxTotalVCPUs:    8,
		MaxTotalMemoryMB: 8192,
	}
	mgr := machine.NewManager(cfg, db)
	imgService := image.NewService(db, dataDir, "")
	vmService := vmservice.NewService(db, imgService, mgr)
	t.Setenv("TMPDIR", t.TempDir())
	diskService := disk.NewService(db, dataDir, "")
	metricsService := metrics.NewService(cfg, mgr, vmService, diskService)

	router := chi.NewRouter()
	registerRoutes(router, authService, vmService, diskService, metricsService)

	req := httptest.NewRequest(http.MethodGet, "/node/capacity", nil)
	req.Header.Set("Authorization", "Bearer "+token.PlainToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("node capacity status = %d body=%s", rec.Code, rec.Body.String())
	}
	var capacity struct {
		MaxRunningVMs int `json:"max_running_vms"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&capacity); err != nil {
		t.Fatalf("decode capacity: %v", err)
	}
	if capacity.MaxRunningVMs != 8 {
		t.Fatalf("max_running_vms = %d, want 8", capacity.MaxRunningVMs)
	}

	req = httptest.NewRequest(http.MethodGet, "/vms/missing/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token.PlainToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("vm metrics status = %d body=%s; want route-level not-found, not router 404", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "404 page not found\n" {
		t.Fatalf("vm metrics route was not registered")
	}
}
