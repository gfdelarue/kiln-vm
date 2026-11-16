package machine

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestMachineIDFromFirecrackerArgs(t *testing.T) {
	dataDir := filepath.Join("tmp", "kiln-vm")
	configPath := filepath.Join(dataDir, "machines", "machine-123", "firecracker.sock.json")

	machineID, gotConfigPath := machineIDFromFirecrackerArgs([]string{
		"/usr/bin/firecracker",
		"--api-sock", filepath.Join(dataDir, "machines", "machine-123", "firecracker.sock"),
		"--config-file", configPath,
	}, dataDir)

	if machineID != "machine-123" {
		t.Fatalf("machineID = %q, want machine-123", machineID)
	}
	if gotConfigPath != filepath.Clean(configPath) {
		t.Fatalf("configPath = %q, want %q", gotConfigPath, filepath.Clean(configPath))
	}
}

func TestMachineIDFromFirecrackerArgsRejectsPathsOutsideTheDataDir(t *testing.T) {
	dataDir := filepath.Join("tmp", "kiln-vm")
	for _, args := range [][]string{
		{"/usr/bin/firecracker"},
		{"/usr/bin/firecracker", "--config-file", filepath.Join("tmp", "other", "machines", "machine-123", "firecracker.sock.json")},
		{"/usr/bin/firecracker", "--config-file", filepath.Join(dataDir, "machines", "machine-123", "other.json")},
		{"/usr/bin/firecracker", "--config-file", filepath.Join(dataDir, "images", "machine-123", "firecracker.sock.json")},
	} {
		if machineID, configPath := machineIDFromFirecrackerArgs(args, dataDir); machineID != "" || configPath != "" {
			t.Fatalf("machineIDFromFirecrackerArgs(%v) = %q, %q; want empty", args, machineID, configPath)
		}
	}
}

func TestFirecrackerConfigPathFromArgsSupportsEqualsForm(t *testing.T) {
	got := firecrackerConfigPathFromArgs([]string{"/usr/bin/firecracker", "--config-file=/tmp/config.json"})
	if got != "/tmp/config.json" {
		t.Fatalf("config path = %q, want /tmp/config.json", got)
	}
}

func TestSplitProcCmdline(t *testing.T) {
	got := splitProcCmdline([]byte("firecracker\x00--config-file\x00/tmp/config.json\x00"))
	want := []string{"firecracker", "--config-file", "/tmp/config.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitProcCmdline = %#v, want %#v", got, want)
	}
}
