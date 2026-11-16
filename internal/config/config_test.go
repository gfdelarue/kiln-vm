package config

import (
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func useGuestDNSResolverFile(t *testing.T, content string) {
	t.Helper()
	resolverFile := t.TempDir() + "/resolv.conf"
	if err := os.WriteFile(resolverFile, []byte(content), 0644); err != nil {
		t.Fatalf("write resolver file: %v", err)
	}
	originalFiles := append([]string(nil), defaultGuestDNSResolverFiles...)
	defaultGuestDNSResolverFiles = []string{resolverFile}
	t.Cleanup(func() {
		defaultGuestDNSResolverFiles = originalFiles
	})
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "")
}

func TestDefaultManagementListenerBindsLoopback(t *testing.T) {
	t.Setenv("KILN_VM_LISTEN_ADDR", "")

	cfg := mustLoad(t)
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		t.Fatalf("parse default management listener %q: %v", cfg.ListenAddr, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("bind default management listener host %q: %v", host, err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("default management listener address = %T, want TCP", listener.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("default management listener bound %s, want loopback-only", addr)
	}
}

func TestLoadValidatesManagementTransportPolicy(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantErr    string
		wantListen string
		wantDomain string
		wantTLS    bool
		wantAllow  bool
	}{
		{
			name:       "default loopback HTTP",
			wantListen: "127.0.0.1:5456",
		},
		{
			name:    "wildcard HTTP requires explicit consent",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": ":8080"},
			wantErr: "KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true",
		},
		{
			name:    "LAN HTTP requires explicit consent",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "192.168.1.25:8080"},
			wantErr: "not loopback",
		},
		{
			name:    "hostname HTTP requires explicit consent",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "kiln-vm.internal:8080"},
			wantErr: "not loopback",
		},
		{
			name:       "localhost HTTP is local",
			env:        map[string]string{"KILN_VM_LISTEN_ADDR": "localhost:8080"},
			wantListen: "localhost:8080",
		},
		{
			name:       "IPv6 loopback HTTP is local",
			env:        map[string]string{"KILN_VM_LISTEN_ADDR": "[::1]:8080"},
			wantListen: "[::1]:8080",
		},
		{
			name: "remote HTTP with explicit consent",
			env: map[string]string{
				"KILN_VM_LISTEN_ADDR":                "0.0.0.0:8080",
				"KILN_VM_ALLOW_INSECURE_REMOTE_HTTP": "true",
			},
			wantListen: "0.0.0.0:8080",
			wantAllow:  true,
		},
		{
			name:    "malformed insecure consent",
			env:     map[string]string{"KILN_VM_ALLOW_INSECURE_REMOTE_HTTP": "TRUE"},
			wantErr: "must be exactly true or false",
		},
		{
			name:    "empty insecure consent is malformed",
			env:     map[string]string{"KILN_VM_ALLOW_INSECURE_REMOTE_HTTP": ""},
			wantErr: "must be exactly true or false",
		},
		{
			name:    "malformed TLS boolean",
			env:     map[string]string{"KILN_VM_TLS_ENABLED": "1"},
			wantErr: "must be exactly true or false",
		},
		{
			name:    "empty TLS boolean is malformed",
			env:     map[string]string{"KILN_VM_TLS_ENABLED": ""},
			wantErr: "must be exactly true or false",
		},
		{
			name:    "TLS requires domain",
			env:     map[string]string{"KILN_VM_TLS_ENABLED": "true"},
			wantErr: "KILN_VM_TLS_DOMAIN is required",
		},
		{
			name: "TLS ignores legacy plaintext wildcard",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED": "true",
				"KILN_VM_TLS_DOMAIN":  "api.example.test",
				"KILN_VM_LISTEN_ADDR": ":8080",
			},
			wantListen: ":8080",
			wantDomain: "api.example.test",
			wantTLS:    true,
		},
		{
			name: "TLS domain is canonicalized",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED": "true",
				"KILN_VM_TLS_DOMAIN":  "API.Example.TEST.",
			},
			wantListen: "127.0.0.1:5456",
			wantDomain: "api.example.test",
			wantTLS:    true,
		},
		{
			name: "TLS rejects a single-label domain",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED": "true",
				"KILN_VM_TLS_DOMAIN":  "kiln-vm",
			},
			wantErr: "fully qualified DNS hostname",
		},
		{
			name: "TLS rejects a URL as domain",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED": "true",
				"KILN_VM_TLS_DOMAIN":  "https://api.example.test",
			},
			wantErr: "valid ASCII DNS hostname",
		},
		{
			name: "TLS rejects an invalid IPv4-looking domain",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED": "true",
				"KILN_VM_TLS_DOMAIN":  "999.999.999.999",
			},
			wantErr: "valid DNS hostname",
		},
		{
			name: "TLS rejects whitespace certificate directory",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED":  "true",
				"KILN_VM_TLS_DOMAIN":   "api.example.test",
				"KILN_VM_TLS_CERT_DIR": " /var/lib/kiln-vm/certs ",
			},
			wantErr: "KILN_VM_TLS_CERT_DIR must not contain surrounding whitespace",
		},
		{
			name: "TLS rejects insecure plaintext consent",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED":                "true",
				"KILN_VM_TLS_DOMAIN":                 "api.example.test",
				"KILN_VM_ALLOW_INSECURE_REMOTE_HTTP": "true",
			},
			wantErr: "must be false when KILN_VM_TLS_ENABLED=true",
		},
		{
			name: "TLS and ACME listeners must differ",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED":      "true",
				"KILN_VM_TLS_DOMAIN":       "api.example.test",
				"KILN_VM_TLS_LISTEN_ADDR":  "127.0.0.1:8443",
				"KILN_VM_ACME_LISTEN_ADDR": "127.0.0.1:8443",
			},
			wantErr: "must be different",
		},
		{
			name:    "invalid plaintext address",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "localhost"},
			wantErr: "valid host:port",
		},
		{
			name:    "plaintext address rejects surrounding whitespace",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": " 127.0.0.1:8080 "},
			wantErr: "must not contain surrounding whitespace",
		},
		{
			name:    "plaintext address rejects port zero",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "127.0.0.1:0"},
			wantErr: "between 1 and 65535",
		},
		{
			name:    "plaintext address rejects invalid DNS host",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "bad host:8080"},
			wantErr: "valid DNS hostname",
		},
		{
			name:    "plaintext address rejects invalid IPv4 literal",
			env:     map[string]string{"KILN_VM_LISTEN_ADDR": "999.999.999.999:8080"},
			wantErr: "IP literal or valid DNS hostname",
		},
		{
			name: "TLS rejects invalid listener host before binding",
			env: map[string]string{
				"KILN_VM_TLS_ENABLED":     "true",
				"KILN_VM_TLS_DOMAIN":      "api.example.test",
				"KILN_VM_TLS_LISTEN_ADDR": "bad host:443",
			},
			wantErr: "KILN_VM_TLS_LISTEN_ADDR host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{
				"KILN_VM_LISTEN_ADDR",
				"KILN_VM_TLS_DOMAIN",
				"KILN_VM_TLS_CERT_DIR",
				"KILN_VM_TLS_LISTEN_ADDR",
				"KILN_VM_ACME_LISTEN_ADDR",
			} {
				t.Setenv(key, "")
			}
			t.Setenv("KILN_VM_ALLOW_INSECURE_REMOTE_HTTP", "false")
			t.Setenv("KILN_VM_TLS_ENABLED", "false")
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ListenAddr != tt.wantListen {
				t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, tt.wantListen)
			}
			if cfg.TLSDomain != tt.wantDomain {
				t.Fatalf("TLSDomain = %q, want %q", cfg.TLSDomain, tt.wantDomain)
			}
			if cfg.TLSEnabled != tt.wantTLS {
				t.Fatalf("TLSEnabled = %t, want %t", cfg.TLSEnabled, tt.wantTLS)
			}
			if cfg.AllowInsecureRemoteHTTP != tt.wantAllow {
				t.Fatalf("AllowInsecureRemoteHTTP = %t, want %t", cfg.AllowInsecureRemoteHTTP, tt.wantAllow)
			}
		})
	}
}

