package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"golang.org/x/crypto/acme/autocert"

	"github.com/gfdelarue/kiln-vm/internal/auth"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/metrics"
	mw "github.com/gfdelarue/kiln-vm/internal/middleware"
	"github.com/gfdelarue/kiln-vm/internal/store"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

type Server struct {
	cfg            *config.Config
	httpSrv        *http.Server
	httpListener   net.Listener
	tlsSrv         *http.Server
	tlsListener    net.Listener
	acmeSrv        *http.Server
	acmeListener   net.Listener
	listeners      *BoundListeners
	mgr            *machine.Manager
	requestBaseCtx context.Context
	cancelRequests context.CancelFunc
	shuttingDown   atomic.Bool
	shutdownOnce   sync.Once
	shutdownErr    error
}

type BoundListeners struct {
	management net.Listener
	acme       net.Listener
	closeOnce  sync.Once
	closeErr   error
}

var ErrInvalidManagementTransport = errors.New("invalid management transport configuration")

func BindListeners(cfg *config.Config) (*BoundListeners, error) {
	if cfg == nil {
		return nil, fmt.Errorf("bind management listeners: configuration is nil")
	}

	managementAddr := cfg.ListenAddr
	managementName := "plaintext management"
	if cfg.TLSEnabled {
		managementAddr = cfg.TLSListenAddr
		managementName = "TLS management"
	}

	management, err := net.Listen("tcp", managementAddr)
	if err != nil {
		return nil, fmt.Errorf("bind %s listener %q: %w", managementName, managementAddr, err)
	}
	bound := &BoundListeners{management: management}

	if !cfg.TLSEnabled {
		if !cfg.AllowInsecureRemoteHTTP && !listenerIsLoopback(management) {
			_ = bound.Close()
			return nil, fmt.Errorf("%w: bound KILN_VM_LISTEN_ADDR %q to non-loopback address %s; use HTTPS or set KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true to explicitly allow remote plaintext management HTTP", ErrInvalidManagementTransport, cfg.ListenAddr, management.Addr())
		}
		return bound, nil
	}

	acme, err := net.Listen("tcp", cfg.ACMEListenAddr)
	if err != nil {
		_ = bound.Close()
		return nil, fmt.Errorf("bind ACME listener %q: %w", cfg.ACMEListenAddr, err)
	}
	bound.acme = acme
	return bound, nil
}

func (l *BoundListeners) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		var errs []error
		if l.management != nil {
			if err := l.management.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, fmt.Errorf("close management listener: %w", err))
			}
		}
		if l.acme != nil {
			if err := l.acme.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, fmt.Errorf("close ACME listener: %w", err))
			}
		}
		l.closeErr = errors.Join(errs...)
	})
	return l.closeErr
}

func listenerIsLoopback(listener net.Listener) bool {
	addr, ok := listener.Addr().(*net.TCPAddr)
	return ok && addr.IP != nil && addr.IP.IsLoopback()
}

func validateBoundListeners(cfg *config.Config, listeners *BoundListeners) error {
	if cfg == nil {
		return fmt.Errorf("initialize management transport: configuration is nil")
	}
	if listeners == nil || listeners.management == nil {
		return fmt.Errorf("initialize management transport: management listener is not bound")
	}
	if cfg.TLSEnabled && listeners.acme == nil {
		return fmt.Errorf("initialize management transport: ACME listener is not bound in TLS mode")
	}
	if !cfg.TLSEnabled && listeners.acme != nil {
		return fmt.Errorf("initialize management transport: unexpected ACME listener in plaintext mode")
	}
	if !cfg.TLSEnabled && !cfg.AllowInsecureRemoteHTTP && !listenerIsLoopback(listeners.management) {
		return fmt.Errorf("initialize management transport: plaintext management listener %s is not loopback", listeners.management.Addr())
	}
	return nil
}

