package disk

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

const staleScratchAge = time.Hour

var (
	diskLowAvailableWarningBytes       uint64 = 8 * 1024 * 1024 * 1024
	diskLargeReclaimableWarningBytes   uint64 = 32 * 1024 * 1024 * 1024
	diskHighAllocatedWarningPercentage uint64 = 85
	diskProfileImageCacheMaxBytes      uint64 = 8 * 1024 * 1024 * 1024
)

type Service struct {
	db       *store.DB
	dataDir  string
	buildDir string
	tempDir  string
	now      func() time.Time

	profileImageCacheMaxBytes uint64
}

type Option func(*Service)

func WithProfileImageCacheMaxBytes(maxBytes uint64) Option {
	return func(s *Service) {
		s.profileImageCacheMaxBytes = maxBytes
	}
}

func NewService(db *store.DB, dataDir, buildDir string, options ...Option) *Service {
	if buildDir == "" {
		buildDir = dataDir
	}
	service := &Service{
		db:       db,
		dataDir:  dataDir,
		buildDir: buildDir,
		tempDir:  os.TempDir(),
		now:      time.Now,

		profileImageCacheMaxBytes: diskProfileImageCacheMaxBytes,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Usage() (*models.DiskUsageReport, error) {
	report := &models.DiskUsageReport{
		DataDir:  s.dataDir,
		BuildDir: s.buildDir,
	}
	entries, err := s.collectEntries()
	if err != nil {
		return nil, err
	}
	report.Entries = entries
	for _, entry := range entries {
		report.Totals.ApparentBytes += entry.ApparentBytes
		report.Totals.AllocatedBytes += entry.AllocatedBytes
		report.Totals.ReclaimableBytes += entry.ReclaimableBytes
	}
	report.Warnings = s.diskUsageWarnings(report)
	return report, nil
}

func (s *Service) Cleanup(dryRun bool) (*models.CleanupReport, error) {
	usage, err := s.Usage()
	if err != nil {
		return nil, err
	}
	result := &models.CleanupReport{
		DryRun: dryRun,
		Usage:  *usage,
	}
	for _, entry := range usage.Entries {
		if !entry.Reclaimable {
			continue
		}
		if dryRun {
			result.Removed = append(result.Removed, entry)
			continue
		}
		if err := s.removeReclaimableEntry(entry); err != nil {
			entry.Reason = entry.Reason + ": " + err.Error()
			result.Failed = append(result.Failed, entry)
			continue
		}
		result.Removed = append(result.Removed, entry)
	}
	if !dryRun {
		nextUsage, err := s.Usage()
		if err != nil {
			return nil, err
		}
		result.Usage = *nextUsage
	}
	return result, nil
}

func (s *Service) removeReclaimableEntry(entry models.DiskUsageEntry) error {
	if !entry.Reclaimable {
		return fmt.Errorf("refuse to remove non-reclaimable disk usage entry %s", entry.Path)
	}
	if entry.Kind == "vm-profile-image" {
		return s.removeProfileImageCacheEntry(entry)
	}
	if err := os.RemoveAll(entry.Path); err != nil {
		return fmt.Errorf("remove disk usage entry %s: %w", entry.Path, err)
	}
	return nil
}

func (s *Service) removeProfileImageCacheEntry(entry models.DiskUsageEntry) error {
	record, err := s.db.GetVMProfileImage(entry.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get vm profile image: %w", err)
	}
	count, err := s.db.CountVMsByImageID(record.ImageID)
	if err != nil {
		return fmt.Errorf("count vms using profile image: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("profile image is in use by %d vm(s)", count)
	}
	image, err := s.db.GetImage(record.ImageID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("get profile backing image: %w", err)
	}
	imageDir := entry.Path
	if err == nil && image.RootfsPath != "" {
		imageDir = filepath.Dir(image.RootfsPath)
	}
	if err := s.db.DeleteVMProfileImage(record.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("delete vm profile image record: %w", err)
	}
	if err := os.RemoveAll(imageDir); err != nil {
		return fmt.Errorf("remove backing image dir: %w", err)
	}
	if err := s.db.DeleteImage(record.ImageID); err != nil {
		return fmt.Errorf("delete backing image record: %w", err)
	}
	return nil
}

func (s *Service) collectEntries() ([]models.DiskUsageEntry, error) {
	images, err := s.db.ListImages()
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	machines, err := s.db.ListMachines()
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	profileImages, err := s.db.ListVMProfileImages()
	if err != nil {
		return nil, fmt.Errorf("list profile images: %w", err)
	}

	imageRecords := map[string]models.Image{}
	for _, image := range images {
		imageRecords[image.ID] = image
	}
	machineRecords := map[string]models.Machine{}
	vmImageUsers := map[string]int{}
	for _, machine := range machines {
		machineRecords[machine.ID] = machine
		vmImageUsers[machine.ImageID]++
	}
	profileImageEntries := classifyProfileImageCaches(profileImages, vmImageUsers)

	var entries []models.DiskUsageEntry
	imageEntries, err := s.imageEntries(imageRecords, vmImageUsers, profileImageEntries)
	if err != nil {
		return nil, err
	}
	entries = append(entries, imageEntries...)
	machineEntries, err := s.machineEntries(machineRecords)
	if err != nil {
		return nil, err
	}
	entries = append(entries, machineEntries...)
	scratchEntries, err := s.scratchEntries()
	if err != nil {
		return nil, err
	}
	entries = append(entries, scratchEntries...)
	tempEntries, err := s.listTempEntries()
	if err != nil {
		return nil, err
	}
	entries = append(entries, tempEntries...)
	windowsProfileCacheEntries, err := s.windowsProfileCacheEntries()
	if err != nil {
		return nil, err
	}
	entries = append(entries, windowsProfileCacheEntries...)
	return entries, nil
}

func (s *Service) diskUsageWarnings(report *models.DiskUsageReport) []models.DiskUsageWarning {
	if report == nil {
		return nil
	}
	warnings := []models.DiskUsageWarning{}
	for _, path := range uniqueNonEmptyPaths(report.DataDir, report.BuildDir, s.tempDir) {
		snap, err := hostfs.SnapshotFilesystem(path)
		if err != nil {
			continue
		}
		if snap.AvailableBytes < diskLowAvailableWarningBytes {
			warnings = append(warnings, models.DiskUsageWarning{
				Code:           "low_available_disk",
				Severity:       "warning",
				Path:           path,
				AvailableBytes: snap.AvailableBytes,
				TotalBytes:     snap.TotalBytes,
				Message:        "kiln-vm filesystem has low available space; run kiln-vm cleanup --dry-run and consider deleting unused VMs/images before large applies",
			})
		}
		if hostfs.UsageAtLeastPercent(snap, diskHighAllocatedWarningPercentage) {
			warnings = append(warnings, models.DiskUsageWarning{
				Code:           "high_filesystem_usage",
				Severity:       "warning",
				Path:           path,
				AvailableBytes: snap.AvailableBytes,
				TotalBytes:     snap.TotalBytes,
				Message:        "kiln-vm filesystem usage is high; cleanup should be reviewed before provisioning more capacity",
			})
		}
	}
	if report.Totals.ReclaimableBytes >= diskLargeReclaimableWarningBytes {
		warnings = append(warnings, models.DiskUsageWarning{
			Code:             "large_reclaimable_data",
			Severity:         "warning",
			ReclaimableBytes: report.Totals.ReclaimableBytes,
			AllocatedBytes:   report.Totals.AllocatedBytes,
			Message:          "kiln-vm has a large amount of safely reclaimable data; run kiln-vm cleanup --dry-run to inspect it",
		})
	}
	return warnings
}

func uniqueNonEmptyPaths(paths ...string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	return result
}

type profileImageEntry struct {
	record      models.VMProfileImage
	state       string
	reclaimable bool
	reason      string
}

func (s *Service) imageEntries(records map[string]models.Image, vmImageUsers map[string]int, profileImages map[string]profileImageEntry) ([]models.DiskUsageEntry, error) {
	root := filepath.Join(s.dataDir, "images")
	entries, err := directoryEntries(root)
	if err != nil {
		return nil, err
	}
	result := make([]models.DiskUsageEntry, 0, len(entries))
	for _, dir := range entries {
		id := dir.Name()
		if id == "staging" {
			continue
		}
		path := filepath.Join(root, id)
		if profileImage, ok := profileImages[id]; ok {
			entry, err := usageEntry("vm-profile-image", profileImage.record.ID, profileImageCacheName(profileImage.record), path)
			if err != nil {
				return nil, err
			}
			entry.Referenced = true
			entry.State = profileImage.state
			entry.Reclaimable = profileImage.reclaimable
			entry.Reason = profileImage.reason
			if entry.Reclaimable {
				entry.ReclaimableBytes = entry.AllocatedBytes
			}
			result = append(result, entry)
			continue
		}
		record, referenced := records[id]
		entry, err := usageEntry("image", id, record.Name, path)
		if err != nil {
			return nil, err
		}
		entry.Referenced = referenced
		if referenced {
			if vmImageUsers[id] > 0 {
				entry.State = "in-use"
			} else {
				entry.State = "unused"
			}
		} else {
			entry.Reclaimable = true
			entry.Reason = "orphan-image-dir"
			entry.ReclaimableBytes = entry.AllocatedBytes
		}
		result = append(result, entry)
	}
	return s.applyProfileImageCacheBudget(result, profileImages), nil
}

func classifyProfileImageCaches(records []models.VMProfileImage, vmImageUsers map[string]int) map[string]profileImageEntry {
	result := map[string]profileImageEntry{}
	groups := map[string][]models.VMProfileImage{}
	for _, record := range records {
		key := fmt.Sprintf("%s/%d", record.Profile, record.RootfsSizeMB)
		groups[key] = append(groups[key], record)
	}

	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			if group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].ID > group[j].ID
			}
			return group[i].CreatedAt.After(group[j].CreatedAt)
		})
		for index, record := range group {
			entry := profileImageEntry{
				record: record,
				state:  "profile-cache",
				reason: "latest-vm-profile-image-cache",
			}
			if vmImageUsers[record.ImageID] > 0 {
				entry.state = "profile-cache-in-use"
				entry.reason = "active-vm-profile-image-cache"
			}
			if index > 0 {
				entry.state = "superseded"
				entry.reason = "superseded-vm-profile-image-cache"
				entry.reclaimable = true
				if vmImageUsers[record.ImageID] > 0 {
					entry.state = "superseded-in-use"
					entry.reason = "active-vm-profile-image-cache"
					entry.reclaimable = false
				}
			}
			result[record.ImageID] = entry
		}
	}
	return result
}