func TestEnvIntOrDefault(t *testing.T) {
	t.Setenv("KILN_VM_TEST_INT", "42")
	if got := envIntOrDefault("KILN_VM_TEST_INT", 7); got != 42 {
		t.Fatalf("envIntOrDefault = %d, want 42", got)
	}

	t.Setenv("KILN_VM_TEST_BAD_INT", "nope")
	if got := envIntOrDefault("KILN_VM_TEST_BAD_INT", 7); got != 7 {
		t.Fatalf("bad envIntOrDefault = %d, want default 7", got)
	}
}

func TestEnvInt64OrDefault(t *testing.T) {
	t.Setenv("KILN_VM_TEST_INT64", "524288")
	if got := envInt64OrDefault("KILN_VM_TEST_INT64", 7); got != 524288 {
		t.Fatalf("envInt64OrDefault = %d, want 524288", got)
	}

	t.Setenv("KILN_VM_TEST_BAD_INT64", "huge")
	if got := envInt64OrDefault("KILN_VM_TEST_BAD_INT64", 7); got != 7 {
		t.Fatalf("bad envInt64OrDefault = %d, want default 7", got)
	}
}

func TestEnvDurationOrDefault(t *testing.T) {
	t.Setenv("KILN_VM_TEST_DURATION", "15m")
	if got := envDurationOrDefault("KILN_VM_TEST_DURATION", time.Minute); got != 15*time.Minute {
		t.Fatalf("envDurationOrDefault = %s, want 15m", got)
	}

	t.Setenv("KILN_VM_TEST_BAD_DURATION", "soon")
	if got := envDurationOrDefault("KILN_VM_TEST_BAD_DURATION", time.Minute); got != time.Minute {
		t.Fatalf("bad envDurationOrDefault = %s, want default 1m", got)
	}
}

