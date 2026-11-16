package libvirtnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

type fakeVirsh struct {
	defined   bool
	active    bool
	autostart bool
	calls     []string
	xml       string
}

func (f *fakeVirsh) run(_ context.Context, args ...string) (string, error) {
	command := args[2]
	f.calls = append(f.calls, command)
	switch command {
	case "net-info":
		if !f.defined {
			return "", errors.New("network not found")
		}
		return "Name: kiln-vm\nActive: " + yesNo(f.active) + "\nAutostart: " + yesNo(f.autostart) + "\n", nil
	case "net-define":
		data, err := os.ReadFile(args[3])
		if err != nil {
			return "", fmt.Errorf("read network xml: %w", err)
		}
		f.xml = string(data)
		f.defined = true
	case "net-start":
		f.active = true
	case "net-autostart":
		f.autostart = true
	}
	return "", nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func TestEnsureDefinesStartsAndAutostartsTheManagedNetwork(t *testing.T) {
	fake := &fakeVirsh{}
	network := Network{ConnectURI: "qemu:///system", Name: ManagedName, CIDR: "10.232.0.0/24", Run: fake.run}

	if err := network.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if want := []string{"net-info", "net-define", "net-info", "net-start", "net-autostart"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("virsh calls = %v, want %v", fake.calls, want)
	}
	for _, want := range []string{"<name>kiln-vm</name>", "<bridge name='kiln-vm-br0'", "<ip address='10.232.0.1' netmask='255.255.255.0'>", "<range start='10.232.0.2' end='10.232.0.254'/>"} {
		if !strings.Contains(fake.xml, want) {
			t.Fatalf("network XML missing %q:\n%s", want, fake.xml)
		}
	}
}

func TestEnsureLeavesAnActiveManagedNetworkAlone(t *testing.T) {
	fake := &fakeVirsh{defined: true, active: true, autostart: true}
	network := Network{Name: ManagedName, Run: fake.run}

	if err := network.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if want := []string{"net-info"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("virsh calls = %v, want %v", fake.calls, want)
	}
}

func TestEnsureNeverChangesANetworkKilnVMDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fake    *fakeVirsh
		wantErr string
	}{
		{name: "missing", fake: &fakeVirsh{}, wantErr: "not available"},
		{name: "inactive", fake: &fakeVirsh{defined: true}, wantErr: "not active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := Network{Name: "default", Run: tc.fake.run}

			err := network.Ensure(context.Background())

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Ensure error = %v, want %q", err, tc.wantErr)
			}
			if want := []string{"net-info"}; !slices.Equal(tc.fake.calls, want) {
				t.Fatalf("virsh calls = %v, want %v", tc.fake.calls, want)
			}
		})
	}
}

func TestManagedXMLRejectsUnusableCIDRs(t *testing.T) {
	for _, cidr := range []string{"10.232.0.0/30", "10.232.0.5/24", "fd00::/64", "nope"} {
		if _, err := ManagedXML(cidr); err == nil {
			t.Fatalf("ManagedXML(%q) succeeded, want error", cidr)
		}
	}
	xml, err := ManagedXML("192.168.50.0/29")
	if err != nil {
		t.Fatalf("ManagedXML /29: %v", err)
	}
	if !strings.Contains(xml, "<range start='192.168.50.2' end='192.168.50.6'/>") {
		t.Fatalf("unexpected /29 plan:\n%s", xml)
	}
}
