package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/limits"
)

const (
	DefaultKernelPath     = "/usr/lib/kiln-vm/vmlinux"
	DefaultFirecrackerBin = "/usr/bin/firecracker"
	DefaultAgentBinPath   = "/usr/lib/kiln-vm/kiln-vm-agent"
	DefaultLibvirtNetwork = "kiln-vm"
	DefaultLibvirtCIDR    = "10.232.0.0/24"
	DefaultMaxVMDiskMB    = limits.DefaultMaxVMRootDiskMB
	ExitCodeConfig        = 78
)

type Config struct {
	ListenAddr                    string
	AllowInsecureRemoteHTTP       bool
	DataDir                       string
	KernelPath                    string
	FirecrackerBin                string
	AgentBinPath                  string
	BridgeCIDR                    string
	LogLevel                      string
	TLSEnabled                    bool
	TLSDomain                     string
	TLSCertDir                    string
	TLSListenAddr                 string
	ACMEListenAddr                string
	ACMEEmail                     string
	MaxRunningVMs                 int
	MaxTotalVCPUs                 int
	PooledCPUs                    int
	MaxTotalMemoryMB              int
	MaxVMDiskMB                   int64
	ImageBuildTimeout             time.Duration
	ImageBuildDir                 string
	ImageMinFreeMB                int
	ProfileImageMode              string
	ProfileImageCatalog           string
	ProfileImageCacheMaxMB        int64
	ProfileImageToken             string
	ProfileImageTokenFile         string
	ProfileImagePullTimeout       time.Duration
	ProfileImagePullHeaderTimeout time.Duration
	GuestDNSServers               []string
	GuestDNSProxyEnabled          bool
	GuestDNSProxyUpstreams        []string
	GuestDNSProbeName             string
	BootstrapTokenFile            string
	LibvirtConnectURI             string
	LibvirtNetwork                string
	LibvirtCIDR                   string
	WindowsProfileDir             string
}