func TestEnvListOrDefault(t *testing.T) {
	fallback := []string{"1.1.1.1", "8.8.8.8"}

	if got := envListOrDefault("KILN_VM_TEST_LIST_UNSET", fallback); !reflect.DeepEqual(got, fallback) {
		t.Fatalf("unset envListOrDefault = %#v, want %#v", got, fallback)
	}

	t.Setenv("KILN_VM_TEST_LIST", " 9.9.9.9, ,1.0.0.1 ")
	if got := envListOrDefault("KILN_VM_TEST_LIST", fallback); !reflect.DeepEqual(got, []string{"9.9.9.9", "1.0.0.1"}) {
		t.Fatalf("envListOrDefault = %#v", got)
	}
}

func TestLoadGuestDNSConfigExplicitOverrideDisablesProxy(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", " 9.9.9.9, ,10.0.0.53 ")

	got := loadGuestDNSConfig()
	want := []string{"9.9.9.9", "10.0.0.53"}
	if got.ProxyEnabled {
		t.Fatal("loadGuestDNSConfig enabled proxy for explicit guest DNS override")
	}
	if !reflect.DeepEqual(got.Servers, want) {
		t.Fatalf("loadGuestDNSConfig servers = %#v, want %#v", got.Servers, want)
	}
	if len(got.ProxyUpstreams) != 0 {
		t.Fatalf("loadGuestDNSConfig proxy upstreams = %#v, want none for explicit override", got.ProxyUpstreams)
	}
}

