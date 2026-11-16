package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestDiskHandlerUsesRealStoreAndFilesystem(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rootfs := filepath.Join(dataDir, "images", "orphan-image", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(rootfs), 0750); err != nil {
		t.Fatalf("MkdirAll(rootfs): %v", err)
	}
	if err := hostfs.CreateSparseFile(rootfs, 128, 0640); err != nil {
		t.Fatalf("CreateSparseFile: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	handler := NewDiskHandler(disk.NewService(db, dataDir, ""))
	usageResp := httptest.NewRecorder()
	handler.Usage(usageResp, httptest.NewRequest(http.MethodGet, "/disk/usage", nil))
	if usageResp.Code != http.StatusOK {
		t.Fatalf("usage status = %d body=%s", usageResp.Code, usageResp.Body.String())
	}
	var usage models.DiskUsageReport
	if err := json.Unmarshal(usageResp.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if usage.Totals.ReclaimableBytes == 0 {
		t.Fatalf("expected reclaimable bytes: %+v", usage)
	}

	body, _ := json.Marshal(models.CleanupRequest{DryRun: false})
	cleanupResp := httptest.NewRecorder()
	handler.Cleanup(cleanupResp, httptest.NewRequest(http.MethodPost, "/cleanup", bytes.NewReader(body)))
	if cleanupResp.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d body=%s", cleanupResp.Code, cleanupResp.Body.String())
	}
	if _, err := os.Stat(filepath.Dir(rootfs)); !os.IsNotExist(err) {
		t.Fatalf("orphan image directory still exists or stat failed unexpectedly after cleanup: %v", err)
	}
}

func TestDiskHandlerRejectsInvalidCleanupJSON(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	t.Setenv("TMPDIR", t.TempDir())
	handler := NewDiskHandler(disk.NewService(db, dataDir, ""))
	resp := httptest.NewRecorder()
	handler.Cleanup(resp, httptest.NewRequest(http.MethodPost, "/cleanup", bytes.NewBufferString("{")))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestDiskHandlerDoesNotTreatRecentScratchAsReclaimable(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	scratch := filepath.Join(dataDir, "contexts", "recent")
	if err := os.MkdirAll(scratch, 0750); err != nil {
		t.Fatalf("MkdirAll(scratch): %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(scratch, now, now); err != nil {
		t.Fatalf("Chtimes(scratch): %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	handler := NewDiskHandler(disk.NewService(db, dataDir, ""))
	resp := httptest.NewRecorder()
	handler.Usage(resp, httptest.NewRequest(http.MethodGet, "/disk/usage", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var usage models.DiskUsageReport
	if err := json.Unmarshal(resp.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	for _, entry := range usage.Entries {
		if entry.ID == "recent" && entry.Reclaimable {
			t.Fatalf("recent scratch should not be reclaimable: %+v", entry)
		}
	}
}
