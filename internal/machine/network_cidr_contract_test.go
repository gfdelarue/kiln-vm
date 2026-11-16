package machine

import (
	"strings"
	"testing"
)

func TestNetworkManagerAllocationHonorsConfiguredBaseCIDR(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("10.99.0.0/16", 1, nil, "")

	nm.mu.Lock()
	idx := nm.nextIdx
	nm.nextIdx++
	nm.mu.Unlock()

	alloc := mustNetworkAllocation(t, nm, idx)
	if !strings.HasPrefix(alloc.GuestIP, "10.99.") || !strings.HasPrefix(alloc.GatewayIP, "10.99.") {
		t.Fatalf("allocation ignored configured base CIDR: gateway=%s guest=%s", alloc.GatewayIP, alloc.GuestIP)
	}
}

func TestNetworkManagerAllocationRejectsInvalidBaseCIDR(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("not-a-cidr", 1, nil, "")

	if _, err := nm.allocationForIndex(1); err == nil || !strings.Contains(err.Error(), "parse base CIDR") {
		t.Fatalf("allocationForIndex invalid base error = %v, want parse base CIDR error", err)
	}
}

func TestNetworkManagerAllocationFailsWhenConfiguredBaseCIDRIsExhausted(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("10.99.0.0/29", 1, nil, "")

	first := mustNetworkAllocation(t, nm, 1)
	if first.GatewayIP != "10.99.0.5" || first.GuestIP != "10.99.0.6" {
		t.Fatalf("first allocation = gateway %s guest %s, want 10.99.0.5/10.99.0.6", first.GatewayIP, first.GuestIP)
	}

	if _, err := nm.allocationForIndex(2); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("allocationForIndex exhausted error = %v, want exhausted", err)
	}
}
