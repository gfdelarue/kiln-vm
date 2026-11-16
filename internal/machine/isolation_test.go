package machine

import (
	"errors"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestIsolationRefusesForeignChainsAndReferences(t *testing.T) {
	owner := "00000000-0000-0000-0000-000000000001"
	a := Allocation{TAPDevice: "kv1", GuestIP: "172.16.0.6", GatewayIP: "172.16.0.5"}
	script, err := isolationTableTransaction(owner, a, false, "*filter\nCOMMIT\n", false, "filter")
	if err != nil {
		t.Fatal(err)
	}
	chain := strings.Fields(strings.Split(script, "\n")[1])[0][1:]
	for _, inventory := range []string{
		":" + chain + " - [0:0]\n",
		":" + chain + " - [0:0]\n-A " + chain + " -j ACCEPT\n",
		":" + chain + " - [0:0]\n-A " + chain + " -m comment --comment kiln-vm-isolation-v1:" + owner + " -j DROP\n-A OUTPUT -j " + chain + "\n",
	} {
		for _, remove := range []bool{false, true} {
			if _, err := isolationTableTransaction(owner, a, false, inventory, remove, "filter"); err == nil {
				t.Fatal("accepted foreign firewall state")
			}
		}
	}
}

func TestIsolationRejectsUnsupportedBackendAndUnknownPolicy(t *testing.T) {
	if err := models.ValidateNetworkPolicy(models.NetworkPolicyIsolatedWeb, "libvirt-qemu"); err == nil {
		t.Fatal("accepted unsupported network path")
	}
	if err := models.ValidateNetworkPolicy("misspelled", "firecracker"); err == nil {
		t.Fatal("accepted unknown policy")
	}
}

func TestValidateCreateRequestReportsNetworkPolicyAsValidation(t *testing.T) {
	req := &models.CreateMachineRequest{
		Name:          "research",
		Backend:       "firecracker",
		GuestOS:       "linux",
		ImageID:       "img",
		NetworkPolicy: "misspelled",
	}
	err := validateCreateRequest(req)
	if !errors.Is(err, apierr.ErrValidation) || !strings.Contains(err.Error(), "network_policy") {
		t.Fatalf("validateCreateRequest error = %v, want network_policy validation", err)
	}
}

func TestRawIngressGuardPrecedesFilterAccepts(t *testing.T) {
	alloc := Allocation{GuestIP: "172.16.0.2", GatewayIP: "172.16.0.1", TAPDevice: "kv0"}
	for _, ipv6 := range []bool{false, true} {
		script, err := isolationTableTransaction("12345678-1234-1234-1234-123456789012", alloc, ipv6, "", false, "raw")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(script, "*raw\n") || !strings.Contains(script, "-I PREROUTING 1 -i kv0") || strings.Contains(script, "conntrack") {
			t.Fatalf("invalid pre-conntrack guard: %s", script)
		}
		if !ipv6 && (!strings.Contains(script, "-d 100.64.0.0/10") || !strings.Contains(script, "-d 169.254.0.0/16") || !strings.Contains(script, "! -s 172.16.0.2/32")) {
			t.Fatal("missing private destination or spoof guard")
		}
	}
}
