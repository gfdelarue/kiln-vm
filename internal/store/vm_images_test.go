package store

import (
	"database/sql"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func createStoreTestImage(t *testing.T, db *DB, id string, now time.Time) {
	t.Helper()
	if err := db.CreateImage(&models.Image{
		ID:         id,
		Name:       "vm-image",
		Tag:        id,
		BootMode:   "persistent",
		SizeMB:     1024,
		RootfsPath: "/tmp/" + id + "/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage(%s): %v", id, err)
	}
}

func TestVMLabelsAndImageNameRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	createStoreTestImage(t, db, "img-labels", now)
	for _, id := range []string{"machine-labelled", "machine-plain"} {
		if err := db.CreateMachine(&models.Machine{
			ID: id, Name: id, ImageID: "img-labels", State: models.MachineStateStopped,
			VCPUs: 1, MemoryMB: 512, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateMachine: %v", err)
		}
	}
	labels := map[string]string{"kiln-run/env": "abc123", "owner": ""}
	if err := db.CreateVM(&models.VMRecord{
		ID: "vm-labelled", Name: "labelled", Profile: "alpine", MachineID: "machine-labelled",
		ImageID: "img-labels", VMImage: "saved-env", Labels: labels, DiskSizeMB: 1024,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVM labelled: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID: "vm-plain", Name: "plain", Profile: "alpine", MachineID: "machine-plain",
		ImageID: "img-labels", DiskSizeMB: 1024, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVM plain: %v", err)
	}

	got, err := db.GetVM("labelled")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if !maps.Equal(got.Labels, labels) || got.VMImage != "saved-env" {
		t.Fatalf("GetVM labels/image = %v/%q, want %v/saved-env", got.Labels, got.VMImage, labels)
	}
	plain, err := db.GetVMByMachineID("machine-plain")
	if err != nil {
		t.Fatalf("GetVMByMachineID: %v", err)
	}
	if plain.Labels != nil || plain.VMImage != "" {
		t.Fatalf("plain VM labels/image = %v/%q, want none", plain.Labels, plain.VMImage)
	}
	listed, err := db.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	found := false
	for _, vm := range listed {
		if vm.ID == "vm-labelled" {
			found = maps.Equal(vm.Labels, labels)
		}
	}
	if !found {
		t.Fatalf("ListVMs did not return labelled VM with labels: %+v", listed)
	}
}

func TestVMImageCreateGetListAndDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	createStoreTestImage(t, db, "img-saved", now)
	createStoreTestImage(t, db, "img-built", now)

	saved := &models.VMImage{
		ID: "vmimg-saved", Name: "saved", Profile: "alpine", Source: models.VMImageSourceVM,
		ImageID: "img-saved", DiskSizeMB: 1024, Labels: map[string]string{"env": "a"}, CreatedAt: now,
	}
	built := &models.VMImage{
		ID: "vmimg-built", Name: "built", Profile: "debian", Source: models.VMImageSourceDockerfile,
		ImageID: "img-built", DiskSizeMB: 2048, Resolved: map[string]string{"alpine:3.20": "alpine@sha256:abc"},
		CreatedAt: now.Add(time.Second),
	}
	for _, img := range []*models.VMImage{saved, built} {
		if err := db.CreateVMImage(img); err != nil {
			t.Fatalf("CreateVMImage(%s): %v", img.Name, err)
		}
	}

	byName, err := db.GetVMImage("saved")
	if err != nil {
		t.Fatalf("GetVMImage by name: %v", err)
	}
	if byName.ID != "vmimg-saved" || byName.Source != models.VMImageSourceVM || byName.ImageID != "img-saved" ||
		byName.DiskSizeMB != 1024 || !maps.Equal(byName.Labels, saved.Labels) || byName.Resolved != nil {
		t.Fatalf("GetVMImage(saved) = %+v", byName)
	}
	byID, err := db.GetVMImage("vmimg-built")
	if err != nil {
		t.Fatalf("GetVMImage by id: %v", err)
	}
	if byID.Name != "built" || !maps.Equal(byID.Resolved, built.Resolved) || byID.Labels != nil {
		t.Fatalf("GetVMImage(vmimg-built) = %+v", byID)
	}
	byImage, err := db.GetVMImageByImageID("img-built")
	if err != nil || byImage.ID != "vmimg-built" {
		t.Fatalf("GetVMImageByImageID = %+v, %v", byImage, err)
	}

	listed, err := db.ListVMImages()
	if err != nil {
		t.Fatalf("ListVMImages: %v", err)
	}
	if len(listed) != 2 || listed[0].Name != "built" || listed[1].Name != "saved" {
		t.Fatalf("ListVMImages = %+v, want newest first", listed)
	}

	duplicate := *saved
	duplicate.ID = "vmimg-duplicate"
	duplicate.ImageID = "img-built"
	if err := db.CreateVMImage(&duplicate); err == nil {
		t.Fatal("CreateVMImage accepted a duplicate name")
	}

	if err := db.DeleteVMImage("vmimg-saved"); err != nil {
		t.Fatalf("DeleteVMImage: %v", err)
	}
	if _, err := db.GetVMImage("saved"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetVMImage after delete error = %v, want sql.ErrNoRows", err)
	}
	if err := db.DeleteVMImage("vmimg-saved"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second DeleteVMImage error = %v, want sql.ErrNoRows", err)
	}
}

func TestVMImageRequiresExistingBackingImage(t *testing.T) {
	db := openTestDB(t)
	err := db.CreateVMImage(&models.VMImage{
		ID: "vmimg-orphan", Name: "orphan", Profile: "alpine", Source: models.VMImageSourceVM,
		ImageID: "missing", DiskSizeMB: 1024, CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("CreateVMImage accepted a missing backing image")
	}
}