func TestLoadRejectsExplicitGuestDNSResolverInsideBridgeCIDR(t *testing.T) {
	tests := []struct {
		name      string
		resolvers string
	}{
		{name: "allocated gateway self recursion", resolvers: "172.16.0.1"},
		{name: "another address in allocation domain", resolvers: "172.16.99.53"},
		{name: "mixed safe and overlapping still rejects", resolvers: "1.1.1.1,172.16.99.53"},
		{name: "IPv4 mapped address canonicalizes before comparison", resolvers: "::ffff:172.16.0.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KILN_VM_BRIDGE_CIDR", "172.16.0.0/16")
			t.Setenv("KILN_VM_GUEST_DNS_SERVERS", test.resolvers)
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted a guest DNS resolver inside KILN_VM_BRIDGE_CIDR")
			}
			for _, want := range []string{"KILN_VM_GUEST_DNS_SERVERS", "overlaps KILN_VM_BRIDGE_CIDR", "choose a non-overlapping"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want containing %q", err, want)
				}
			}
		})
	}
}

func TestLoadRejectsDiscoveredDNSProxyResolverInsideBridgeCIDR(t *testing.T) {
	tests := []struct {
		name       string
		resolvConf string
	}{
		{
			name:       "discovered allocation address",
			resolvConf: "nameserver 172.16.0.53\n",
		},
		{
			name:       "mixed discovered resolvers still reject",
			resolvConf: "nameserver 127.0.0.53\nnameserver 172.16.4.53\nnameserver 2001:db8::53\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useGuestDNSResolverFile(t, test.resolvConf)
			t.Setenv("KILN_VM_BRIDGE_CIDR", "172.16.0.0/16")
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted a discovered DNS proxy resolver inside KILN_VM_BRIDGE_CIDR")
			}
			for _, want := range []string{"host DNS proxy upstream", "overlaps KILN_VM_BRIDGE_CIDR", "choose a non-overlapping"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want containing %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "resolv.conf") || strings.Contains(err.Error(), "nameserver") {
				t.Fatalf("Load error exposed unrelated resolver-file details: %q", err)
			}
		})
	}
}

func TestLoadAllowsDNSProxyResolversOutsideBridgeCIDR(t *testing.T) {
	useGuestDNSResolverFile(t, "nameserver 127.0.0.53\nnameserver ::1\nnameserver 2001:db8::53\nnameserver 192.0.2.53\n")
	t.Setenv("KILN_VM_BRIDGE_CIDR", "172.16.0.0/16")

	cfg := mustLoad(t)
	want := []string{"127.0.0.53", "::1", "2001:db8::53", "192.0.2.53"}
	if !cfg.GuestDNSProxyEnabled || !reflect.DeepEqual(cfg.GuestDNSProxyUpstreams, want) {
		t.Fatalf("safe DNS proxy config = enabled:%t upstreams:%#v, want enabled with %#v", cfg.GuestDNSProxyEnabled, cfg.GuestDNSProxyUpstreams, want)
	}
}

func TestLoadRejectsInvalidBridgeCIDRBeforeDNSProxyStartup(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "1.1.1.1")
	t.Setenv("KILN_VM_BRIDGE_CIDR", "not-a-cidr")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "KILN_VM_BRIDGE_CIDR must be a valid IPv4 CIDR") {
		t.Fatalf("Load error = %v, want invalid KILN_VM_BRIDGE_CIDR", err)
	}
}

func TestLoadRejectsNonNetworkAlignedBridgeCIDR(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "192.0.2.53")
	t.Setenv("KILN_VM_BRIDGE_CIDR", "172.16.0.1/16")
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted KILN_VM_BRIDGE_CIDR with host bits")
	}
	for _, want := range []string{"KILN_VM_BRIDGE_CIDR 172.16.0.1/16", "not network-aligned", "use 172.16.0.0/16"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want containing %q", err, want)
		}
	}
}

