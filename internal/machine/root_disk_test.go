package machine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestManagerRootDiskPathIsFirecrackerOnly(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{ID: "image-root-disk", Name: "base", Tag: "latest", RootfsPath: "/tmp/rootfs.ext4", CreatedAt: now}); err != nil {
		t.Fatalf("create image: %v", err)
	}
	for _, m := range []models.Machine{
		{ID: "11111111-0000-4000-8000-000000000001", Name: "linux", Backend: "firecracker"},
		{ID: "22222222-0000-4000-8000-000000000002", Name: "windows", Backend: libvirtBackend},
	} {
		m.ImageID, m.State, m.VCPUs, m.MemoryMB, m.CreatedAt, m.UpdatedAt = "image-root-disk", models.MachineStateStopped, 1, 256, now, now
		if err := db.CreateMachine(&m); err != nil {
			t.Fatalf("create machine: %v", err)
		}
	}
	manager := NewManager(&config.Config{DataDir: dataDir, BridgeCIDR: "172.16.0.1/24"}, db)

	path, err := manager.RootDiskPath("1111")
	if err != nil {
		t.Fatalf("RootDiskPath(firecracker): %v", err)
	}
	if want := filepath.Join(dataDir, "machines", "11111111-0000-4000-8000-000000000001", "rootfs.ext4"); path != want {
		t.Fatalf("RootDiskPath = %q, want %q", path, want)
	}
	if _, err := manager.RootDiskPath("2222"); !errors.Is(err, apierr.ErrValidation) {
		t.Fatalf("RootDiskPath(libvirt) error = %v, want validation", err)
	}
	if _, err := manager.RootDiskPath("3333"); !errors.Is(err, apierr.ErrNotFound) {
		t.Fatalf("RootDiskPath(missing) error = %v, want not found", err)
	}
}
