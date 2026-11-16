package models

import "fmt"

const NetworkPolicyIsolatedWeb = "isolated-web"

// IsolatedWeb permits public IPv4 HTTP/HTTPS and the gateway DNS. It denies
// host services, private/reserved addresses, unsolicited ingress, spoofed
// guest addresses, and all IPv6.
func ValidateNetworkPolicy(policy, backend string) error {
	if policy == "" {
		return nil
	}
	if policy != NetworkPolicyIsolatedWeb {
		return fmt.Errorf("unknown network_policy %q", policy)
	}
	if backend != "firecracker" {
		return fmt.Errorf("isolated-web requires the Firecracker backend")
	}
	return nil
}