func TestLoadRejectsBridgeCIDRTooSmallForFirstAllocation(t *testing.T) {
	for _, bridgeCIDR := range []string{"172.16.0.0/30", "172.16.0.0/31", "172.16.0.0/32"} {
		t.Run(bridgeCIDR, func(t *testing.T) {
			t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "192.0.2.53")
			t.Setenv("KILN_VM_BRIDGE_CIDR", bridgeCIDR)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted undersized KILN_VM_BRIDGE_CIDR %s", bridgeCIDR)
			}
			for _, want := range []string{bridgeCIDR, "first kiln-vm VM /30 allocation", "IPv4 /29 or larger range"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want containing %q", err, want)
				}
			}
		})
	}
}

func TestLoadAcceptsNetworkAlignedBridgeCIDRsWithAllocationCapacity(t *testing.T) {
	for _, bridgeCIDR := range []string{"172.16.0.0/29", "172.16.0.0/16"} {
		t.Run(bridgeCIDR, func(t *testing.T) {
			t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "192.0.2.53")
			t.Setenv("KILN_VM_BRIDGE_CIDR", bridgeCIDR)
			cfg := mustLoad(t)
			if cfg.BridgeCIDR != bridgeCIDR {
				t.Fatalf("BridgeCIDR = %q, want validated value preserved as %q", cfg.BridgeCIDR, bridgeCIDR)
			}
		})
	}
}

func TestLoadUsesValidatedBridgeNetworkForResolverOverlap(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "172.16.0.7")
	t.Setenv("KILN_VM_BRIDGE_CIDR", "172.16.0.0/29")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "resolver 172.16.0.7 overlaps KILN_VM_BRIDGE_CIDR 172.16.0.0/29") {
		t.Fatalf("Load error = %v, want overlap against canonical validated /29", err)
	}
}

func TestLoadCanonicalizesGuestDNSProbeName(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_PROBE_NAME", "Probe.Example.COM")

	cfg := mustLoad(t)
	if cfg.GuestDNSProbeName != "probe.example.com." {
		t.Fatalf("GuestDNSProbeName = %q, want canonical FQDN", cfg.GuestDNSProbeName)
	}
}

func TestLoadRejectsInvalidGuestDNSProbeNames(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "surrounding whitespace", value: " example.com. ", wantErr: "surrounding whitespace"},
		{name: "root", value: ".", wantErr: "DNS root"},
		{name: "empty label", value: "bad..example", wantErr: "empty label"},
		{name: "leading hyphen", value: "-bad.example", wantErr: "start or end"},
		{name: "unsupported character", value: "bad_name.example", wantErr: "ASCII letters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KILN_VM_GUEST_DNS_PROBE_NAME", test.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDiscoverGuestDNSProxyUpstreamsKeepsHostStubResolver(t *testing.T) {
	dir := t.TempDir()
	resolvConf := dir + "/resolv.conf"
	if err := os.WriteFile(resolvConf, []byte(`
nameserver 127.0.0.53
nameserver 127.0.0.54
nameserver 192.168.1.254
nameserver 127.0.0.53
nameserver ::1
nameserver 2001:db8::53
nameserver fe80::1
`), 0644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}

	got := discoverGuestDNSProxyUpstreams([]string{resolvConf})
	want := []string{"127.0.0.53", "127.0.0.54", "192.168.1.254", "::1", "2001:db8::53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverGuestDNSProxyUpstreams = %#v, want %#v", got, want)
	}
}

func TestLoadUsesPackagedRuntimeDefaults(t *testing.T) {
	cfg := mustLoad(t)
	if cfg.KernelPath != DefaultKernelPath {
		t.Fatalf("KernelPath = %q, want %q", cfg.KernelPath, DefaultKernelPath)
	}
	if cfg.FirecrackerBin != DefaultFirecrackerBin {
		t.Fatalf("FirecrackerBin = %q, want %q", cfg.FirecrackerBin, DefaultFirecrackerBin)
	}
	if cfg.AgentBinPath != DefaultAgentBinPath {
		t.Fatalf("AgentBinPath = %q, want %q", cfg.AgentBinPath, DefaultAgentBinPath)
	}
}