func (s *Service) applyProfileImageCacheBudget(entries []models.DiskUsageEntry, profileImages map[string]profileImageEntry) []models.DiskUsageEntry {
	if s == nil || s.profileImageCacheMaxBytes == 0 {
		return entries
	}
	byRecordID := map[string]profileImageEntry{}
	for _, profileImage := range profileImages {
		byRecordID[profileImage.record.ID] = profileImage
	}
	type candidate struct {
		index     int
		createdAt time.Time
	}
	var retainedBytes uint64
	candidates := []candidate{}
	for index, entry := range entries {
		if entry.Kind != "vm-profile-image" || entry.State != "profile-cache" || entry.Reclaimable {
			continue
		}
		retainedBytes += entry.AllocatedBytes
		profileImage, ok := byRecordID[entry.ID]
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{index: index, createdAt: profileImage.record.CreatedAt})
	}
	if retainedBytes <= s.profileImageCacheMaxBytes {
		return entries
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].createdAt.Equal(candidates[j].createdAt) {
			return entries[candidates[i].index].ID < entries[candidates[j].index].ID
		}
		return candidates[i].createdAt.Before(candidates[j].createdAt)
	})
	for _, candidate := range candidates {
		if retainedBytes <= s.profileImageCacheMaxBytes {
			break
		}
		entry := &entries[candidate.index]
		entry.State = "profile-cache-over-budget"
		entry.Reclaimable = true
		entry.Reason = "profile-image-cache-budget-exceeded"
		entry.ReclaimableBytes = entry.AllocatedBytes
		retainedBytes -= entry.AllocatedBytes
	}
	return entries
}