func New(cfg *config.Config, db *store.DB, listeners *BoundListeners) (*Server, error) {
	if err := validateBoundListeners(cfg, listeners); err != nil {
		return nil, err
	}
	r := chi.NewRouter()
	requestBaseCtx, cancelRequests := context.WithCancel(context.Background())
	retainRequestContext := false
	defer func() {
		if !retainRequestContext {
			cancelRequests()
		}
	}()

	// Global middleware
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(mw.RequestLogger)
	r.Use(chimw.Recoverer)
	r.Use(mw.TimeoutExceptStreaming(60 * time.Second))

	authService := auth.NewService(db)
	mgr := machine.NewManager(cfg, db)

	imgService := image.NewService(db, cfg.DataDir, cfg.AgentBinPath)
	imgService.SetBuildTimeout(cfg.ImageBuildTimeout)
	imgService.SetBuildDir(cfg.ImageBuildDir)
	imgService.SetMinBuildFreeMB(int64(cfg.ImageMinFreeMB))
	imgService.CleanupStaging()
	vmService := vmservice.NewService(db, imgService, mgr)
	vmService.SetMaxDiskSizeMB(cfg.MaxVMDiskMB)
	windowsProfileDir := cfg.WindowsProfileDir
	if windowsProfileDir == "" {
		windowsProfileDir = filepath.Join(cfg.DataDir, "windows-profiles")
	}
	vmService.SetProfileInfrastructureConfig(windowsProfileDir, cfg.LibvirtConnectURI, cfg.LibvirtNetwork, cfg.LibvirtCIDR)
	profileImageToken, profileImageTokenSource, err := config.ResolveProfileImageToken(cfg.ProfileImageToken, cfg.ProfileImageTokenFile)
	if err != nil {
		slog.Error("failed to read VM profile image token", "source", profileImageTokenSource, "path", cfg.ProfileImageTokenFile, "error", err)
	}
	profileCatalog, err := vmservice.LoadProfileImageCatalogWithToken(cfg.ProfileImageCatalog, profileImageToken)
	if err != nil {
		slog.Error("failed to load VM profile image catalog", "path", cfg.ProfileImageCatalog, "error", err)
		profileCatalog = &vmservice.ProfileImageCatalog{SchemaVersion: 1}
	}
	if err := vmService.SetProfileImageSource(vmservice.ProfileImageSource{
		Mode:        cfg.ProfileImageMode,
		CatalogPath: cfg.ProfileImageCatalog,
		Token:       profileImageToken,
	}, profileCatalog); err != nil {
		slog.Error("failed to configure VM profile image source", "mode", cfg.ProfileImageMode, "error", err)
	}
	vmService.SetProfileImagePullTimeout(cfg.ProfileImagePullTimeout)
	vmService.SetProfileImagePullHeaderTimeout(cfg.ProfileImagePullHeaderTimeout)
	diskService := disk.NewService(
		db,
		cfg.DataDir,
		cfg.ImageBuildDir,
		disk.WithProfileImageCacheMaxBytes(uint64FromConfigMB(cfg.ProfileImageCacheMaxMB)),
	)
	metricsService := metrics.NewService(cfg, mgr, vmService, diskService)

	mgr.RecoverRunning()
	mgr.StartNetworkReconciler(requestBaseCtx)
	vmService.StartPowerManager(requestBaseCtx)

	token, err := authService.Bootstrap()
	if err != nil {
		slog.Error("failed to bootstrap auth", "error", err)
	} else if token != "" {
		if err := writeBootstrapTokenFile(cfg.BootstrapTokenFile, token); err != nil {
			slog.Error("failed to write bootstrap token file; logging the token instead", "path", cfg.BootstrapTokenFile, "error", err, "token", token)
		} else {
			slog.Info("wrote bootstrap token", "path", cfg.BootstrapTokenFile)
		}
	}

	registerRoutes(r, authService, vmService, diskService, metricsService)

	s, err := newManagementTransport(cfg, listeners, requestBaseCtx, cancelRequests, r)
	if err != nil {
		return nil, err
	}
	s.mgr = mgr
	retainRequestContext = true

	return s, nil
}