func TestLoadImageBuildConfig(t *testing.T) {
	t.Setenv("KILN_VM_IMAGE_BUILD_DIR", "/mnt/large/kiln-vm-build")
	t.Setenv("KILN_VM_IMAGE_MIN_FREE_MB", "4096")
	t.Setenv("KILN_VM_PROFILE_IMAGE_MODE", "pull")
	t.Setenv("KILN_VM_PROFILE_IMAGE_CATALOG", "/etc/kiln-vm/profile-images.json")
	t.Setenv("KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB", "6144")
	t.Setenv("KILN_VM_PROFILE_IMAGE_TOKEN", "secret")
	t.Setenv("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", "/etc/kiln-vm/profile-image-token")
	t.Setenv("KILN_VM_PROFILE_IMAGE_PULL_TIMEOUT", "12m")
	t.Setenv("KILN_VM_PROFILE_IMAGE_PULL_HEADER_TIMEOUT", "7s")

	cfg := mustLoad(t)
	if cfg.ImageBuildDir != "/mnt/large/kiln-vm-build" {
		t.Fatalf("ImageBuildDir = %q", cfg.ImageBuildDir)
	}
	if cfg.ImageMinFreeMB != 4096 {
		t.Fatalf("ImageMinFreeMB = %d, want 4096", cfg.ImageMinFreeMB)
	}
	if cfg.ProfileImageMode != "pull" {
		t.Fatalf("ProfileImageMode = %q", cfg.ProfileImageMode)
	}
	if cfg.ProfileImageCatalog != "/etc/kiln-vm/profile-images.json" {
		t.Fatalf("ProfileImageCatalog = %q", cfg.ProfileImageCatalog)
	}
	if cfg.ProfileImageCacheMaxMB != 6144 {
		t.Fatalf("ProfileImageCacheMaxMB = %d, want 6144", cfg.ProfileImageCacheMaxMB)
	}
	if cfg.ProfileImageToken != "secret" {
		t.Fatalf("ProfileImageToken = %q", cfg.ProfileImageToken)
	}
	if cfg.ProfileImageTokenFile != "/etc/kiln-vm/profile-image-token" {
		t.Fatalf("ProfileImageTokenFile = %q", cfg.ProfileImageTokenFile)
	}
	if cfg.ProfileImagePullTimeout != 12*time.Minute {
		t.Fatalf("ProfileImagePullTimeout = %s", cfg.ProfileImagePullTimeout)
	}
	if cfg.ProfileImagePullHeaderTimeout != 7*time.Second {
		t.Fatalf("ProfileImagePullHeaderTimeout = %s", cfg.ProfileImagePullHeaderTimeout)
	}
}