func profileImageCacheName(record models.VMProfileImage) string {
	return fmt.Sprintf("%s/%dMB", record.Profile, record.RootfsSizeMB)
}

func (s *Service) machineEntries(records map[string]models.Machine) ([]models.DiskUsageEntry, error) {
	root := filepath.Join(s.dataDir, "machines")
	entries, err := directoryEntries(root)
	if err != nil {
		return nil, err
	}
	result := make([]models.DiskUsageEntry, 0, len(entries))
	for _, dir := range entries {
		id := dir.Name()
		path := filepath.Join(root, id)
		record, referenced := records[id]
		entry, err := usageEntry("machine", id, record.Name, path)
		if err != nil {
			return nil, err
		}
		entry.Referenced = referenced
		if referenced {
			entry.State = string(record.State)
		} else {
			entry.Reclaimable = true
			entry.Reason = "orphan-machine-dir"
			entry.ReclaimableBytes = entry.AllocatedBytes
		}
		result = append(result, entry)
	}
	return result, nil
}

func (s *Service) scratchEntries() ([]models.DiskUsageEntry, error) {
	var entries []models.DiskUsageEntry
	for _, root := range []string{
		filepath.Join(s.buildDir, "images", "staging"),
		filepath.Join(s.buildDir, "contexts"),
	} {
		children, err := directoryEntries(root)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			path := filepath.Join(root, child.Name())
			entry, err := usageEntry("build-scratch", child.Name(), "", path)
			if err != nil {
				return nil, err
			}
			entry.Referenced = false
			if staleFileInfo(child, s.now()) {
				entry.Reclaimable = true
				entry.Reason = "stale-build-scratch"
				entry.ReclaimableBytes = entry.AllocatedBytes
			} else {
				entry.State = "recent"
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (s *Service) listTempEntries() ([]models.DiskUsageEntry, error) {
	root := s.tempDir
	children, err := directoryEntries(root)
	if err != nil {
		return nil, err
	}
	entries := []models.DiskUsageEntry{}
	for _, child := range children {
		name := child.Name()
		if !isTempName(name) {
			continue
		}
		path := filepath.Join(root, name)
		mountpoint, err := hostfs.IsMountPoint(path)
		if err != nil {
			return nil, fmt.Errorf("check kiln-vm temp mountpoint %s: %w", path, err)
		}
		if mountpoint {
			entries = append(entries, models.DiskUsageEntry{
				Kind:       "kiln-vm-temp",
				ID:         name,
				Name:       name,
				Path:       path,
				State:      "mounted",
				Referenced: true,
				Reason:     "active-rootfs-mountpoint",
			})
			continue
		}
		entry, err := usageEntry("kiln-vm-temp", name, name, path)
		if err != nil {
			entries = append(entries, models.DiskUsageEntry{
				Kind:       "kiln-vm-temp",
				ID:         name,
				Name:       name,
				Path:       path,
				State:      "inaccessible",
				Referenced: true,
				Reason:     err.Error(),
			})
			continue
		}
		entry.Referenced = false
		if staleFileInfo(child, s.now()) {
			entry.Reclaimable = true
			entry.Reason = "stale-kiln-vm-temp"
			entry.ReclaimableBytes = entry.AllocatedBytes
		} else {
			entry.State = "recent"
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func isTempName(name string) bool {
	return matchTempName(name, "kiln-vm-rootfs-*") ||
		matchTempName(name, "kiln-vm-profile-rootfs-*.ext4")
}

func matchTempName(name, pattern string) bool {
	matched, err := filepath.Match(pattern, name)
	return err == nil && matched
}

func (s *Service) windowsProfileCacheEntries() ([]models.DiskUsageEntry, error) {
	root := filepath.Join(s.dataDir, "windows-profiles", "cache")
	children, err := directoryEntries(root)
	if err != nil {
		return nil, err
	}
	result := make([]models.DiskUsageEntry, 0, len(children))
	for _, child := range children {
		path := filepath.Join(root, child.Name())
		entry, err := usageEntry("windows-profile-cache", child.Name(), child.Name(), path)
		if err != nil {
			return nil, err
		}
		entry.Referenced = false
		if filepath.Ext(child.Name()) == ".iso" {
			if staleFileInfo(child, s.now()) {
				entry.Reclaimable = true
				entry.Reason = "stale-windows-install-media"
				entry.ReclaimableBytes = entry.AllocatedBytes
			} else {
				entry.State = "recent"
			}
		} else {
			entry.State = "payload-cache"
		}
		result = append(result, entry)
	}
	return result, nil
}

func directoryEntries(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err == nil {
		return entries, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %w", root, err)
}

func usageEntry(kind, id, name, path string) (models.DiskUsageEntry, error) {
	apparent, allocated, err := dirUsage(path)
	if err != nil {
		return models.DiskUsageEntry{}, err
	}
	return models.DiskUsageEntry{
		Kind:           kind,
		ID:             id,
		Name:           name,
		Path:           path,
		ApparentBytes:  apparent,
		AllocatedBytes: allocated,
	}, nil
}

func dirUsage(root string) (uint64, uint64, error) {
	var apparent uint64
	var allocated uint64
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk disk usage: %w", walkErr)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat disk usage entry: %w", err)
		}
		apparent += uint64(info.Size())
		blocks, err := hostfs.AllocatedBytes(path)
		if err == nil {
			allocated += blocks
		}
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("measure disk usage for %s: %w", root, err)
	}
	return apparent, allocated, nil
}

func staleFileInfo(entry os.DirEntry, now time.Time) bool {
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return staleFile(info, now)
}

func staleFile(info os.FileInfo, now time.Time) bool {
	return info != nil && now.Sub(info.ModTime()) >= staleScratchAge
}
