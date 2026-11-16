package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestCopyFilePreservesSparseRootfsAllocation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "dst.ext4")
	if err := hostfs.CreateSparseFile(src, 256, 0640); err != nil {
		t.Fatalf("CreateSparseFile(src): %v", err)
	}
	f, err := os.OpenFile(src, os.O_WRONLY, 0640)
	if err != nil {
		t.Fatalf("OpenFile(src): %v", err)
	}
	if _, err := f.WriteAt([]byte("kiln-vm-machine-rootfs"), 192*1024*1024); err != nil {
		f.Close()
		t.Fatalf("WriteAt(src): %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(src): %v", err)
	}

	if err := copyFile(context.Background(), src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat(dst): %v", err)
	}
	if info.Size() != 256*1024*1024 {
		t.Fatalf("dst size = %d, want %d", info.Size(), int64(256*1024*1024))
	}
	allocated, err := hostfs.AllocatedBytes(dst)
	if err != nil {
		t.Fatalf("AllocatedBytes(dst): %v", err)
	}
	if allocated > 8*1024*1024 {
		t.Fatalf("copied rootfs allocated %d bytes, want <= 8MiB", allocated)
	}
}

func TestCreateRemovesMachineDirWhenNetworkSetupFailsAfterRootfsCopy(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rootfs := filepath.Join(dataDir, "images", "img-1", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(rootfs), 0750); err != nil {
		t.Fatalf("MkdirAll(rootfs): %v", err)
	}
	if err := hostfs.CreateSparseFile(rootfs, 128, 0640); err != nil {
		t.Fatalf("CreateSparseFile(rootfs): %v", err)
	}
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "img-1",
		Name:       "base",
		Tag:        "latest",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: rootfs,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	t.Setenv("PATH", t.TempDir())
	manager := NewManager(&config.Config{DataDir: dataDir, BridgeCIDR: "172.16.0.0/16"}, db)
	_, err = manager.Create(context.Background(), &models.CreateMachineRequest{
		Name:     "will-fail",
		ImageID:  "img-1",
		VCPUs:    1,
		MemoryMB: 512,
	})
	if err == nil {
		t.Fatal("expected network setup failure")
	}
	machines, err := db.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(machines) != 0 {
		t.Fatalf("machine DB row was left behind: %+v", machines)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "machines"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir(machines): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("machine directories were left behind: %+v", entries)
	}
}
