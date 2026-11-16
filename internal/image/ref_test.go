package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestServiceResolvesImagePrefixesAgainstRealStore(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	images := []models.Image{
		{ID: "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5", Name: "first", Tag: "latest", BootMode: BootModePersistent, SizeMB: 128, RootfsPath: "/tmp/first", CreatedAt: now},
		{ID: "20f805c1-ffff-4e8e-94c6-ff5cda2a14c5", Name: "second", Tag: "latest", BootMode: BootModePersistent, SizeMB: 128, RootfsPath: "/tmp/second", CreatedAt: now},
		{ID: "d4223daa-11dc-4f65-8b87-05675d4d7fba", Name: "third", Tag: "latest", BootMode: BootModePersistent, SizeMB: 128, RootfsPath: "/tmp/third", CreatedAt: now},
	}
	for i := range images {
		if err := db.CreateImage(&images[i]); err != nil {
			t.Fatalf("create image %s: %v", images[i].ID, err)
		}
	}

	service := NewService(db, t.TempDir(), "")
	img, err := service.Get("d4223daa")
	if err != nil {
		t.Fatalf("get image by prefix: %v", err)
	}
	if img.ID != images[2].ID {
		t.Fatalf("resolved image %q, want %q", img.ID, images[2].ID)
	}

	if err := service.Delete("20f805c17"); err != nil {
		t.Fatalf("delete image by prefix: %v", err)
	}
	if _, err := db.GetImage(images[0].ID); err == nil {
		t.Fatalf("deleted image %s still exists", images[0].ID)
	}

	img, err = service.Get("20f805c1")
	if err != nil {
		t.Fatalf("get remaining image by formerly ambiguous prefix: %v", err)
	}
	if img.ID != images[1].ID {
		t.Fatalf("resolved image %q, want %q", img.ID, images[1].ID)
	}
}

func TestServiceImportsRootfsIntoRealStore(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	dataDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := createSparseFile(source, 128); err != nil {
		t.Fatalf("create source rootfs: %v", err)
	}
	file, err := os.OpenFile(source, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open source rootfs: %v", err)
	}
	if _, err := file.WriteAt([]byte("imported-rootfs"), 64*1024*1024); err != nil {
		file.Close()
		t.Fatalf("write source marker: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	service := NewService(db, dataDir, "")
	img, err := service.ImportRootfs(context.Background(), &models.ImportImageRequest{
		Name:         "imported",
		Tag:          "v1",
		RootfsPath:   source,
		RootfsSizeMB: 128,
		BootMode:     BootModePersistent,
	})
	if err != nil {
		t.Fatalf("ImportRootfs: %v", err)
	}
	if img.ID == "" || img.SizeMB != 128 || img.BootMode != BootModePersistent {
		t.Fatalf("imported image = %+v", img)
	}
	stored, err := db.GetImage(img.ID)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if _, err := os.Stat(stored.RootfsPath); err != nil {
		t.Fatalf("stat stored rootfs: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source rootfs was not preserved for non-windows import: %v", err)
	}
	allocated, err := allocatedBytes(stored.RootfsPath)
	if err != nil {
		t.Fatalf("allocatedBytes: %v", err)
	}
	if allocated > 8*1024*1024 {
		t.Fatalf("imported sparse rootfs allocated %d bytes, want <= 8MiB", allocated)
	}
}

func TestServiceImportsBootableQCOW2WithoutExt4SizeCheck(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	dataDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "windows-os.qcow2")
	if err := createSparseFile(source, 128); err != nil {
		t.Fatalf("create source qcow2: %v", err)
	}
	file, err := os.OpenFile(source, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open source qcow2: %v", err)
	}
	if _, err := file.WriteAt([]byte("windows-qcow2"), 64*1024*1024); err != nil {
		file.Close()
		t.Fatalf("write source marker: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close source qcow2: %v", err)
	}

	service := NewService(db, dataDir, "")
	img, err := service.ImportRootfs(context.Background(), &models.ImportImageRequest{
		Name:         "windows-os",
		Tag:          "v1",
		RootfsPath:   source,
		RootfsSizeMB: 64 * 1024,
		BootMode:     BootModeBootableQCOW2,
	})
	if err != nil {
		t.Fatalf("ImportRootfs(bootable qcow2): %v", err)
	}
	if img.BootMode != BootModeBootableQCOW2 || img.SizeMB != 64*1024 {
		t.Fatalf("imported image = %+v", img)
	}
	if filepath.Base(img.RootfsPath) != "disk.qcow2" {
		t.Fatalf("stored disk path = %q", img.RootfsPath)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source qcow2 was not preserved after bootable import: %v", err)
	}
	data := make([]byte, len("windows-qcow2"))
	stored, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open stored qcow2: %v", err)
	}
	if _, err := stored.ReadAt(data, 64*1024*1024); err != nil {
		stored.Close()
		t.Fatalf("read stored marker: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("close stored qcow2: %v", err)
	}
	if string(data) != "windows-qcow2" {
		t.Fatalf("stored qcow2 content = %q", string(data))
	}
	allocated, err := allocatedBytes(img.RootfsPath)
	if err != nil {
		t.Fatalf("allocatedBytes: %v", err)
	}
	if allocated > 8*1024*1024 {
		t.Fatalf("imported sparse qcow2 allocated %d bytes, want <= 8MiB", allocated)
	}
}

func TestServiceRejectsBootableQCOW2DockerfileBuild(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	service := NewService(db, t.TempDir(), "")
	_, err = service.Build(context.Background(), &models.BuildImageRequest{
		Name:         "windows-os",
		Dockerfile:   "FROM scratch\n",
		RootfsSizeMB: 128,
		BootMode:     BootModeBootableQCOW2,
	})
	if !errors.Is(err, apierr.ErrValidation) {
		t.Fatalf("Build bootable qcow2 error = %v, want validation", err)
	}
	if !strings.Contains(err.Error(), "cannot be built from a Dockerfile") {
		t.Fatalf("Build bootable qcow2 error = %v", err)
	}
}

func TestServiceRejectsUnknownBootMode(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	service := NewService(db, t.TempDir(), "")
	_, err = service.Build(context.Background(), &models.BuildImageRequest{
		Name:         "unknown-mode",
		Dockerfile:   "FROM scratch\n",
		RootfsSizeMB: 128,
		BootMode:     "iso-installer",
	})
	if err == nil {
		t.Fatal("Build with unknown boot mode succeeded")
	}
	if !strings.Contains(err.Error(), BootModeBootableQCOW2) {
		t.Fatalf("Build unknown boot mode error = %v", err)
	}
}

func TestServiceRejectsAmbiguousImagePrefixAgainstRealStore(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	images := []models.Image{
		{ID: "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5", Name: "first", Tag: "latest", BootMode: BootModePersistent, SizeMB: 128, RootfsPath: "/tmp/first", CreatedAt: now},
		{ID: "20f805c1-ffff-4e8e-94c6-ff5cda2a14c5", Name: "second", Tag: "latest", BootMode: BootModePersistent, SizeMB: 128, RootfsPath: "/tmp/second", CreatedAt: now},
	}
	for i := range images {
		if err := db.CreateImage(&images[i]); err != nil {
			t.Fatalf("create image %s: %v", images[i].ID, err)
		}
	}

	service := NewService(db, t.TempDir(), "")
	_, err = service.Get("20f805c1")
	if err == nil {
		t.Fatal("expected ambiguous prefix error")
	}
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}