func Load() (*Config, error) {
	tlsEnabled, err := envStrictBoolOrDefault("KILN_VM_TLS_ENABLED", false)
	if err != nil {
		return nil, err
	}
	allowInsecureRemoteHTTP, err := envStrictBoolOrDefault("KILN_VM_ALLOW_INSECURE_REMOTE_HTTP", false)
	if err != nil {
		return nil, err
	}

	guestDNS := loadGuestDNSConfig()
	cfg := &Config{
		ListenAddr:                    envOrDefault("KILN_VM_LISTEN_ADDR", "127.0.0.1:5456"),
		AllowInsecureRemoteHTTP:       allowInsecureRemoteHTTP,
		DataDir:                       envOrDefault("KILN_VM_DATA_DIR", "/var/lib/kiln-vm"),
		KernelPath:                    envOrDefault("KILN_VM_KERNEL_PATH", DefaultKernelPath),
		FirecrackerBin:                envOrDefault("KILN_VM_FIRECRACKER_BIN", DefaultFirecrackerBin),
		AgentBinPath:                  envOrDefault("KILN_VM_AGENT_BIN", DefaultAgentBinPath),
		BridgeCIDR:                    envOrDefault("KILN_VM_BRIDGE_CIDR", "10.231.0.0/16"),
		LogLevel:                      envOrDefault("KILN_VM_LOG_LEVEL", "info"),
		TLSEnabled:                    tlsEnabled,
		TLSDomain:                     envOrDefault("KILN_VM_TLS_DOMAIN", ""),
		TLSCertDir:                    envOrDefault("KILN_VM_TLS_CERT_DIR", "/var/lib/kiln-vm/certs"),
		TLSListenAddr:                 envOrDefault("KILN_VM_TLS_LISTEN_ADDR", ":443"),
		ACMEListenAddr:                envOrDefault("KILN_VM_ACME_LISTEN_ADDR", ":80"),
		ACMEEmail:                     envOrDefault("KILN_VM_ACME_EMAIL", ""),
		MaxRunningVMs:                 envIntOrDefault("KILN_VM_MAX_RUNNING_VMS", 8),
		MaxTotalVCPUs:                 envIntOrDefault("KILN_VM_MAX_TOTAL_VCPUS", 8),
		PooledCPUs:                    envIntOrDefault("KILN_VM_POOLED_CPU_LIMIT", 0),
		MaxTotalMemoryMB:              envIntOrDefault("KILN_VM_MAX_TOTAL_MEMORY_MB", 8192),
		MaxVMDiskMB:                   envInt64OrDefault("KILN_VM_MAX_VM_DISK_MB", DefaultMaxVMDiskMB),
		ImageBuildTimeout:             envDurationOrDefault("KILN_VM_IMAGE_BUILD_TIMEOUT", 30*time.Minute),
		ImageBuildDir:                 envOrDefault("KILN_VM_IMAGE_BUILD_DIR", ""),
		ImageMinFreeMB:                envIntOrDefault("KILN_VM_IMAGE_MIN_FREE_MB", 2048),
		ProfileImageMode:              envOrDefault("KILN_VM_PROFILE_IMAGE_MODE", "auto"),
		ProfileImageCatalog:           envOrDefault("KILN_VM_PROFILE_IMAGE_CATALOG", "/usr/share/kiln-vm/profile-images.json"),
		ProfileImageCacheMaxMB:        envInt64OrDefault("KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB", 8192),
		ProfileImageToken:             envOrDefault("KILN_VM_PROFILE_IMAGE_TOKEN", ""),
		ProfileImageTokenFile:         envOrDefault("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", ""),
		ProfileImagePullTimeout:       envDurationOrDefault("KILN_VM_PROFILE_IMAGE_PULL_TIMEOUT", 30*time.Minute),
		ProfileImagePullHeaderTimeout: envDurationOrDefault("KILN_VM_PROFILE_IMAGE_PULL_HEADER_TIMEOUT", 30*time.Second),
		GuestDNSServers:               guestDNS.Servers,
		GuestDNSProxyEnabled:          guestDNS.ProxyEnabled,
		GuestDNSProxyUpstreams:        guestDNS.ProxyUpstreams,
		GuestDNSProbeName:             envOrDefault("KILN_VM_GUEST_DNS_PROBE_NAME", "example.com."),
		BootstrapTokenFile:            envOrDefault("KILN_VM_BOOTSTRAP_TOKEN_FILE", ""),
		LibvirtConnectURI:             envOrDefault("KILN_VM_LIBVIRT_CONNECT_URI", "qemu:///system"),
		LibvirtNetwork:                envOrDefault("KILN_VM_LIBVIRT_NETWORK", DefaultLibvirtNetwork),
		LibvirtCIDR:                   envOrDefault("KILN_VM_LIBVIRT_CIDR", DefaultLibvirtCIDR),
		WindowsProfileDir:             envOrDefault("KILN_VM_WINDOWS_PROFILE_DIR", ""),
	}
	if cfg.BootstrapTokenFile == "" {
		cfg.BootstrapTokenFile = filepath.Join(cfg.DataDir, "bootstrap-token")
	}
	if cfg.PooledCPUs < 0 || (cfg.MaxTotalVCPUs > 0 && cfg.PooledCPUs > cfg.MaxTotalVCPUs) {
		return nil, fmt.Errorf("KILN_VM_POOLED_CPU_LIMIT must be nonnegative and no greater than KILN_VM_MAX_TOTAL_VCPUS")
	}
	if err := validateManagementTransport(cfg); err != nil {
		return nil, err
	}
	if err := validateGuestDNSNetworkSeparation(cfg); err != nil {
		return nil, err
	}
	if err := validateLibvirtNetworkSeparation(cfg); err != nil {
		return nil, err
	}
	probeName, err := canonicalGuestDNSProbeName(cfg.GuestDNSProbeName)
	if err != nil {
		return nil, err
	}
	cfg.GuestDNSProbeName = probeName
	return cfg, nil
}

func validateGuestDNSNetworkSeparation(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("guest DNS network configuration is nil")
	}
	bridgeNetwork, err := validatedBridgeNetwork(cfg.BridgeCIDR)
	if err != nil {
		return err
	}

	resolverSets := []struct {
		field     string
		addresses []string
	}{
		{field: "KILN_VM_GUEST_DNS_SERVERS", addresses: cfg.GuestDNSServers},
		{field: "host DNS proxy upstream", addresses: cfg.GuestDNSProxyUpstreams},
	}
	for _, resolverSet := range resolverSets {
		for _, rawAddress := range resolverSet.addresses {
			address := guestDNSResolverAddress(rawAddress)
			if address == nil {
				continue
			}
			address = address.To4()
			if address == nil || !bridgeNetwork.Contains(address) {
				continue
			}
			return fmt.Errorf("%s resolver %s overlaps KILN_VM_BRIDGE_CIDR %s; choose a non-overlapping KILN_VM_BRIDGE_CIDR or resolver address", resolverSet.field, address.String(), bridgeNetwork.String())
		}
	}
	return nil
}