func newManagementTransport(
	cfg *config.Config,
	listeners *BoundListeners,
	requestBaseCtx context.Context,
	cancelRequests context.CancelFunc,
	handler http.Handler,
) (*Server, error) {
	if err := validateBoundListeners(cfg, listeners); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:            cfg,
		listeners:      listeners,
		requestBaseCtx: requestBaseCtx,
		cancelRequests: cancelRequests,
	}
	if handler == nil {
		return nil, fmt.Errorf("initialize management transport: handler is nil")
	}
	if !cfg.TLSEnabled {
		s.httpSrv = newHTTPServer(requestBaseCtx, cfg.ListenAddr, handler)
		s.httpListener = listeners.management
		return s, nil
	}

	hostPolicy := autocert.HostWhitelist(cfg.TLSDomain)

	certManager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: hostPolicy,
		Cache:      autocert.DirCache(cfg.TLSCertDir),
		Email:      cfg.ACMEEmail,
	}
	tlsHandler := &tlsHostHandler{
		domain:  cfg.TLSDomain,
		handler: handler,
	}
	s.tlsSrv = newHTTPServer(requestBaseCtx, cfg.TLSListenAddr, tlsHandler)
	s.tlsSrv.TLSConfig = &tls.Config{
		GetCertificate: certManager.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	redirectHandler := authorizedHTTPSRedirectHandler(hostPolicy, cfg.TLSListenAddr)
	s.acmeSrv = newHTTPServer(requestBaseCtx, cfg.ACMEListenAddr, certManager.HTTPHandler(redirectHandler))
	s.tlsListener = listeners.management
	s.acmeListener = listeners.acme
	return s, nil
}

func uint64FromConfigMB(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value) * 1024 * 1024
}

func writeBootstrapTokenFile(path string, token string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create bootstrap token directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return fmt.Errorf("write bootstrap token: %w", err)
	}
	return nil
}

// StartWithReady starts kiln-vm's selected transport and calls ready only after
// every serve loop has been launched. A readiness failure tears down the whole
// listener group and is returned like any other startup failure.
func (s *Server) StartWithReady(ready func() error) error {
	type serveTarget struct {
		name  string
		serve func() error
	}
	type serveResult struct {
		name string
		err  error
	}

	var targets []serveTarget
	if s.httpSrv != nil {
		if s.httpListener == nil {
			return fmt.Errorf("plaintext management listener was not bound before server initialization")
		}
		listener := s.httpListener
		targets = append(targets, serveTarget{
			name: "plaintext management",
			serve: func() error {
				slog.Info("starting plaintext management server", "addr", listener.Addr().String())
				return s.httpSrv.Serve(listener)
			},
		})
	}
	if s.tlsSrv != nil {
		if s.tlsListener == nil {
			return fmt.Errorf("TLS management listener was not bound before server initialization")
		}
		listener := s.tlsListener
		targets = append(targets, serveTarget{
			name: "TLS management",
			serve: func() error {
				slog.Info("starting TLS management server", "addr", listener.Addr().String())
				return s.tlsSrv.ServeTLS(listener, "", "")
			},
		})
	}
	if s.acmeSrv != nil {
		if s.acmeListener == nil {
			return fmt.Errorf("ACME listener was not bound before server initialization")
		}
		listener := s.acmeListener
		targets = append(targets, serveTarget{
			name: "ACME",
			serve: func() error {
				slog.Info("starting ACME server", "addr", listener.Addr().String())
				return s.acmeSrv.Serve(listener)
			},
		})
	}
	if len(targets) == 0 {
		return fmt.Errorf("no management listener configured")
	}

	results := make(chan serveResult, len(targets))
	for _, target := range targets {
		target := target
		go func() {
			results <- serveResult{name: target.name, err: target.serve()}
		}()
	}

	var serveErr error
	if ready != nil {
		if err := ready(); err != nil {
			serveErr = fmt.Errorf("signal management transport readiness: %w", err)
			s.closeHTTPServers()
			if s.listeners != nil {
				_ = s.listeners.Close()
			}
		}
	}
	for range targets {
		result := <-results
		if result.err == nil {
			if !s.shuttingDown.Load() && serveErr == nil {
				serveErr = fmt.Errorf("%s server stopped unexpectedly", result.name)
			}
		} else if !errors.Is(result.err, http.ErrServerClosed) && !errors.Is(result.err, net.ErrClosed) {
			if serveErr == nil {
				serveErr = fmt.Errorf("%s server failed: %w", result.name, result.err)
			}
		} else if !s.shuttingDown.Load() && serveErr == nil {
			serveErr = fmt.Errorf("%s server stopped unexpectedly: %w", result.name, result.err)
		}
		if serveErr != nil {
			s.closeHTTPServers()
			if s.listeners != nil {
				_ = s.listeners.Close()
			}
		}
	}
	return serveErr
}

