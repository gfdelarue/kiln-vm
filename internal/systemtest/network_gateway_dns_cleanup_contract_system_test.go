//go:build system

package systemtest

import (
	"path/filepath"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestNetworkGatewayDNSBringdownTargetMatchersAreExact(t *testing.T) {
	t.Run("Firecracker argv accepts both supported forms", func(t *testing.T) {
		tests := []struct {
			name       string
			argv       []byte
			wantSocket string
			wantConfig string
		}{
			{
				name:       "separate values",
				argv:       []byte("firecracker\x00--api-sock\x00/var/lib/kiln-vm/machines/machine-123/firecracker.sock\x00--config-file\x00/var/lib/kiln-vm/machines/machine-123/firecracker.sock.json\x00"),
				wantSocket: "/var/lib/kiln-vm/machines/machine-123/firecracker.sock",
				wantConfig: "/var/lib/kiln-vm/machines/machine-123/firecracker.sock.json",
			},
			{
				name:       "equals values",
				argv:       []byte("firecracker\x00--config-file=/tmp/config.json\x00--api-sock=/tmp/firecracker.sock\x00"),
				wantSocket: "/tmp/firecracker.sock",
				wantConfig: "/tmp/config.json",
			},
			{
				name: "unrelated arguments",
				argv: []byte("firecracker\x00--id\x00machine-123\x00"),
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				socket, config := firecrackerRuntimeArgs(test.argv)
				if socket != test.wantSocket || config != test.wantConfig {
					t.Fatalf("runtime args = socket:%q config:%q, want socket:%q config:%q", socket, config, test.wantSocket, test.wantConfig)
				}
			})
		}
	})

	t.Run("disk inventory matches components not substrings", func(t *testing.T) {
		vm := &models.VM{MachineID: "machine-123", Name: "gateway-dns-target"}
		runtimeDir := "/var/lib/kiln-vm/machines/machine-123"
		tests := []struct {
			name  string
			entry models.DiskUsageEntry
			want  bool
		}{
			{name: "machine ID", entry: models.DiskUsageEntry{ID: vm.MachineID, Path: "/var/lib/kiln-vm/images/rootfs"}, want: true},
			{name: "consumer name", entry: models.DiskUsageEntry{Name: vm.Name, Path: "/var/lib/kiln-vm/deployments/target"}, want: true},
			{name: "runtime directory", entry: models.DiskUsageEntry{Path: runtimeDir}, want: true},
			{name: "runtime child", entry: models.DiskUsageEntry{Path: runtimeDir + "/rootfs.ext4"}, want: true},
			{name: "exact scratch component", entry: models.DiskUsageEntry{Path: "/var/lib/kiln-vm/scratch/machine-123/build"}, want: true},
			{name: "nearby machine", entry: models.DiskUsageEntry{Path: "/var/lib/kiln-vm/machines/machine-1234/rootfs.ext4"}},
			{name: "substring only", entry: models.DiskUsageEntry{Path: "/var/lib/kiln-vm/scratch/prefix-machine-123-suffix/build"}},
			{name: "unrelated", entry: models.DiskUsageEntry{ID: "machine-999", Name: "other", Path: "/var/lib/kiln-vm/machines/machine-999"}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				entryPath := filepath.Clean(test.entry.Path)
				if got := diskEntryTargetsVM(test.entry, entryPath, runtimeDir, vm); got != test.want {
					t.Fatalf("target match = %t, want %t for %+v", got, test.want, test.entry)
				}
			})
		}
	})
}