func TestResolveProfileImageTokenPrefersDirectToken(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("file-secret\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	token, source, err := ResolveProfileImageToken(" direct-secret ", tokenPath)
	if err != nil {
		t.Fatalf("ResolveProfileImageToken: %v", err)
	}
	if token != "direct-secret" || source != "KILN_VM_PROFILE_IMAGE_TOKEN" {
		t.Fatalf("token/source = %q/%q, want direct token", token, source)
	}
}

func TestResolveProfileImageTokenReadsFile(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("file-secret\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	token, source, err := ResolveProfileImageToken("", tokenPath)
	if err != nil {
		t.Fatalf("ResolveProfileImageToken: %v", err)
	}
	if token != "file-secret" || source != "KILN_VM_PROFILE_IMAGE_TOKEN_FILE" {
		t.Fatalf("token/source = %q/%q, want file token", token, source)
	}
}

func TestResolveProfileImageTokenReportsUnreadableFile(t *testing.T) {
	_, source, err := ResolveProfileImageToken("", t.TempDir()+"/missing-token")
	if err == nil {
		t.Fatal("ResolveProfileImageToken succeeded, want error")
	}
	if source != "KILN_VM_PROFILE_IMAGE_TOKEN_FILE" {
		t.Fatalf("source = %q, want token file", source)
	}
}

func TestLoadVMResourceCaps(t *testing.T) {
	t.Setenv("KILN_VM_MAX_VM_DISK_MB", "65536")

	cfg := mustLoad(t)
	if cfg.MaxVMDiskMB != 65536 {
		t.Fatalf("MaxVMDiskMB = %d, want 65536", cfg.MaxVMDiskMB)
	}
}

func TestLoadBootstrapTokenFile(t *testing.T) {
	t.Setenv("KILN_VM_BOOTSTRAP_TOKEN_FILE", "/run/kiln-vm/bootstrap-token")

	cfg := mustLoad(t)
	if cfg.BootstrapTokenFile != "/run/kiln-vm/bootstrap-token" {
		t.Fatalf("BootstrapTokenFile = %q", cfg.BootstrapTokenFile)
	}
}

func TestLoadBootstrapTokenFileDefaultsToDataDir(t *testing.T) {
	t.Setenv("KILN_VM_DATA_DIR", "/srv/kiln-vm")
	t.Setenv("KILN_VM_BOOTSTRAP_TOKEN_FILE", "")

	cfg := mustLoad(t)
	if cfg.BootstrapTokenFile != "/srv/kiln-vm/bootstrap-token" {
		t.Fatalf("BootstrapTokenFile = %q, want /srv/kiln-vm/bootstrap-token", cfg.BootstrapTokenFile)
	}
}

func TestLoadLibvirtWindowsProfileConfig(t *testing.T) {
	t.Setenv("KILN_VM_LIBVIRT_CONNECT_URI", "qemu:///session")
	t.Setenv("KILN_VM_LIBVIRT_NETWORK", "kiln-vm-test")
	t.Setenv("KILN_VM_WINDOWS_PROFILE_DIR", "/var/tmp/kiln-vm-windows")

	cfg := mustLoad(t)
	if cfg.LibvirtConnectURI != "qemu:///session" {
		t.Fatalf("LibvirtConnectURI = %q", cfg.LibvirtConnectURI)
	}
	if cfg.LibvirtNetwork != "kiln-vm-test" {
		t.Fatalf("LibvirtNetwork = %q", cfg.LibvirtNetwork)
	}
	if cfg.WindowsProfileDir != "/var/tmp/kiln-vm-windows" {
		t.Fatalf("WindowsProfileDir = %q", cfg.WindowsProfileDir)
	}
}

func TestLoadRejectsAManagedLibvirtNetworkInsideTheBridgeCIDR(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "192.0.2.53")
	t.Setenv("KILN_VM_LIBVIRT_NETWORK", "")
	t.Setenv("KILN_VM_LIBVIRT_CIDR", "10.231.4.0/24")

	_, err := Load()

	if err == nil || !strings.Contains(err.Error(), "KILN_VM_LIBVIRT_CIDR 10.231.4.0/24 overlaps KILN_VM_BRIDGE_CIDR 10.231.0.0/16") {
		t.Fatalf("Load error = %v, want libvirt CIDR overlap", err)
	}
}

func TestLoadIgnoresTheLibvirtCIDRForAnOperatorNetwork(t *testing.T) {
	t.Setenv("KILN_VM_GUEST_DNS_SERVERS", "192.0.2.53")
	t.Setenv("KILN_VM_LIBVIRT_NETWORK", "lab")
	t.Setenv("KILN_VM_LIBVIRT_CIDR", "10.231.4.0/24")

	cfg := mustLoad(t)

	if cfg.LibvirtNetwork != "lab" {
		t.Fatalf("LibvirtNetwork = %q, want lab", cfg.LibvirtNetwork)
	}
}