func validateLibvirtNetworkSeparation(cfg *Config) error {
	if cfg.LibvirtNetwork != DefaultLibvirtNetwork {
		return nil
	}
	_, libvirtNetwork, err := net.ParseCIDR(strings.TrimSpace(cfg.LibvirtCIDR))
	if err != nil || libvirtNetwork.IP.To4() == nil {
		return fmt.Errorf("KILN_VM_LIBVIRT_CIDR must be a valid IPv4 CIDR")
	}
	bridgeNetwork, err := validatedBridgeNetwork(cfg.BridgeCIDR)
	if err != nil {
		return err
	}
	if bridgeNetwork.Contains(libvirtNetwork.IP) || libvirtNetwork.Contains(bridgeNetwork.IP) {
		return fmt.Errorf("KILN_VM_LIBVIRT_CIDR %s overlaps KILN_VM_BRIDGE_CIDR %s; choose non-overlapping ranges", libvirtNetwork, bridgeNetwork)
	}
	return nil
}

func validatedBridgeNetwork(value string) (*net.IPNet, error) {
	trimmed := strings.TrimSpace(value)
	bridgeAddress, bridgeNetwork, err := net.ParseCIDR(trimmed)
	if err != nil {
		return nil, fmt.Errorf("KILN_VM_BRIDGE_CIDR must be a valid IPv4 CIDR: %w", err)
	}
	bridgeAddress = bridgeAddress.To4()
	if bridgeAddress == nil {
		return nil, fmt.Errorf("KILN_VM_BRIDGE_CIDR must be an IPv4 CIDR")
	}
	prefixBits, addressBits := bridgeNetwork.Mask.Size()
	if addressBits != net.IPv4len*8 {
		return nil, fmt.Errorf("KILN_VM_BRIDGE_CIDR must be an IPv4 CIDR")
	}
	networkAddress := bridgeAddress.Mask(bridgeNetwork.Mask)
	if !bridgeAddress.Equal(networkAddress) {
		return nil, fmt.Errorf("KILN_VM_BRIDGE_CIDR %s is not network-aligned; use %s/%d", trimmed, networkAddress.String(), prefixBits)
	}
	if prefixBits > 29 {
		return nil, fmt.Errorf("KILN_VM_BRIDGE_CIDR %s is too small for the first kiln-vm VM /30 allocation; use an IPv4 /29 or larger range", trimmed)
	}
	bridgeNetwork.IP = networkAddress
	return bridgeNetwork, nil
}

func guestDNSResolverAddress(rawAddress string) net.IP {
	address := strings.TrimSpace(rawAddress)
	if parsed := net.ParseIP(address); parsed != nil {
		return parsed
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		if parsed := net.ParseIP(address[1 : len(address)-1]); parsed != nil {
			return parsed
		}
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	return net.ParseIP(strings.TrimSpace(strings.Trim(host, "[]")))
}

func canonicalGuestDNSProbeName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME is required")
	}
	if value != trimmed {
		return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME must not contain surrounding whitespace")
	}
	name := strings.TrimSuffix(trimmed, ".")
	if name == "" {
		return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME must not be the DNS root")
	}
	if len(name) > 253 {
		return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME must not exceed 253 bytes")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME must not contain an empty label")
		}
		if len(label) > 63 {
			return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME labels must not exceed 63 bytes")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME labels must not start or end with a hyphen")
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return "", fmt.Errorf("KILN_VM_GUEST_DNS_PROBE_NAME must contain only ASCII letters, digits, hyphens, and dots")
		}
	}
	return strings.ToLower(name) + ".", nil
}

