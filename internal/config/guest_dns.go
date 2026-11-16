package config

import (
	"bufio"
	"net"
	"os"
	"strings"
)

var defaultGuestDNSResolverFiles = []string{
	"/etc/resolv.conf",
	"/run/systemd/resolve/resolv.conf",
	"/run/NetworkManager/no-stub-resolv.conf",
	"/run/resolvconf/resolv.conf",
}

var fallbackGuestDNSServers = []string{"1.1.1.1", "8.8.8.8"}

type GuestDNSConfig struct {
	Servers        []string
	ProxyEnabled   bool
	ProxyUpstreams []string
}

func loadGuestDNSConfig() GuestDNSConfig {
	if strings.TrimSpace(os.Getenv("KILN_VM_GUEST_DNS_SERVERS")) != "" {
		return GuestDNSConfig{
			Servers:      envListOrDefault("KILN_VM_GUEST_DNS_SERVERS", fallbackGuestDNSServers),
			ProxyEnabled: false,
		}
	}
	upstreams := discoverGuestDNSProxyUpstreams(defaultGuestDNSResolverFiles)
	if len(upstreams) == 0 {
		upstreams = fallbackGuestDNSServers
	}
	return GuestDNSConfig{
		ProxyEnabled:   true,
		ProxyUpstreams: upstreams,
	}
}

func discoverGuestDNSProxyUpstreams(files []string) []string {
	for _, file := range files {
		servers := guestDNSProxyUpstreamsFromResolvConf(file)
		if len(servers) > 0 {
			return servers
		}
	}
	return nil
}

func guestDNSProxyUpstreamsFromResolvConf(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseGuestDNSProxyUpstreamsFromResolvConf(string(data))
}

func parseGuestDNSProxyUpstreamsFromResolvConf(content string) []string {
	return parseNameserversFromResolvConf(content, normalizeGuestDNSProxyUpstream)
}

func parseNameserversFromResolvConf(content string, normalize func(string) (string, bool)) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	seen := map[string]bool{}
	servers := []string{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		server, ok := normalize(fields[1])
		if !ok || seen[server] {
			continue
		}
		seen[server] = true
		servers = append(servers, server)
	}
	return servers
}

func normalizeGuestDNSProxyUpstream(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.Trim(raw, "[]"))
	if raw == "" {
		return "", false
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return "", false
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return "", false
	}
	return ip.String(), true
}
