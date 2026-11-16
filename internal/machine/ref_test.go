package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestManagerResolvesMachinePrefixesAgainstRealStore(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "aaaaaaaa-0000-4000-8000-000000000001",
		Name:       "base",
		Tag:        "latest",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: filepath.Join(dataDir, "images", "base", "rootfs.ext4"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}

	machines := []models.Machine{
		{
			ID:        "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5",
			Name:      "first",
			ImageID:   "aaaaaaaa-0000-4000-8000-000000000001",
			State:     models.MachineStateStopped,
			VCPUs:     1,
			MemoryMB:  512,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        "d4223daa-11dc-4f65-8b87-05675d4d7fba",
			Name:      "second",
			ImageID:   "aaaaaaaa-0000-4000-8000-000000000001",
			State:     models.MachineStateStopped,
			VCPUs:     1,
			MemoryMB:  512,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	for i := range machines {
		if err := db.CreateMachine(&machines[i]); err != nil {
			t.Fatalf("create machine %s: %v", machines[i].ID, err)
		}
		machineDir := filepath.Join(dataDir, "machines", machines[i].ID)
		if err := os.MkdirAll(machineDir, 0750); err != nil {
			t.Fatalf("create machine dir: %v", err)
		}
	}

	manager := NewManager(&config.Config{DataDir: dataDir, BridgeCIDR: "172.16.0.1/24"}, db)
	got, err := manager.Get("d4223daa")
	if err != nil {
		t.Fatalf("get machine by prefix: %v", err)
	}
	if got.ID != machines[1].ID {
		t.Fatalf("resolved machine %q, want %q", got.ID, machines[1].ID)
	}

	if err := manager.Delete("20f805c1"); err != nil {
		t.Fatalf("delete machine by prefix: %v", err)
	}
	if _, err := db.GetMachine(machines[0].ID); err == nil {
		t.Fatalf("deleted machine %s still exists", machines[0].ID)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "machines", machines[0].ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted machine dir still exists or stat failed unexpectedly: %v", err)
	}
}

func TestManagerRejectsAmbiguousMachinePrefixAgainstRealStore(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "aaaaaaaa-0000-4000-8000-000000000001",
		Name:       "base",
		Tag:        "latest",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: filepath.Join(dataDir, "images", "base", "rootfs.ext4"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}

	for _, id := range []string{
		"20f805c1-7be1-4e8e-94c6-ff5cda2a14c5",
		"20f805c1-ffff-4e8e-94c6-ff5cda2a14c5",
	} {
		if err := db.CreateMachine(&models.Machine{
			ID:        id,
			Name:      id,
			ImageID:   "aaaaaaaa-0000-4000-8000-000000000001",
			State:     models.MachineStateStopped,
			VCPUs:     1,
			MemoryMB:  512,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create machine %s: %v", id, err)
		}
	}

	manager := NewManager(&config.Config{DataDir: dataDir, BridgeCIDR: "172.16.0.1/24"}, db)
	_, err = manager.Get("20f805c1")
	if err == nil {
		t.Fatal("expected ambiguous prefix error")
	}
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}
