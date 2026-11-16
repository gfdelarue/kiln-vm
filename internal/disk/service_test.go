package disk

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestUsageClassifiesReferencedAndOrphanedDataWithRealStore(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	createSparseRootfs(t, filepath.Join(dataDir, "images", "img-used", "rootfs.ext4"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "images", "orphan-image", "rootfs.ext4"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "machines", "machine-used", "rootfs.ext4"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "machines", "orphan-machine", "rootfs.ext4"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "windows-profiles", "cache", "Win11.iso"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "windows-profiles", "cache", "node.zip"), 8)
	oldTime := now.Add(-2 * staleScratchAge)
	if err := os.Chtimes(filepath.Join(dataDir, "windows-profiles", "cache", "Win11.iso"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(windows iso cache): %v", err)
	}
	if err := db.CreateImage(&models.Image{
		ID:         "img-used",
		Name:       "base",
		Tag:        "latest",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: filepath.Join(dataDir, "images", "img-used", "rootfs.ext4"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-used",
		Name:      "vm-used",
		ImageID:   "img-used",
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	report, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	requireEntry(t, report, "image", "img-used", false)
	requireEntry(t, report, "image", "orphan-image", true)
	requireEntry(t, report, "machine", "machine-used", false)
	requireEntry(t, report, "machine", "orphan-machine", true)
	requireEntry(t, report, "windows-profile-cache", "Win11.iso", true)
	requireEntry(t, report, "windows-profile-cache", "node.zip", false)
	if report.Totals.AllocatedBytes == 0 {
		t.Fatal("expected allocated bytes to be measured")
	}
	if report.Totals.ReclaimableBytes == 0 {
		t.Fatal("expected orphaned bytes to be reclaimable")
	}
}

func TestUsageWarnsWhenReclaimableDataIsLarge(t *testing.T) {
	oldThreshold := diskLargeReclaimableWarningBytes
	diskLargeReclaimableWarningBytes = 1
	t.Cleanup(func() { diskLargeReclaimableWarningBytes = oldThreshold })

	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	createSparseRootfs(t, filepath.Join(dataDir, "images", "orphan-image", "rootfs.ext4"), 128)

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	report, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if !hasDiskWarning(report.Warnings, "large_reclaimable_data") {
		t.Fatalf("warnings = %+v, want large reclaimable warning", report.Warnings)
	}
}

func TestUsageWarningsIncludeConfiguredTempFilesystem(t *testing.T) {
	oldThreshold := diskLowAvailableWarningBytes
	diskLowAvailableWarningBytes = ^uint64(0)
	t.Cleanup(func() { diskLowAvailableWarningBytes = oldThreshold })

	dataDir := t.TempDir()
	tempDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	t.Setenv("TMPDIR", tempDir)
	svc := NewService(db, dataDir, "")
	report, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, warning := range report.Warnings {
		if warning.Code == "low_available_disk" && warning.Path == tempDir {
			return
		}
	}
	t.Fatalf("warnings = %+v, want low-space warning coverage for temp filesystem %s", report.Warnings, tempDir)
}

func TestCleanupRemovesStaleTempDataOnly(t *testing.T) {
	dataDir := t.TempDir()
	tempDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	staleRootfs := filepath.Join(tempDir, "kiln-vm-rootfs-old")
	recentRootfs := filepath.Join(tempDir, "kiln-vm-rootfs-recent")
	staleProfileRootfs := filepath.Join(tempDir, "kiln-vm-profile-rootfs-old.ext4")
	otherTemp := filepath.Join(tempDir, "other-temp")
	createSparseRootfs(t, filepath.Join(staleRootfs, "marker"), 1)
	createSparseRootfs(t, filepath.Join(recentRootfs, "marker"), 1)
	createSparseRootfs(t, staleProfileRootfs, 1)
	createSparseRootfs(t, filepath.Join(otherTemp, "marker"), 1)
	oldTime := now.Add(-2 * staleScratchAge)
	if err := os.Chtimes(staleRootfs, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(stale rootfs): %v", err)
	}
	if err := os.Chtimes(filepath.Join(staleRootfs, "marker"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(stale rootfs marker): %v", err)
	}
	if err := os.Chtimes(staleProfileRootfs, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(stale profile rootfs): %v", err)
	}

	t.Setenv("TMPDIR", tempDir)
	svc := NewService(db, dataDir, "")
	usage, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	requireEntry(t, usage, "kiln-vm-temp", "kiln-vm-rootfs-old", true)
	requireEntry(t, usage, "kiln-vm-temp", "kiln-vm-rootfs-recent", false)
	requireEntry(t, usage, "kiln-vm-temp", "kiln-vm-profile-rootfs-old.ext4", true)
	if findOptionalEntry(usage, "kiln-vm-temp", "other-temp") != nil {
		t.Fatalf("non-kiln-vm temp entry was reported: %+v", usage.Entries)
	}

	report, err := svc.Cleanup(false)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	requireRemoved(t, report, "kiln-vm-temp", "kiln-vm-rootfs-old")
	requireRemoved(t, report, "kiln-vm-temp", "kiln-vm-profile-rootfs-old.ext4")
	if _, err := os.Stat(staleRootfs); !os.IsNotExist(err) {
		t.Fatalf("stale rootfs temp still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(staleProfileRootfs); !os.IsNotExist(err) {
		t.Fatalf("stale profile rootfs temp still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(recentRootfs); err != nil {
		t.Fatalf("recent rootfs temp was removed: %v", err)
	}
	if _, err := os.Stat(otherTemp); err != nil {
		t.Fatalf("non-kiln-vm temp was removed: %v", err)
	}
}

func TestCleanupRemovesOnlyReclaimableData(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	createSparseRootfs(t, filepath.Join(dataDir, "images", "img-used", "rootfs.ext4"), 128)
	createSparseRootfs(t, filepath.Join(dataDir, "images", "orphan-image", "rootfs.ext4"), 128)
	staleScratch := filepath.Join(dataDir, "contexts", "old-context")
	createSparseRootfs(t, filepath.Join(staleScratch, "rootfs.ext4"), 128)
	staleISO := filepath.Join(dataDir, "windows-profiles", "cache", "Win11.iso")
	recentISO := filepath.Join(dataDir, "windows-profiles", "cache", "RecentWin11.iso")
	payloadCache := filepath.Join(dataDir, "windows-profiles", "cache", "node.zip")
	createSparseRootfs(t, staleISO, 128)
	createSparseRootfs(t, recentISO, 128)
	createSparseRootfs(t, payloadCache, 8)
	oldTime := now.Add(-2 * staleScratchAge)
	if err := os.Chtimes(staleScratch, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(stale scratch): %v", err)
	}
	if err := os.Chtimes(staleISO, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(stale windows iso): %v", err)
	}
	if err := db.CreateImage(&models.Image{
		ID:         "img-used",
		Name:       "base",
		Tag:        "latest",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: filepath.Join(dataDir, "images", "img-used", "rootfs.ext4"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	dryRun, err := svc.Cleanup(true)
	if err != nil {
		t.Fatalf("Cleanup(dry-run): %v", err)
	}
	if len(dryRun.Removed) != 3 {
		t.Fatalf("dry-run removed count = %d, want 3: %+v", len(dryRun.Removed), dryRun.Removed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "images", "orphan-image")); err != nil {
		t.Fatalf("dry-run removed orphan image: %v", err)
	}

	report, err := svc.Cleanup(false)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(report.Removed) != 3 {
		t.Fatalf("removed count = %d, want 3: %+v", len(report.Removed), report.Removed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "images", "img-used")); err != nil {
		t.Fatalf("referenced image was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "images", "orphan-image")); !os.IsNotExist(err) {
		t.Fatalf("orphan image still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(staleScratch); !os.IsNotExist(err) {
		t.Fatalf("stale scratch still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(staleISO); !os.IsNotExist(err) {
		t.Fatalf("stale windows iso cache still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(recentISO); err != nil {
		t.Fatalf("recent windows iso cache was removed: %v", err)
	}
	if _, err := os.Stat(payloadCache); err != nil {
		t.Fatalf("windows payload cache was removed: %v", err)
	}
}

func TestUsageMarksSupersededProfileImageCachesReclaimable(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	oldUbuntu := createProfileImageCache(t, db, dataDir, "profile-old-ubuntu", "img-old-ubuntu", "ubuntu-docker", 4096, "old-fingerprint", now.Add(-2*time.Hour))
	currentUbuntu := createProfileImageCache(t, db, dataDir, "profile-current-ubuntu", "img-current-ubuntu", "ubuntu-docker", 4096, "current-fingerprint", now)
	windows := createProfileImageCache(t, db, dataDir, "profile-windows", "img-windows", "windows11", 65536, "windows-fingerprint", now.Add(-4*time.Hour))

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	report, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	requireEntry(t, report, "vm-profile-image", oldUbuntu.ID, true)
	oldEntry := findEntry(t, report, "vm-profile-image", oldUbuntu.ID)
	if oldEntry.State != "superseded" || oldEntry.Reason != "superseded-vm-profile-image-cache" {
		t.Fatalf("old profile cache entry = %+v, want superseded reclaimable cache", oldEntry)
	}
	requireEntry(t, report, "vm-profile-image", currentUbuntu.ID, false)
	currentEntry := findEntry(t, report, "vm-profile-image", currentUbuntu.ID)
	if currentEntry.State != "profile-cache" || currentEntry.Reason != "latest-vm-profile-image-cache" {
		t.Fatalf("current profile cache entry = %+v, want retained latest cache explanation", currentEntry)
	}
	requireEntry(t, report, "vm-profile-image", windows.ID, false)
}

func TestUsageMarksUnusedProfileImageCachesReclaimableWhenBudgetExceeded(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	oldest := createProfileImageCache(t, db, dataDir, "profile-oldest", "img-oldest", "ubuntu-docker", 4096, "fingerprint", now.Add(-3*time.Hour))
	middle := createProfileImageCache(t, db, dataDir, "profile-middle", "img-middle", "ubuntu-docker", 6144, "fingerprint", now.Add(-2*time.Hour))
	newest := createProfileImageCache(t, db, dataDir, "profile-newest", "img-newest", "ubuntu-docker", 8192, "fingerprint", now.Add(-time.Hour))
	allocateProfileImageCacheBytes(t, dataDir, oldest.ImageID)
	allocateProfileImageCacheBytes(t, dataDir, middle.ImageID)
	allocateProfileImageCacheBytes(t, dataDir, newest.ImageID)

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "", WithProfileImageCacheMaxBytes(1))
	report, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, cache := range []models.VMProfileImage{oldest, middle, newest} {
		entry := findEntry(t, report, "vm-profile-image", cache.ID)
		if !entry.Reclaimable || entry.State != "profile-cache-over-budget" || entry.Reason != "profile-image-cache-budget-exceeded" {
			t.Fatalf("over-budget cache entry %s = %+v, want budget reclaimable", cache.ID, entry)
		}
		if entry.ReclaimableBytes == 0 {
			t.Fatalf("over-budget cache entry %s has zero reclaimable bytes: %+v", cache.ID, entry)
		}
	}
}

func TestCleanupDoesNotReclaimCurrentProfileImageCacheUsedByVMUnderBudgetPressure(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	activeCache := createProfileImageCache(t, db, dataDir, "profile-active", "img-active", "ubuntu-docker", 4096, "fingerprint", now)
	allocateProfileImageCacheBytes(t, dataDir, activeCache.ImageID)
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-active-cache",
		Name:      "active-cache-user",
		ImageID:   activeCache.ImageID,
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-active-cache",
		Name:       "active-cache-user",
		Profile:    "ubuntu-docker",
		MachineID:  "machine-active-cache",
		ImageID:    activeCache.ImageID,
		DiskSizeMB: activeCache.RootfsSizeMB,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "", WithProfileImageCacheMaxBytes(1))
	report, err := svc.Cleanup(false)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, removed := range report.Removed {
		if removed.Kind == "vm-profile-image" && removed.ID == activeCache.ID {
			t.Fatalf("active profile image cache was removed under budget pressure: %+v", removed)
		}
	}
	entry := findEntry(t, &report.Usage, "vm-profile-image", activeCache.ID)
	if entry.Reclaimable || entry.State != "profile-cache-in-use" || entry.Reason != "active-vm-profile-image-cache" {
		t.Fatalf("active profile image cache entry = %+v, want protected active cache explanation", entry)
	}
	if _, err := db.GetVMProfileImage(activeCache.ID); err != nil {
		t.Fatalf("active cache record removed: %v", err)
	}
}

func TestCleanupDeletesSupersededProfileImageCacheRecordsAndBackingImage(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	oldCache := createProfileImageCache(t, db, dataDir, "profile-old", "img-old", "ubuntu-docker", 4096, "old-fingerprint", now.Add(-time.Hour))
	currentCache := createProfileImageCache(t, db, dataDir, "profile-current", "img-current", "ubuntu-docker", 4096, "current-fingerprint", now)
	oldImageDir := filepath.Join(dataDir, "images", oldCache.ImageID)
	currentImageDir := filepath.Join(dataDir, "images", currentCache.ImageID)

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	report, err := svc.Cleanup(false)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("cleanup failures: %+v", report.Failed)
	}
	requireRemoved(t, report, "vm-profile-image", oldCache.ID)
	if _, err := db.GetVMProfileImage(oldCache.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old profile image cache record still exists or failed unexpectedly: %v", err)
	}
	if _, err := db.GetImage(oldCache.ImageID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old backing image record still exists or failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(oldImageDir); !os.IsNotExist(err) {
		t.Fatalf("old backing image dir still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := db.GetVMProfileImage(currentCache.ID); err != nil {
		t.Fatalf("current profile image cache record removed: %v", err)
	}
	if _, err := os.Stat(currentImageDir); err != nil {
		t.Fatalf("current backing image dir removed: %v", err)
	}
}

func TestCleanupDoesNotReclaimSupersededProfileImageCacheUsedByVM(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	oldCache := createProfileImageCache(t, db, dataDir, "profile-old", "img-old", "ubuntu-docker", 4096, "old-fingerprint", now.Add(-time.Hour))
	createProfileImageCache(t, db, dataDir, "profile-current", "img-current", "ubuntu-docker", 4096, "current-fingerprint", now)
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-old-cache",
		Name:      "old-cache-user",
		ImageID:   oldCache.ImageID,
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-old-cache",
		Name:       "old-cache-user",
		Profile:    "ubuntu-docker",
		MachineID:  "machine-old-cache",
		ImageID:    oldCache.ImageID,
		DiskSizeMB: oldCache.RootfsSizeMB,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	svc := NewService(db, dataDir, "")
	usage, err := svc.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	oldEntry := findEntry(t, usage, "vm-profile-image", oldCache.ID)
	if oldEntry.Reclaimable || oldEntry.State != "superseded-in-use" {
		t.Fatalf("old in-use cache entry = %+v, want protected superseded-in-use", oldEntry)
	}
	report, err := svc.Cleanup(false)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, removed := range report.Removed {
		if removed.Kind == "vm-profile-image" && removed.ID == oldCache.ID {
			t.Fatalf("in-use profile image cache was removed: %+v", removed)
		}
	}
	if _, err := db.GetVMProfileImage(oldCache.ID); err != nil {
		t.Fatalf("in-use old profile cache record removed: %v", err)
	}
}

func createSparseRootfs(t *testing.T, path string, sizeMB int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := hostfs.CreateSparseFile(path, sizeMB, 0640); err != nil {
		t.Fatalf("CreateSparseFile(%s): %v", path, err)
	}
}

func createProfileImageCache(t *testing.T, db *store.DB, dataDir, profileID, imageID, profile string, rootfsSizeMB int64, fingerprint string, createdAt time.Time) models.VMProfileImage {
	t.Helper()
	rootfs := filepath.Join(dataDir, "images", imageID, "rootfs.ext4")
	createSparseRootfs(t, rootfs, 128)
	if err := db.CreateImage(&models.Image{
		ID:         imageID,
		Name:       "kiln-vm-profile-" + profile,
		Tag:        fingerprint,
		BootMode:   "persistent",
		SizeMB:     rootfsSizeMB,
		RootfsPath: rootfs,
		CreatedAt:  createdAt,
	}); err != nil {
		t.Fatalf("CreateImage(%s): %v", imageID, err)
	}
	record := models.VMProfileImage{
		ID:           profileID,
		Profile:      profile,
		ImageID:      imageID,
		Fingerprint:  fingerprint,
		RootfsSizeMB: rootfsSizeMB,
		CreatedAt:    createdAt,
	}
	if err := db.CreateVMProfileImage(&record); err != nil {
		t.Fatalf("CreateVMProfileImage(%s): %v", profileID, err)
	}
	return record
}

func allocateProfileImageCacheBytes(t *testing.T, dataDir, imageID string) {
	t.Helper()
	payloadPath := filepath.Join(dataDir, "images", imageID, "payload.bin")
	payload := make([]byte, 8192)
	if err := os.WriteFile(payloadPath, payload, 0640); err != nil {
		t.Fatalf("WriteFile(%s): %v", payloadPath, err)
	}
}

func requireEntry(t *testing.T, report *models.DiskUsageReport, kind, id string, reclaimable bool) {
	t.Helper()
	entry := findEntry(t, report, kind, id)
	if entry.Reclaimable != reclaimable {
		t.Fatalf("%s/%s reclaimable = %v, want %v: %+v", kind, id, entry.Reclaimable, reclaimable, entry)
	}
}

func findEntry(t *testing.T, report *models.DiskUsageReport, kind, id string) models.DiskUsageEntry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Kind == kind && entry.ID == id {
			return entry
		}
	}
	t.Fatalf("missing %s/%s in report: %+v", kind, id, report.Entries)
	return models.DiskUsageEntry{}
}

func findOptionalEntry(report *models.DiskUsageReport, kind, id string) *models.DiskUsageEntry {
	for _, entry := range report.Entries {
		if entry.Kind == kind && entry.ID == id {
			copy := entry
			return &copy
		}
	}
	return nil
}

func hasDiskWarning(warnings []models.DiskUsageWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func requireRemoved(t *testing.T, report *models.CleanupReport, kind, id string) {
	t.Helper()
	for _, entry := range report.Removed {
		if entry.Kind == kind && entry.ID == id {
			return
		}
	}
	t.Fatalf("missing removed %s/%s in cleanup report: %+v", kind, id, report.Removed)
}