// Shutdown gracefully stops the HTTP server then all running VMs.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shuttingDown.Store(true)
		slog.Info("shutting down kiln-vm servers")
		if s.cancelRequests != nil {
			s.cancelRequests()
		}

		var errs []error
		type shutdownResult struct {
			name string
			err  error
		}
		results := make(chan shutdownResult, 3)
		activeServers := 0
		for _, server := range []struct {
			name string
			srv  *http.Server
		}{
			{name: "plaintext management", srv: s.httpSrv},
			{name: "TLS management", srv: s.tlsSrv},
			{name: "ACME", srv: s.acmeSrv},
		} {
			if server.srv == nil {
				continue
			}
			activeServers++
			server := server
			go func() {
				err := server.srv.Shutdown(ctx)
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					if closeErr := server.srv.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
						err = errors.Join(err, fmt.Errorf("force close: %w", closeErr))
					}
					results <- shutdownResult{name: server.name, err: err}
					return
				}
				results <- shutdownResult{name: server.name}
			}()
		}
		for range activeServers {
			result := <-results
			if result.err != nil {
				errs = append(errs, fmt.Errorf("%s shutdown: %w", result.name, result.err))
			}
		}
		if s.listeners != nil {
			if err := s.listeners.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if s.mgr != nil {
			slog.Info("stopping all VMs")
			s.mgr.StopAll()
		}
		s.shutdownErr = errors.Join(errs...)
	})
	return s.shutdownErr
}

func (s *Server) closeHTTPServers() {
	for _, server := range []*http.Server{s.httpSrv, s.tlsSrv, s.acmeSrv} {
		if server == nil {
			continue
		}
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("close server after listener failure", "error", err)
		}
	}
}

func newHTTPServer(baseCtx context.Context, addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return baseCtx
		},
	}
}

func authorizedHTTPSRedirectHandler(hostPolicy autocert.HostPolicy, tlsListenAddr string) http.Handler {
	_, tlsPort, _ := net.SplitHostPort(strings.TrimSpace(tlsListenAddr))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, err := canonicalRequestHostname(r.Host)
		if err != nil || hostPolicy == nil || hostPolicy(r.Context(), host) != nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Use HTTPS", http.StatusBadRequest)
			return
		}

		if tlsPort != "" {
			host = net.JoinHostPort(host, tlsPort)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusFound)
	})
}

type tlsHostHandler struct {
	domain  string
	handler http.Handler
}

func (h *tlsHostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, hostErr := canonicalRequestHostname(r.Host)
	sni := ""
	var sniErr error
	if r.TLS == nil {
		sniErr = fmt.Errorf("TLS connection state is missing")
	} else {
		sni, sniErr = canonicalRequestHostname(r.TLS.ServerName)
	}
	if hostErr != nil || sniErr != nil || host != sni {
		http.Error(w, "TLS server name and HTTP Host must match", http.StatusMisdirectedRequest)
		return
	}
	if !strings.EqualFold(host, h.domain) {
		http.NotFound(w, r)
		return
	}
	h.handler.ServeHTTP(w, r)
}

func canonicalRequestHostname(authority string) (string, error) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", fmt.Errorf("host is empty")
	}
	host := authority
	if parsedHost, _, err := net.SplitHostPort(authority); err == nil {
		host = parsedHost
	} else if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]")
	} else if strings.Contains(authority, ":") {
		return "", fmt.Errorf("host %q has an invalid port", authority)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	for _, char := range host {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '.' {
			return "", fmt.Errorf("host %q is invalid", authority)
		}
	}
	return host, nil
}