func validateManagementTransport(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("management transport configuration is nil")
	}
	if cfg.TLSEnabled {
		if cfg.AllowInsecureRemoteHTTP {
			return fmt.Errorf("KILN_VM_ALLOW_INSECURE_REMOTE_HTTP must be false when KILN_VM_TLS_ENABLED=true; TLS replaces plaintext management HTTP")
		}
		canonicalDomain, err := canonicalTLSDomain(cfg.TLSDomain)
		if err != nil {
			return err
		}
		cfg.TLSDomain = canonicalDomain
		if strings.TrimSpace(cfg.TLSCertDir) == "" {
			return fmt.Errorf("KILN_VM_TLS_CERT_DIR is required when KILN_VM_TLS_ENABLED=true")
		}
		if cfg.TLSCertDir != strings.TrimSpace(cfg.TLSCertDir) {
			return fmt.Errorf("KILN_VM_TLS_CERT_DIR must not contain surrounding whitespace")
		}
		if err := validateTCPListenAddr("KILN_VM_TLS_LISTEN_ADDR", cfg.TLSListenAddr); err != nil {
			return err
		}
		if err := validateTCPListenAddr("KILN_VM_ACME_LISTEN_ADDR", cfg.ACMEListenAddr); err != nil {
			return err
		}
		if strings.TrimSpace(cfg.TLSListenAddr) == strings.TrimSpace(cfg.ACMEListenAddr) {
			return fmt.Errorf("KILN_VM_TLS_LISTEN_ADDR and KILN_VM_ACME_LISTEN_ADDR must be different")
		}
		return nil
	}

	if err := validateTCPListenAddr("KILN_VM_LISTEN_ADDR", cfg.ListenAddr); err != nil {
		return err
	}
	if !cfg.AllowInsecureRemoteHTTP && !isLoopbackListenHost(cfg.ListenAddr) {
		return fmt.Errorf("KILN_VM_LISTEN_ADDR %q is not loopback; use HTTPS or set KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true to explicitly allow remote plaintext management HTTP", cfg.ListenAddr)
	}
	return nil
}

func validateTCPListenAddr(key, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is required", key)
	}
	if value != trimmed {
		return fmt.Errorf("%s must not contain surrounding whitespace", key)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must be a valid host:port address: %w", key, err)
	}
	if err := validateTCPListenHost(key, host); err != nil {
		return err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("%s must use a numeric TCP port between 1 and 65535", key)
	}
	return nil
}

func validateTCPListenHost(key, host string) error {
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if strings.ContainsAny(host, ":%") || looksLikeInvalidIPv4(host) {
		return fmt.Errorf("%s host %q must be an IP literal or valid DNS hostname", key, host)
	}
	hostname := strings.TrimSuffix(strings.ToLower(host), ".")
	if hostname == "" || len(hostname) > 253 {
		return fmt.Errorf("%s host %q must be an IP literal or valid DNS hostname", key, host)
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%s host %q must be an IP literal or valid DNS hostname", key, host)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return fmt.Errorf("%s host %q must be an IP literal or valid DNS hostname", key, host)
			}
		}
	}
	return nil
}

func looksLikeInvalidIPv4(host string) bool {
	if strings.Count(host, ".") != 3 {
		return false
	}
	for _, char := range host {
		if (char < '0' || char > '9') && char != '.' {
			return false
		}
	}
	return true
}

func canonicalTLSDomain(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("KILN_VM_TLS_DOMAIN is required when KILN_VM_TLS_ENABLED=true")
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("KILN_VM_TLS_DOMAIN must not contain surrounding whitespace")
	}
	domain := strings.ToLower(strings.TrimSuffix(value, "."))
	if len(domain) > 253 || net.ParseIP(strings.Trim(domain, "[]")) != nil || looksLikeInvalidIPv4(domain) {
		return "", fmt.Errorf("KILN_VM_TLS_DOMAIN %q must be a valid DNS hostname", value)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("KILN_VM_TLS_DOMAIN %q must be a fully qualified DNS hostname", value)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("KILN_VM_TLS_DOMAIN %q must be a valid DNS hostname", value)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("KILN_VM_TLS_DOMAIN %q must be a valid ASCII DNS hostname", value)
			}
		}
	}
	return domain, nil
}

func isLoopbackListenHost(value string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ResolveProfileImageToken(token, tokenFile string) (string, string, error) {
	if value := strings.TrimSpace(token); value != "" {
		return value, "KILN_VM_PROFILE_IMAGE_TOKEN", nil
	}
	tokenFile = strings.TrimSpace(tokenFile)
	if tokenFile == "" {
		return "", "", nil
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", "KILN_VM_PROFILE_IMAGE_TOKEN_FILE", fmt.Errorf("read profile image token file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", "KILN_VM_PROFILE_IMAGE_TOKEN_FILE", nil
	}
	return value, "KILN_VM_PROFILE_IMAGE_TOKEN_FILE", nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envStrictBoolOrDefault(key string, def bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def, nil
	}
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be exactly true or false, got %q", key, raw)
	}
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.Atoi(v)
		if err == nil {
			return parsed
		}
	}
	return def
}

func envInt64OrDefault(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return parsed
		}
	}
	return def
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		parsed, err := time.ParseDuration(v)
		if err == nil {
			return parsed
		}
	}
	return def
}

func envListOrDefault(key string, def []string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return append([]string{}, def...)
	}
	var values []string
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return append([]string{}, def...)
	}
	return values
}
