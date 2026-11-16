package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestEnsureProfileImagePullsTrustedCatalogImage(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	rootfs := make([]byte, 128*1024*1024)
	copy(rootfs[32*1024*1024:], []byte("catalog-rootfs"))
	rootfsDigest := "sha256:" + testSHA256(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + testSHA256(layer)
	manifest := map[string]any{
		"schemaVersion": 2,
		"config": map[string]any{
			"mediaType": "application/vnd.kiln-vm.vm-profile.config.v1+json",
			"digest":    "sha256:" + strings.Repeat("0", 64),
			"size":      2,
		},
		"layers": []map[string]any{{
			"mediaType": image.OCIRootfsGzipMediaType,
			"digest":    layerDigest,
			"size":      len(layer),
			"annotations": map[string]string{
				"io.kiln-vm.rootfs.sha256": rootfsDigest,
			},
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := "sha256:" + testSHA256(manifestBytes)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/kiln-vm/profile/alpine/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
		case "/v2/kiln-vm/profile/alpine/blobs/" + layerDigest:
			w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      strings.TrimPrefix(server.URL, "https://") + "/kiln-vm/profile/alpine@" + manifestDigest,
		LayerDigest:    layerDigest,
		RootfsSHA256:   rootfsDigest,
		LayerMediaType: image.OCIRootfsGzipMediaType,
	}}}
	svc := NewService(db, imageService, &readinessMachineService{})
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if record.Profile != profile.Name || record.RootfsSizeMB != 128 || record.Fingerprint != fingerprint {
		t.Fatalf("profile image record = %+v", record)
	}
	img, err := imageService.Get(record.ImageID)
	if err != nil {
		t.Fatalf("Get imported image: %v", err)
	}
	info, err := os.Stat(img.RootfsPath)
	if err != nil {
		t.Fatalf("stat imported rootfs: %v", err)
	}
	if info.Size() != int64(128*1024*1024) {
		t.Fatalf("imported rootfs size = %d", info.Size())
	}
	data := make([]byte, len("catalog-rootfs"))
	file, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open imported rootfs: %v", err)
	}
	if _, err := file.ReadAt(data, 32*1024*1024); err != nil {
		file.Close()
		t.Fatalf("read marker: %v", err)
	}
	_ = file.Close()
	if string(data) != "catalog-rootfs" {
		t.Fatalf("marker = %q", data)
	}
	if strings.Contains(img.RootfsPath, filepath.Base(dataDir)+"/build") {
		t.Fatalf("imported image path still points at staging: %s", img.RootfsPath)
	}
}

func TestWindowsProfileRequiresExplicitPreparation(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	svc := NewService(db, imageService, &readinessMachineService{})

	_, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile: "windows11",
	})
	if err == nil {
		t.Fatal("EnsureProfileImage(windows11) succeeded, want preparation error")
	}
	if !strings.Contains(err.Error(), "kiln-vm profiles prepare windows11") {
		t.Fatalf("EnsureProfileImage(windows11) error = %v", err)
	}
}

func TestWindowsProfileFingerprintDoesNotDependOnTheRuntimeFingerprint(t *testing.T) {
	profile, err := GetProfile("windows11")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	left := profileFingerprint(profile, "runtime-a")
	right := profileFingerprint(profile, "runtime-b")
	if left != right {
		t.Fatalf("windows profile fingerprint changed with runtime: %s != %s", left, right)
	}
}

func TestProfileFingerprintIncludesPreparationStrategyVersion(t *testing.T) {
	base := windowsLibvirtProfileForTest(t)
	changed := base
	changed.Preparation.Version = base.Preparation.Version + 1

	if profileFingerprint(base, "runtime") == profileFingerprint(changed, "runtime") {
		t.Fatal("profile fingerprint ignored preparation strategy version")
	}

	changed = base
	changed.Preparation.RequiredInputs = append([]string(nil), base.Preparation.RequiredInputs...)
	changed.Preparation.RequiredInputs = append(changed.Preparation.RequiredInputs, "edition")
	if profileFingerprint(base, "runtime") == profileFingerprint(changed, "runtime") {
		t.Fatal("profile fingerprint ignored preparation input schema")
	}
}

func TestPrepareProfileRejectsWrongStrategy(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := image.NewService(db, t.TempDir(), "")
	svc := NewService(db, imageService, &readinessMachineService{})

	_, err := svc.PrepareProfile(context.Background(), "windows11", &models.PrepareVMProfileRequest{
		Strategy: string(models.VMProfilePreparationDockerRootfs),
	})

	assertValidationError(t, err, "strategy", `profile "windows11" uses preparation strategy "windows-unattended"`)
}

func TestEnsureProfileImageReusesPreparedWindowsImageAcrossFingerprintChange(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	svc := NewService(db, imageService, &readinessMachineService{})
	profile, err := GetProfile("windows11")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}

	imageDir := filepath.Join(dataDir, "images", "windows-image")
	if err := os.MkdirAll(imageDir, 0750); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	diskPath := filepath.Join(imageDir, "disk.qcow2")
	if err := os.WriteFile(diskPath, []byte("prepared-windows-qcow2"), 0600); err != nil {
		t.Fatalf("write prepared disk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "id_ed25519"), []byte("private-key"), 0600); err != nil {
		t.Fatalf("write prepared key: %v", err)
	}
	img := &models.Image{
		ID:         "windows-image",
		Name:       "vm-profile-windows11",
		Tag:        "v1-65536mb",
		BootMode:   image.BootModeBootableQCOW2,
		SizeMB:     profile.DefaultDiskSizeMB,
		RootfsPath: diskPath,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.CreateImage(img); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	old := &models.VMProfileImage{
		ID:           "old-windows-cache",
		Profile:      profile.Name,
		ImageID:      img.ID,
		Fingerprint:  "old-kiln-vm-runtime-bound-fingerprint",
		RootfsSizeMB: profile.DefaultDiskSizeMB,
		CreatedAt:    time.Now().UTC().Add(-time.Hour),
	}
	if err := db.CreateVMProfileImage(old); err != nil {
		t.Fatalf("CreateVMProfileImage(old): %v", err)
	}

	got, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile: profile.Name,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage(windows11): %v", err)
	}
	wantFingerprint := profileFingerprint(profile, imageService.RuntimeFingerprint())
	if got.ImageID != img.ID || got.Fingerprint != wantFingerprint || got.ID == old.ID {
		t.Fatalf("migrated profile image = %+v, want image=%s fingerprint=%s new id", got, img.ID, wantFingerprint)
	}
	if _, err := db.GetVMProfileImage(old.ID); err == nil {
		t.Fatalf("old profile image cache entry still exists")
	}
	if _, err := db.GetVMProfileImage(got.ID); err != nil {
		t.Fatalf("new profile image cache entry missing: %v", err)
	}
}

func TestProfilesReportWindowsProfileNeedsPreparation(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	svc := NewService(db, imageService, &readinessMachineService{})
	svc.SetProfileInfrastructureConfig("", "qemu:///system", "windows-net", "10.232.0.0/24")

	profiles := svc.Profiles()
	var windows models.VMProfile
	for _, profile := range profiles {
		if profile.Name == "windows11" {
			windows = profile
			break
		}
	}
	if windows.Name == "" {
		t.Fatal("windows11 profile is missing")
	}
	if windows.Backend != "libvirt-qemu" || windows.GuestOS != "windows" || !windows.RequiresPreparation {
		t.Fatalf("windows11 profile = %+v", windows)
	}
	if windows.Preparation.Kind != models.VMProfilePreparationWindowsUnattended ||
		windows.Preparation.Version != 1 ||
		windows.Preparation.ImageBootMode != image.BootModeBootableQCOW2 {
		t.Fatalf("windows11 preparation = %+v", windows.Preparation)
	}
	if windows.Libvirt == nil ||
		windows.Libvirt.OSInfo != "win11" ||
		windows.Libvirt.Firmware != "uefi" ||
		!windows.Libvirt.SecureBoot ||
		!windows.Libvirt.SMM ||
		windows.Libvirt.TPM == nil ||
		windows.Libvirt.Disk.Bus != "virtio" ||
		windows.Libvirt.Network.Source != "windows-net" ||
		windows.Libvirt.Network.Model != "virtio" ||
		windows.Libvirt.Graphics.Type != "spice" {
		t.Fatalf("windows11 libvirt spec = %+v", windows.Libvirt)
	}
	if windows.Networking.Public.Kind != models.VMProfilePublicNetworkLibvirtNAT ||
		windows.Networking.Public.Source != "windows-net" {
		t.Fatalf("windows11 networking = %+v, want libvirt public NAT", windows.Networking)
	}
	if windows.Access.Kind != models.VMProfileAccessSSH || windows.Access.User != "kiln-vm" {
		t.Fatalf("windows11 access = %+v", windows.Access)
	}
	if windows.Readiness.Kind != models.VMProfileReadinessSSHExec || windows.Readiness.ExpectedStdoutSubstring != "kiln-vm-ready" {
		t.Fatalf("windows11 readiness = %+v", windows.Readiness)
	}
	if windows.Prepared {
		t.Fatalf("windows11 profile unexpectedly prepared without profile image")
	}
}

func TestProfilesReportDockerCapability(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := image.NewService(db, t.TempDir(), "")
	svc := NewService(db, imageService, &readinessMachineService{})

	profiles := svc.Profiles()
	var ubuntuDocker models.VMProfile
	for _, profile := range profiles {
		if profile.Name == "ubuntu-docker" {
			ubuntuDocker = profile
			break
		}
	}
	if ubuntuDocker.Name == "" {
		t.Fatal("ubuntu-docker profile is missing")
	}
	if len(ubuntuDocker.Capabilities) != 1 || ubuntuDocker.Capabilities[0] != "docker" {
		t.Fatalf("ubuntu-docker capabilities = %#v, want [docker]", ubuntuDocker.Capabilities)
	}
	if ubuntuDocker.Access.Kind != models.VMProfileAccessGuestAgent {
		t.Fatalf("ubuntu-docker access = %+v, want guest-agent", ubuntuDocker.Access)
	}
	if ubuntuDocker.Readiness.Kind != models.VMProfileReadinessGuestAgentExec {
		t.Fatalf("ubuntu-docker readiness = %+v, want guest-agent-exec", ubuntuDocker.Readiness)
	}
	if ubuntuDocker.Networking.Public.Kind != models.VMProfilePublicNetworkFirecrackerNAT {
		t.Fatalf("ubuntu-docker networking = %+v, want Firecracker public NAT", ubuntuDocker.Networking)
	}
}

func TestCreateVMRejectsDiskAboveConfiguredCap(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)
	svc.SetMaxDiskSizeMB(1024)

	_, err := svc.Create(context.Background(), &models.CreateVMRequest{
		Name:       "too-large",
		Profile:    "alpine",
		DiskSizeMB: 1025,
	})

	assertValidationError(t, err, "disk_size_mb", "must be between 128 and 1024")
}

func TestCreateVMRejectsDefaultProfileDiskAboveConfiguredCap(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)
	svc.SetMaxDiskSizeMB(1024)

	_, err := svc.Create(context.Background(), &models.CreateVMRequest{
		Name:    "default-too-large",
		Profile: "ubuntu",
	})

	assertValidationError(t, err, "disk_size_mb", "must be between 128 and 1024")
}

func TestPlanCreateUsesProfileDefaults(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)
	profile, err := getProfile("ubuntu-docker")
	if err != nil {
		t.Fatalf("getProfile: %v", err)
	}

	plan, err := svc.PlanCreate(&models.CreateVMRequest{
		Name:    "builder",
		Profile: "ubuntu-docker",
	})
	if err != nil {
		t.Fatalf("PlanCreate: %v", err)
	}
	if plan.Name != "builder" || plan.Profile != "ubuntu-docker" {
		t.Fatalf("plan identity = %+v", plan)
	}
	if plan.CPU.Kind != "shared" || plan.CPU.VCPUs != profile.DefaultVCPUs || plan.MemoryMB != profile.DefaultMemoryMB || plan.DiskSizeMB != profile.DefaultDiskSizeMB {
		t.Fatalf("plan shape = cpu %+v memory %d disk %d", plan.CPU, plan.MemoryMB, plan.DiskSizeMB)
	}
	if len(plan.ProfileCapabilities) != 1 || plan.ProfileCapabilities[0] != "docker" {
		t.Fatalf("plan capabilities = %+v, want docker from profile", plan.ProfileCapabilities)
	}
}

func TestPlanCreateAppliesExplicitOverrides(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)

	plan, err := svc.PlanCreate(&models.CreateVMRequest{
		Name:       "small-worker",
		Profile:    "alpine",
		CPU:        &models.CPUConfig{Kind: "dedicated", VCPUs: 1},
		MemoryMB:   1024,
		DiskSizeMB: 2048,
		Power:      &models.VMPowerPolicy{Mode: models.VMPowerModeOnDemand, IdleTimeoutSeconds: 30},
	})
	if err != nil {
		t.Fatalf("PlanCreate: %v", err)
	}
	if plan.Profile != "alpine" || len(plan.ProfileCapabilities) != 0 {
		t.Fatalf("plan profile/capabilities = %+v", plan)
	}
	if plan.CPU.Kind != "dedicated" || plan.CPU.VCPUs != 1 || plan.MemoryMB != 1024 || plan.DiskSizeMB != 2048 {
		t.Fatalf("plan overrides = %+v", plan)
	}
	if plan.Power.Mode != models.VMPowerModeOnDemand || plan.Power.IdleTimeoutSeconds != 30 || plan.Power.StartsWithDaemon() {
		t.Fatalf("plan power override = %+v", plan.Power)
	}
}

func TestCreateVMPassesPlanToMachineAndSeedsProfileCapabilities(t *testing.T) {
	db := openVMServiceTestDB(t)
	now := time.Now().UTC()
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("role-rootfs"), 0600); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	profile, err := getProfile("ubuntu-docker")
	if err != nil {
		t.Fatalf("getProfile: %v", err)
	}
	imageRecord := &models.Image{
		ID:         "image-role-ubuntu-docker",
		Name:       "ubuntu-docker",
		Tag:        "role",
		RootfsPath: rootfs,
		CreatedAt:  now,
	}
	if err := db.CreateImage(imageRecord); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateVMProfileImage(&models.VMProfileImage{
		ID:           "profile-role-ubuntu-docker",
		Profile:      profile.Name,
		ImageID:      imageRecord.ID,
		Fingerprint:  profileFingerprint(profile, "role-runtime"),
		RootfsSizeMB: 262144,
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateVMProfileImage: %v", err)
	}
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "role-runtime",
		getImage:           imageRecord,
	}
	machines := &roleCreateMachineService{db: db}
	svc := NewService(db, imageService, machines)

	vm, err := svc.Create(context.Background(), &models.CreateVMRequest{
		Name:       "runner",
		Profile:    "ubuntu-docker",
		CPU:        &models.CPUConfig{Kind: "shared", VCPUs: 4},
		MemoryMB:   8192,
		DiskSizeMB: 262144,
		Power:      &models.VMPowerPolicy{Mode: models.VMPowerModeAlways},
	})
	if err != nil {
		t.Fatalf("Create VM: %v", err)
	}

	if vm.Name != "runner" || vm.Profile != "ubuntu-docker" || vm.DiskSizeMB != 262144 {
		t.Fatalf("VM = %+v", vm)
	}
	if vm.Power.Policy.Mode != models.VMPowerModeAlways {
		t.Fatalf("VM power = %+v", vm.Power.Policy)
	}
	if machines.createReq == nil {
		t.Fatal("machine create was not called")
	}
	if machines.createReq.Name != "runner" || machines.createReq.ImageID != imageRecord.ID || machines.createReq.MemoryMB != 8192 {
		t.Fatalf("machine request = %+v", machines.createReq)
	}
	if machines.createReq.CPU == nil || machines.createReq.CPU.Kind != "shared" || machines.createReq.CPU.VCPUs != 4 {
		t.Fatalf("machine CPU = %+v", machines.createReq.CPU)
	}
	if len(vm.Capabilities) != 1 || vm.Capabilities[0].Capability != "docker" || vm.Capabilities[0].Status != models.VMCapabilityStatusPending {
		t.Fatalf("VM capabilities = %+v, want pending docker from profile", vm.Capabilities)
	}
}

func TestCreateVMPassesProfileLibvirtDomainSpecToMachine(t *testing.T) {
	profile := windowsLibvirtProfileForTest(t)
	withTestProfile(t, profile)
	db := openVMServiceTestDB(t)
	now := time.Now().UTC()
	disk := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := createSparseTestFile(disk, profile.DefaultDiskSizeMB); err != nil {
		t.Fatalf("create disk: %v", err)
	}
	imageRecord := &models.Image{
		ID:         "image-linux-import-qcow2",
		Name:       profile.Name,
		Tag:        "prepared",
		BootMode:   image.BootModeBootableQCOW2,
		SizeMB:     profile.DefaultDiskSizeMB,
		RootfsPath: disk,
		CreatedAt:  now,
	}
	if err := db.CreateImage(imageRecord); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateVMProfileImage(&models.VMProfileImage{
		ID:           "profile-linux-import-qcow2",
		Profile:      profile.Name,
		ImageID:      imageRecord.ID,
		Fingerprint:  profileFingerprint(profile, "libvirt-runtime"),
		RootfsSizeMB: profile.DefaultDiskSizeMB,
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateVMProfileImage: %v", err)
	}
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "libvirt-runtime",
		getImage:           imageRecord,
	}
	machines := &roleCreateMachineService{db: db}
	svc := NewService(db, imageService, machines)
	svc.SetProfileInfrastructureConfig("", "qemu:///system", "kiln-vm-test-net", "")

	vm, err := svc.Create(context.Background(), &models.CreateVMRequest{
		Name:    "windows-vm",
		Profile: profile.Name,
	})
	if err != nil {
		t.Fatalf("Create libvirt Windows VM: %v", err)
	}
	if vm.Backend != "libvirt-qemu" || vm.GuestOS != "windows" {
		t.Fatalf("VM backend/guest = %s/%s", vm.Backend, vm.GuestOS)
	}
	if machines.createReq == nil {
		t.Fatal("machine create was not called")
	}
	if machines.createReq.Libvirt == nil {
		t.Fatal("machine create did not receive libvirt spec")
	}
	if machines.createReq.Libvirt.OSInfo != "win11" ||
		machines.createReq.Libvirt.Firmware != "uefi" ||
		machines.createReq.Libvirt.Disk.Bus != "virtio" ||
		machines.createReq.Libvirt.Network.Source != "kiln-vm-test-net" ||
		machines.createReq.Libvirt.Network.Model != "virtio" ||
		machines.createReq.Libvirt.Graphics.Type != "spice" {
		t.Fatalf("machine create libvirt spec = %+v", machines.createReq.Libvirt)
	}
}

func TestEnsureProfileImageRejectsRootfsAboveConfiguredCap(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)
	svc.SetMaxDiskSizeMB(64 * 1024)

	_, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      "alpine",
		RootfsSizeMB: 64*1024 + 1,
	})

	assertValidationError(t, err, "rootfs_size_mb", "must be between 128 and 65536")
}

func TestPrepareProfileRejectsPreparedDiskAboveConfiguredCap(t *testing.T) {
	db := openVMServiceTestDB(t)
	svc := NewService(db, nil, nil)
	svc.SetMaxDiskSizeMB(1024)

	_, err := svc.PrepareProfile(context.Background(), "windows11", &models.PrepareVMProfileRequest{
		MediaPath: "/tmp/windows.iso",
	})

	assertValidationError(t, err, "disk_size_mb", "must be between 128 and 1024")
}

func TestPreflightProfileImageShapeRejectsBelowTrustedFloorInPullMode(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{runtimeFingerprint: "runtime"}
	svc := NewService(db, imageService, nil)
	profile, err := getProfile("ubuntu-docker")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	fingerprint := profileFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        "ubuntu-docker",
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   4096,
		Reference:      "https://example.invalid/rootfs.ext4.gz",
		LayerDigest:    "sha256:layer",
		RootfsSHA256:   "sha256:rootfs",
	}}}
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}

	status, err := svc.PreflightProfileImageShape("ubuntu-docker", 3072)
	if !errors.Is(err, apierr.ErrValidation) {
		t.Fatalf("PreflightProfileImageShape error = %v, want validation", err)
	}
	var shapeErr *ProfileImageShapeError
	if !errors.As(err, &shapeErr) {
		t.Fatalf("PreflightProfileImageShape error = %T, want ProfileImageShapeError", err)
	}
	if status == nil || status.MinimumTrustedRootfsSizeMB != 4096 || status.RequestedRootfsSizeMB != 3072 {
		t.Fatalf("status = %+v, want requested=3072 minimum=4096", status)
	}
	if shapeErr.Status.Profile != "ubuntu-docker" || shapeErr.Status.Arch != runtime.GOARCH {
		t.Fatalf("shape error status = %+v", shapeErr.Status)
	}
}

func TestPreflightProfileImageShapeAcceptsTrustedFloorInPullMode(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{runtimeFingerprint: "runtime"}
	svc := NewService(db, imageService, nil)
	profile, err := getProfile("ubuntu-docker")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	fingerprint := profileFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        "ubuntu-docker",
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   4096,
		Reference:      "https://example.invalid/rootfs.ext4.gz",
		LayerDigest:    "sha256:layer",
		RootfsSHA256:   "sha256:rootfs",
	}}}
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}

	status, err := svc.PreflightProfileImageShape("ubuntu-docker", 4096)
	if err != nil {
		t.Fatalf("PreflightProfileImageShape: %v", err)
	}
	if status.MatchedRootfsSizeMB != 4096 {
		t.Fatalf("matched rootfs = %d, want 4096", status.MatchedRootfsSizeMB)
	}
}

func TestPreflightProfileImageShapeAllowsAutoFallbackBelowTrustedFloor(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{runtimeFingerprint: "runtime"}
	svc := NewService(db, imageService, nil)
	profile, err := getProfile("ubuntu-docker")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        "ubuntu-docker",
		ProfileVersion: profile.Version,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   4096,
		Reference:      "https://example.invalid/rootfs.ext4.gz",
		LayerDigest:    "sha256:layer",
		RootfsSHA256:   "sha256:rootfs",
	}}}
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModeAuto}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}

	status, err := svc.PreflightProfileImageShape("ubuntu-docker", 3072)
	if err != nil {
		t.Fatalf("PreflightProfileImageShape: %v", err)
	}
	if status.MinimumTrustedRootfsSizeMB != 4096 || status.MatchedRootfsSizeMB != 0 {
		t.Fatalf("status = %+v, want auto fallback with minimum floor recorded", status)
	}
}

func TestEnsureProfileImagePullsTrustedHTTPArtifact(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	rootfs := make([]byte, 128*1024*1024)
	copy(rootfs[16*1024*1024:], []byte("http-catalog-rootfs"))
	rootfsDigest := "sha256:" + testSHA256(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + testSHA256(layer)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz",
		LayerDigest:    layerDigest,
		RootfsSHA256:   rootfsDigest,
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, &readinessMachineService{})
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	img, err := imageService.Get(record.ImageID)
	if err != nil {
		t.Fatalf("Get imported image: %v", err)
	}
	data := make([]byte, len("http-catalog-rootfs"))
	file, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open imported rootfs: %v", err)
	}
	if _, err := file.ReadAt(data, 16*1024*1024); err != nil {
		file.Close()
		t.Fatalf("read marker: %v", err)
	}
	_ = file.Close()
	if string(data) != "http-catalog-rootfs" {
		t.Fatalf("marker = %q", data)
	}
}

func TestEnsureProfileImageRetriesTrustedHTTPPullOverIPv4BeforeFallback(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	rootfs := make([]byte, 128*1024*1024)
	copy(rootfs[8*1024*1024:], []byte("ipv4-retry-rootfs"))
	rootfsDigest := "sha256:" + testSHA256(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + testSHA256(layer)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		if requests.Add(1) == 1 {
			_, _ = w.Write(stalledGzipPrefix())
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(layer)
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz",
		LayerDigest:    layerDigest,
		RootfsSHA256:   rootfsDigest,
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, &readinessMachineService{})
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())
	svc.SetProfileImagePullStallTimeout(50 * time.Millisecond)

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if requests.Load() < 2 {
		t.Fatalf("requests = %d, want stalled attempt plus IPv4 retry", requests.Load())
	}
	img, err := imageService.Get(record.ImageID)
	if err != nil {
		t.Fatalf("Get imported image: %v", err)
	}
	data := make([]byte, len("ipv4-retry-rootfs"))
	file, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open imported rootfs: %v", err)
	}
	if _, err := file.ReadAt(data, 8*1024*1024); err != nil {
		file.Close()
		t.Fatalf("read marker: %v", err)
	}
	_ = file.Close()
	if string(data) != "ipv4-retry-rootfs" {
		t.Fatalf("marker = %q", data)
	}
}

func TestEnsureProfileImageRetriesTrustedHTTPPullOverIPv4WhenResponseHeadersStall(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	rootfs := make([]byte, 128*1024*1024)
	copy(rootfs[8*1024*1024:], []byte("ipv4-header-retry-rootfs"))
	rootfsDigest := "sha256:" + testSHA256(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + testSHA256(layer)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz",
		LayerDigest:    layerDigest,
		RootfsSHA256:   rootfsDigest,
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, &readinessMachineService{})
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())
	svc.SetProfileImagePullHeaderTimeout(50 * time.Millisecond)

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if requests.Load() < 2 {
		t.Fatalf("requests = %d, want stalled response-header attempt plus IPv4 retry", requests.Load())
	}
	img, err := imageService.Get(record.ImageID)
	if err != nil {
		t.Fatalf("Get imported image: %v", err)
	}
	data := make([]byte, len("ipv4-header-retry-rootfs"))
	file, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open imported rootfs: %v", err)
	}
	if _, err := file.ReadAt(data, 8*1024*1024); err != nil {
		file.Close()
		t.Fatalf("read marker: %v", err)
	}
	_ = file.Close()
	if string(data) != "ipv4-header-retry-rootfs" {
		t.Fatalf("marker = %q", data)
	}
}

func TestEnsureProfileImageRetriesTrustedHTTPPullOverIPv4WhenConnectionClosesBeforeHeaders(t *testing.T) {
	db := openVMServiceTestDB(t)
	dataDir := t.TempDir()
	imageService := image.NewService(db, dataDir, "")
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	rootfs := make([]byte, 128*1024*1024)
	copy(rootfs[8*1024*1024:], []byte("ipv4-close-retry-rootfs"))
	rootfsDigest := "sha256:" + testSHA256(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + testSHA256(layer)
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer cannot hijack connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
	})
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = false
	server.StartTLS()
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/kiln-vm/v0.0.0/alpine.ext4.gz",
		LayerDigest:    layerDigest,
		RootfsSHA256:   rootfsDigest,
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, &readinessMachineService{})
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want closed attempt plus IPv4 retry", requests.Load())
	}
	img, err := imageService.Get(record.ImageID)
	if err != nil {
		t.Fatalf("Get imported image: %v", err)
	}
	data := make([]byte, len("ipv4-close-retry-rootfs"))
	file, err := os.Open(img.RootfsPath)
	if err != nil {
		t.Fatalf("open imported rootfs: %v", err)
	}
	if _, err := file.ReadAt(data, 8*1024*1024); err != nil {
		file.Close()
		t.Fatalf("read marker: %v", err)
	}
	_ = file.Close()
	if string(data) != "ipv4-close-retry-rootfs" {
		t.Fatalf("marker = %q", data)
	}
}

func TestShouldRetryProfileHTTPPullOverIPv4ClassifiesOnlyTransientTransportErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection reset", err: fmt.Errorf("pull profile: %w", syscall.ECONNRESET), want: true},
		{name: "connection aborted", err: fmt.Errorf("pull profile: %w", syscall.ECONNABORTED), want: true},
		{name: "broken pipe", err: fmt.Errorf("pull profile: %w", syscall.EPIPE), want: true},
		{name: "eof", err: fmt.Errorf("pull profile: %w", io.EOF), want: true},
		{name: "unexpected eof", err: fmt.Errorf("pull profile: %w", io.ErrUnexpectedEOF), want: true},
		{name: "content error", err: errors.New("compressed rootfs sha256 mismatch"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryProfileHTTPPullOverIPv4(ctx, tc.err); got != tc.want {
				t.Fatalf("shouldRetryProfileHTTPPullOverIPv4() = %v, want %v", got, tc.want)
			}
		})
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if shouldRetryProfileHTTPPullOverIPv4(cancelledCtx, fmt.Errorf("pull profile: %w", syscall.ECONNRESET)) {
		t.Fatal("shouldRetryProfileHTTPPullOverIPv4 retried after context cancellation")
	}
}

func TestEnsureProfileImageAutoFallsBackWhenTrustedCatalogPullFails(t *testing.T) {
	db := openVMServiceTestDB(t)
	fallbackImage := &models.Image{ID: "local-profile-image", Name: "profile-alpine", Tag: "fallback", SizeMB: 128, RootfsPath: "/tmp/local-profile-image", CreatedAt: time.Now().UTC()}
	if err := db.CreateImage(fallbackImage); err != nil {
		t.Fatalf("CreateImage(fallback): %v", err)
	}
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "runtime-fingerprint",
		buildImage:         fallbackImage,
	}
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      strings.TrimPrefix(server.URL, "https://") + "/kiln-vm/profile/alpine@sha256:" + strings.Repeat("1", 64),
		LayerDigest:    "sha256:" + strings.Repeat("2", 64),
		RootfsSHA256:   "sha256:" + strings.Repeat("3", 64),
		LayerMediaType: image.OCIRootfsGzipMediaType,
	}}}
	svc := NewService(db, imageService, nil)
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModeAuto}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if record.ImageID != "local-profile-image" {
		t.Fatalf("ImageID = %q, want fallback local build image", record.ImageID)
	}
	if imageService.buildCalls != 1 {
		t.Fatalf("Build calls = %d, want local build fallback", imageService.buildCalls)
	}
}

func TestEnsureProfileImagePullDoesNotFallbackWhenTrustedCatalogPullFails(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "runtime-fingerprint",
		buildImage:         &models.Image{ID: "local-profile-image"},
	}
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      strings.TrimPrefix(server.URL, "https://") + "/kiln-vm/profile/alpine@sha256:" + strings.Repeat("1", 64),
		LayerDigest:    "sha256:" + strings.Repeat("2", 64),
		RootfsSHA256:   "sha256:" + strings.Repeat("3", 64),
		LayerMediaType: image.OCIRootfsGzipMediaType,
	}}}
	svc := NewService(db, imageService, nil)
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())

	_, err = svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err == nil {
		t.Fatal("EnsureProfileImage succeeded, want trusted pull failure")
	}
	if imageService.buildCalls != 0 {
		t.Fatalf("Build calls = %d, want no local build fallback in pull mode", imageService.buildCalls)
	}
}

func TestEnsureProfileImageAutoFallsBackWhenTrustedHTTPPullStalls(t *testing.T) {
	db := openVMServiceTestDB(t)
	fallbackImage := &models.Image{ID: "local-profile-image", Name: "profile-alpine", Tag: "fallback", SizeMB: 128, RootfsPath: "/tmp/local-profile-image", CreatedAt: time.Now().UTC()}
	if err := db.CreateImage(fallbackImage); err != nil {
		t.Fatalf("CreateImage(fallback): %v", err)
	}
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "runtime-fingerprint",
		buildImage:         fallbackImage,
	}
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	layer := stalledGzipPrefix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/stalled.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/stalled.ext4.gz",
		LayerDigest:    "sha256:" + strings.Repeat("2", 64),
		RootfsSHA256:   "sha256:" + strings.Repeat("3", 64),
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, nil)
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModeAuto}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())
	svc.SetProfileImagePullStallTimeout(50 * time.Millisecond)

	record, err := svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err != nil {
		t.Fatalf("EnsureProfileImage: %v", err)
	}
	if record.ImageID != fallbackImage.ID {
		t.Fatalf("ImageID = %q, want fallback local build image", record.ImageID)
	}
	if imageService.buildCalls != 1 {
		t.Fatalf("Build calls = %d, want local build fallback", imageService.buildCalls)
	}
}

func TestEnsureProfileImagePullFailsWhenTrustedHTTPPullStalls(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "runtime-fingerprint",
		buildImage:         &models.Image{ID: "local-profile-image"},
	}
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	layer := stalledGzipPrefix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/stalled.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", image.OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/stalled.ext4.gz",
		LayerDigest:    "sha256:" + strings.Repeat("2", 64),
		RootfsSHA256:   "sha256:" + strings.Repeat("3", 64),
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, nil)
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModePull}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())
	svc.SetProfileImagePullStallTimeout(50 * time.Millisecond)

	_, err = svc.EnsureProfileImage(context.Background(), &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("EnsureProfileImage error = %v, want stalled pull failure", err)
	}
	if imageService.buildCalls != 0 {
		t.Fatalf("Build calls = %d, want no local build fallback in pull mode", imageService.buildCalls)
	}
}

func TestEnsureProfileImageAutoDoesNotFallbackAfterCallerCancellation(t *testing.T) {
	db := openVMServiceTestDB(t)
	imageService := &recordingProfileImageService{
		runtimeFingerprint: "runtime-fingerprint",
		buildImage:         &models.Image{ID: "local-profile-image"},
	}
	profile, err := GetProfile("alpine")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	fingerprint := CatalogFingerprint(profile, imageService.RuntimeFingerprint())
	catalog := &ProfileImageCatalog{SchemaVersion: 1, Images: []ProfileImageCatalogEntry{{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    fingerprint,
		Arch:           runtime.GOARCH,
		RootfsSizeMB:   128,
		Reference:      server.URL + "/profiles/stalled.ext4.gz",
		LayerDigest:    "sha256:" + strings.Repeat("2", 64),
		RootfsSHA256:   "sha256:" + strings.Repeat("3", 64),
		LayerMediaType: image.OCIRootfsGzipMediaType,
		Source:         "https",
	}}}
	svc := NewService(db, imageService, nil)
	if err := svc.SetProfileImageSource(ProfileImageSource{Mode: ProfileImageModeAuto}, catalog); err != nil {
		t.Fatalf("SetProfileImageSource: %v", err)
	}
	svc.SetProfileImageHTTPClient(server.Client())
	svc.SetProfileImagePullHeaderTimeout(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = svc.EnsureProfileImage(ctx, &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: 128,
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureProfileImage error = %v, want context deadline exceeded", err)
	}
	if imageService.buildCalls != 0 {
		t.Fatalf("Build calls = %d, want no local build fallback after caller cancellation", imageService.buildCalls)
	}
}

func stalledGzipPrefix() []byte {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("rootfs payload that will never fully arrive"))
	_ = zw.Close()
	layer := gz.Bytes()
	return layer[:10]
}

func testSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertValidationError(t *testing.T, err error, field, message string) {
	t.Helper()
	var validationErr *apierr.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want ValidationError", err, err)
	}
	if validationErr.Field != field || validationErr.Message != message {
		t.Fatalf("validation error = field %q message %q, want field %q message %q", validationErr.Field, validationErr.Message, field, message)
	}
}

func withTestProfile(t *testing.T, profile Profile) {
	t.Helper()
	previous, existed := profiles[profile.Name]
	profiles[profile.Name] = profile
	t.Cleanup(func() {
		if existed {
			profiles[profile.Name] = previous
		} else {
			delete(profiles, profile.Name)
		}
	})
}

func windowsLibvirtProfileForTest(t *testing.T) Profile {
	t.Helper()
	profile, err := GetProfile("windows11")
	if err != nil {
		t.Fatalf("GetProfile(windows11): %v", err)
	}
	profile.Name = "windows-libvirt-test"
	profile.Libvirt = libvirtdomain.Clone(profile.Libvirt)
	profile.DefaultDiskSizeMB = 1024
	return profile
}

func createSparseTestFile(path string, sizeMB int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create sparse test file: %w", err)
	}
	if err := file.Truncate(sizeMB * 1024 * 1024); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate sparse test file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close sparse test file: %w", err)
	}
	return nil
}

func TestWaitReadyFailsFastWhenMachineStopsBeforeGuestAgent(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateStopped,
			Error:    "",
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			IP:       "172.16.0.10",
			VSockCID: 50,
		},
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return nil, fmt.Errorf("machine is not running (state: stopped): %w", apierr.ErrConflict)
		},
	}
	svc := NewService(db, nil, machineService)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	err := svc.WaitReady(ctx, "ready")
	if err == nil {
		t.Fatal("WaitReady succeeded, want stopped-before-ready error")
	}
	if !errors.Is(err, ErrVMStoppedBeforeReady) {
		t.Fatalf("WaitReady error = %v, want ErrVMStoppedBeforeReady", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("WaitReady took %s, want fast failure after terminal VM state", elapsed)
	}
	if machineService.execCalls != 1 {
		t.Fatalf("exec calls = %d, want 1", machineService.execCalls)
	}
}

func TestWaitReadyIncludesMachineBootDiagnostics(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateError,
			Error:    "process_start: start firecracker: exit status 1",
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			IP:       "172.16.0.10",
			VSockCID: 50,
			Diagnostics: []models.Diagnostic{{
				Category:    "machine_boot",
				Phase:       "process_start",
				Resource:    "vm-ready-machine",
				Action:      "start",
				Backend:     "firecracker",
				MachineID:   "machine-vm-ready",
				KernelPath:  "/tmp/missing-kernel",
				KernelType:  "missing",
				ConfigPath:  "/var/lib/kiln-vm/machines/machine-vm-ready/firecracker.sock.json",
				LogPath:     "/var/lib/kiln-vm/machines/machine-vm-ready/console.log",
				Cleanup:     "released runtime network artifacts",
				Remediation: "check kernel",
				Error:       "start firecracker: exit status 1",
				StderrTail:  "firecracker failed\n",
			}},
		},
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return nil, fmt.Errorf("machine is not running (state: error): %w", apierr.ErrConflict)
		},
	}
	svc := NewService(db, nil, machineService)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := svc.WaitReady(ctx, "ready")
	if err == nil {
		t.Fatal("WaitReady succeeded, want machine diagnostics operation error")
	}
	if !errors.Is(err, ErrVMErrorBeforeReady) || !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("WaitReady error = %v, want VM error sentinel and conflict", err)
	}
	var operation *apierr.OperationError
	if !errors.As(err, &operation) {
		t.Fatalf("WaitReady error = %T %v, want OperationError", err, err)
	}
	if operation.Code != "vm_boot_failed" || operation.Phase != "process_start" || operation.Resource != "ready" || operation.Action != "start" {
		t.Fatalf("operation = %+v", operation)
	}
	if operation.Retryable {
		t.Fatalf("operation retryable = true, want false for non-retryable machine diagnostic")
	}
	if len(operation.Diagnostics) != 1 {
		t.Fatalf("operation diagnostics = %+v", operation.Diagnostics)
	}
	diagnostic := operation.Diagnostics[0]
	if diagnostic.KernelType != "missing" || diagnostic.KernelPath != "/tmp/missing-kernel" || diagnostic.LogPath == "" || diagnostic.Cleanup == "" {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
}

func TestWaitReadyContinuesWhileMachineIsRunning(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			IP:       "172.16.0.10",
			VSockCID: 50,
		},
	}
	machineService.execFunc = func(context.Context, string, string) (*execution.BufferedResult, error) {
		if machineService.execCalls < 3 {
			return nil, fmt.Errorf("guest agent unavailable: %w", apierr.ErrConflict)
		}
		return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
	}
	svc := NewService(db, nil, machineService)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.WaitReady(ctx, "ready"); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if machineService.execCalls != 3 {
		t.Fatalf("exec calls = %d, want 3", machineService.execCalls)
	}
}

func TestWaitReadyUsesSSHExecReadiness(t *testing.T) {
	profile := windowsLibvirtProfileForTest(t)
	profile.Name = "ssh-ready-test"
	withTestProfile(t, profile)
	db := openVMServiceTestDB(t)
	setReadyVMProfile(t, db, profile.Name)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			Backend:  "libvirt-qemu",
			GuestOS:  "windows",
			State:    models.MachineStateRunning,
			IP:       "172.16.0.10",
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 512,
		},
		execFunc: func(_ context.Context, _ string, command string) (*execution.BufferedResult, error) {
			if command != "echo kiln-vm-ready" {
				return &execution.BufferedResult{ExitCode: 127, Stderr: "unexpected command"}, nil
			}
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		},
	}
	svc := NewService(db, nil, machineService)

	if err := svc.WaitReady(context.Background(), "ready"); err != nil {
		t.Fatalf("WaitReady(ssh): %v", err)
	}
	if machineService.execCalls != 1 {
		t.Fatalf("exec calls = %d, want 1", machineService.execCalls)
	}
}

func TestDialForwardWorksOverSSHAccessAndValidatesThePort(t *testing.T) {
	profile := windowsLibvirtProfileForTest(t)
	profile.Name = "ssh-forward-test"
	withTestProfile(t, profile)
	db := openVMServiceTestDB(t)
	setReadyVMProfile(t, db, profile.Name)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "ready-machine",
			Backend:  "libvirt-qemu",
			GuestOS:  "windows",
			State:    models.MachineStateRunning,
			IP:       "172.16.0.10",
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 512,
		},
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		},
	}
	svc := NewService(db, nil, machineService)

	conn, err := svc.DialForward(context.Background(), "ready", 3389)
	if err != nil {
		t.Fatalf("DialForward(ssh): %v", err)
	}
	_ = conn.Close()
	if !slices.Equal(machineService.forwardPorts, []int{3389}) {
		t.Fatalf("machine forward ports = %v, want [3389]", machineService.forwardPorts)
	}
	for _, port := range []int{0, 65536} {
		_, err := svc.DialForward(context.Background(), "ready", port)
		if !errors.Is(err, apierr.ErrValidation) || !strings.Contains(err.Error(), "port must be between 1 and 65535") {
			t.Fatalf("DialForward(port %d) error = %v, want port validation", port, err)
		}
	}
}

func TestNormalizePowerPolicyDerivesModeBehavior(t *testing.T) {
	policy, err := NormalizePowerPolicy(nil)
	if err != nil {
		t.Fatalf("NormalizePowerPolicy(default): %v", err)
	}
	if policy.Mode != models.VMPowerModeManual || policy.WakesOnUse() || policy.StartsWithDaemon() {
		t.Fatalf("default policy = %+v, want manual with no automatic wake", policy)
	}

	policy, err = NormalizePowerPolicy(&models.VMPowerPolicy{Mode: models.VMPowerModeWarm})
	if err != nil {
		t.Fatalf("NormalizePowerPolicy: %v", err)
	}
	if policy.Mode != models.VMPowerModeWarm || !policy.StartsWithDaemon() || !policy.WakesOnUse() || policy.IdleTimeoutSeconds != 600 {
		t.Fatalf("warm policy = %+v, want daemon start, wake on use and default idle timeout", policy)
	}

	policy, err = NormalizePowerPolicy(&models.VMPowerPolicy{Mode: models.VMPowerModeOnDemand})
	if err != nil {
		t.Fatalf("NormalizePowerPolicy(on_demand): %v", err)
	}
	if policy.StartsWithDaemon() || !policy.WakesOnUse() || policy.IdleTimeoutSeconds != 600 {
		t.Fatalf("on_demand policy = %+v, want wake on use, no daemon start and default idle timeout", policy)
	}

	policy, err = NormalizePowerPolicy(&models.VMPowerPolicy{Mode: models.VMPowerModeAlways, IdleTimeoutSeconds: 20})
	if err != nil {
		t.Fatalf("NormalizePowerPolicy(always): %v", err)
	}
	if policy.IdleTimeoutSeconds != 0 || !policy.StartsWithDaemon() || !policy.WakesOnUse() {
		t.Fatalf("always policy = %+v, want no idle, daemon start and wake on use", policy)
	}
}

func TestUpdatePowerPolicyPersistsEditablePolicy(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &readinessMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
			IP:       "172.16.0.10",
			VSockCID: 50,
		},
	}
	svc := NewService(db, nil, machineService)

	vm, err := svc.UpdatePowerPolicy(context.Background(), "ready", models.VMPowerPolicy{
		Mode:               models.VMPowerModeOnDemand,
		IdleTimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("UpdatePowerPolicy: %v", err)
	}
	if vm.Power.Policy.Mode != models.VMPowerModeOnDemand ||
		vm.Power.Policy.IdleTimeoutSeconds != 45 {
		t.Fatalf("updated policy = %+v", vm.Power.Policy)
	}
}

func TestUpdatePowerPolicyStartsDaemonStartModes(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateStopped,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	vm, err := svc.UpdatePowerPolicy(context.Background(), "ready", models.VMPowerPolicy{
		Mode: models.VMPowerModeWarm,
	})
	if err != nil {
		t.Fatalf("UpdatePowerPolicy warm: %v", err)
	}
	if machineService.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", machineService.startCalls)
	}
	if vm.State != models.MachineStateRunning || vm.Power.Policy.Mode != models.VMPowerModeWarm {
		t.Fatalf("vm after warm policy = %+v", vm)
	}
}

func TestStopIdleVMsStopsOnlyIdleUnleasedVMs(t *testing.T) {
	db := openVMServiceTestDB(t)
	now := time.Now().UTC()
	if err := db.UpdateVMPowerPolicy("vm-ready", models.VMPowerPolicy{
		Mode:               models.VMPowerModeOnDemand,
		IdleTimeoutSeconds: 60,
	}); err != nil {
		t.Fatalf("UpdateVMPowerPolicy: %v", err)
	}
	if err := db.RecordVMPowerActivity("vm-ready", now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("RecordVMPowerActivity: %v", err)
	}
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	stopped, err := svc.StopIdleVMs(context.Background(), now)
	if err != nil {
		t.Fatalf("StopIdleVMs: %v", err)
	}
	if len(stopped) != 1 || stopped[0].State != models.MachineStateStopped {
		t.Fatalf("stopped = %+v, want one stopped VM", stopped)
	}
	if machineService.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", machineService.stopCalls)
	}
	refreshed, err := db.GetVM("ready")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if refreshed.Power.StopReason != "idle_timeout" || refreshed.Power.LastStoppedAt == nil {
		t.Fatalf("power stop state = %+v, want idle_timeout and timestamp", refreshed.Power)
	}
}

func TestStopIdleVMsSkipsActiveLease(t *testing.T) {
	db := openVMServiceTestDB(t)
	now := time.Now().UTC()
	if err := db.UpdateVMPowerPolicy("vm-ready", models.VMPowerPolicy{
		Mode:               models.VMPowerModeOnDemand,
		IdleTimeoutSeconds: 60,
	}); err != nil {
		t.Fatalf("UpdateVMPowerPolicy: %v", err)
	}
	if err := db.RecordVMPowerActivity("vm-ready", now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("RecordVMPowerActivity: %v", err)
	}
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)
	_, lease, err := svc.beginUse(context.Background(), "ready", "test")
	if err != nil {
		t.Fatalf("beginUse: %v", err)
	}
	defer lease.Release()
	if got := svc.activeLeaseCount("vm-ready"); got != 1 {
		t.Fatalf("active leases = %d, want 1", got)
	}
	if err := db.RecordVMPowerActivity("vm-ready", now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("RecordVMPowerActivity after lease: %v", err)
	}

	stopped, err := svc.StopIdleVMs(context.Background(), now)
	if err != nil {
		t.Fatalf("StopIdleVMs: %v", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("stopped = %+v, want none while leased", stopped)
	}
	if machineService.stopCalls != 0 {
		t.Fatalf("stop calls = %d, want 0", machineService.stopCalls)
	}
}

func TestBeginUseWakeRefreshesIdleActivityAndBlocksIdleStop(t *testing.T) {
	db := openVMServiceTestDB(t)
	now := time.Now().UTC()
	if err := db.UpdateVMPowerPolicy("vm-ready", models.VMPowerPolicy{
		Mode:               models.VMPowerModeOnDemand,
		IdleTimeoutSeconds: 60,
	}); err != nil {
		t.Fatalf("UpdateVMPowerPolicy: %v", err)
	}
	if err := db.RecordVMPowerActivity("vm-ready", now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("RecordVMPowerActivity: %v", err)
	}
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateStopped,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	_, lease, err := svc.beginUse(context.Background(), "ready", "test")
	if err != nil {
		t.Fatalf("beginUse: %v", err)
	}
	defer lease.Release()
	if machineService.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", machineService.startCalls)
	}
	refreshed, err := db.GetVM("ready")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if refreshed.Power.LastActivityAt == nil || refreshed.Power.LastActivityAt.Before(now.Add(-5*time.Second)) {
		t.Fatalf("last activity after wake = %+v, want refreshed", refreshed.Power.LastActivityAt)
	}

	stopped, err := svc.StopIdleVMs(context.Background(), now)
	if err != nil {
		t.Fatalf("StopIdleVMs: %v", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("stopped = %+v, want none while wake lease is active", stopped)
	}
	if machineService.stopCalls != 0 {
		t.Fatalf("stop calls = %d, want 0 while wake lease is active", machineService.stopCalls)
	}
}

func TestStopRejectsActiveUseLease(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	_, lease, err := svc.beginUse(context.Background(), "ready", "test")
	if err != nil {
		t.Fatalf("beginUse: %v", err)
	}
	err = svc.Stop("ready")
	if !errors.Is(err, ErrVMActiveUse) || !errors.Is(err, apierr.ErrConflict) || !strings.Contains(err.Error(), "active use leases") {
		t.Fatalf("Stop with active lease error = %v, want active lease conflict", err)
	}
	if machineService.stopCalls != 0 {
		t.Fatalf("stop calls = %d, want 0 while leased", machineService.stopCalls)
	}

	lease.Release()
	if err := svc.Stop("ready"); err != nil {
		t.Fatalf("Stop after release: %v", err)
	}
	if machineService.stopCalls != 1 {
		t.Fatalf("stop calls after release = %d, want 1", machineService.stopCalls)
	}
}

func TestManualPowerPolicyDoesNotWakeStoppedVM(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateStopped,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	_, lease, err := svc.beginUse(context.Background(), "ready", "test")
	if err == nil {
		lease.Release()
		t.Fatal("beginUse succeeded for stopped manual VM, want conflict")
	}
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("beginUse error = %v, want conflict", err)
	}
	if machineService.startCalls != 0 {
		t.Fatalf("start calls = %d, want 0 for manual stopped wake", machineService.startCalls)
	}
}

func TestManualPowerPolicyAllowsAlreadyRunningUse(t *testing.T) {
	db := openVMServiceTestDB(t)
	machineService := &powerMachineService{
		machine: &models.Machine{
			ID:       "machine-vm-ready",
			Name:     "vm-ready-machine",
			State:    models.MachineStateRunning,
			VCPUs:    1,
			CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
			MemoryMB: 256,
		},
	}
	svc := NewService(db, nil, machineService)

	_, lease, err := svc.beginUse(context.Background(), "ready", "test")
	if err != nil {
		t.Fatalf("beginUse running manual VM: %v", err)
	}
	lease.Release()
	if machineService.startCalls != 0 {
		t.Fatalf("start calls = %d, want 0 for already-running manual use", machineService.startCalls)
	}
}

func openVMServiceTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "image-vm-ready",
		Name:       "vm-ready-image",
		Tag:        "test",
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-vm-ready",
		Name:      "vm-ready-machine",
		ImageID:   "image-vm-ready",
		State:     models.MachineStateRunning,
		VCPUs:     1,
		CPU:       models.CPUConfig{Kind: "shared", VCPUs: 1},
		MemoryMB:  256,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-ready",
		Name:       "ready",
		Profile:    "alpine",
		MachineID:  "machine-vm-ready",
		ImageID:    "image-vm-ready",
		DiskSizeMB: 128,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	return db
}

func setReadyVMProfile(t *testing.T, db *store.DB, profile string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE vms SET profile = ? WHERE id = ?`, profile, "vm-ready"); err != nil {
		t.Fatalf("update ready VM profile: %v", err)
	}
}

type readinessMachineService struct {
	machine      *models.Machine
	execFunc     func(context.Context, string, string) (*execution.BufferedResult, error)
	execCalls    int
	forwardPorts []int
}

type roleCreateMachineService struct {
	db        *store.DB
	createReq *models.CreateMachineRequest
}

func (s *roleCreateMachineService) Create(_ context.Context, req *models.CreateMachineRequest) (*models.Machine, error) {
	clone := *req
	if req.CPU != nil {
		cpu := *req.CPU
		clone.CPU = &cpu
	}
	clone.Libvirt = libvirtdomain.Clone(req.Libvirt)
	s.createReq = &clone
	cpu := models.CPUConfig{Kind: "shared", VCPUs: 1}
	if req.CPU != nil {
		cpu = *req.CPU
	}
	record := &models.Machine{
		ID:        "machine-role-runner",
		Name:      req.Name,
		Backend:   req.Backend,
		GuestOS:   req.GuestOS,
		Libvirt:   libvirtdomain.Clone(req.Libvirt),
		ImageID:   req.ImageID,
		State:     models.MachineStateRunning,
		VCPUs:     cpu.VCPUs,
		CPU:       cpu,
		MemoryMB:  req.MemoryMB,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if s.db != nil {
		if err := s.db.CreateMachine(record); err != nil {
			return nil, fmt.Errorf("create role test machine: %w", err)
		}
	}
	return record, nil
}

func (s *roleCreateMachineService) Get(string) (*models.Machine, error) {
	panic("not used")
}

func (s *roleCreateMachineService) List() ([]models.Machine, error) {
	panic("not used")
}

func (s *roleCreateMachineService) Stop(string) error {
	panic("not used")
}

func (s *roleCreateMachineService) Start(context.Context, string) (*models.Machine, error) {
	panic("not used")
}

func (s *roleCreateMachineService) Delete(id string) error {
	if s.db == nil {
		return nil
	}
	if err := s.db.DeleteMachine(id); err != nil {
		return fmt.Errorf("delete role test machine %s: %w", id, err)
	}
	return nil
}

func (s *roleCreateMachineService) Exec(context.Context, string, string, io.Reader) (*execution.BufferedResult, error) {
	panic("not used")
}

func (s *roleCreateMachineService) Execute(context.Context, string, execution.Request) execution.Outcome {
	panic("not used")
}

func (s *roleCreateMachineService) Upload(context.Context, string, string, uint32, int64, io.Reader) error {
	panic("not used")
}

func (s *roleCreateMachineService) DialShell(context.Context, string) (io.ReadWriteCloser, error) {
	panic("not used")
}

func (s *roleCreateMachineService) RootDiskPath(string) (string, error) {
	return "", errors.New("root disk path should not be called")
}

func (s *roleCreateMachineService) DialForward(context.Context, string, int) (io.ReadWriteCloser, error) {
	panic("not used")
}

func (s *roleCreateMachineService) SSHKey(string) ([]byte, error) {
	panic("not used")
}

type powerMachineService struct {
	machine    *models.Machine
	stopCalls  int
	startCalls int
}

func (s *powerMachineService) Create(context.Context, *models.CreateMachineRequest) (*models.Machine, error) {
	panic("not used")
}

func (s *powerMachineService) Get(id string) (*models.Machine, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	clone := *s.machine
	return &clone, nil
}

func (s *powerMachineService) List() ([]models.Machine, error) {
	panic("not used")
}

func (s *powerMachineService) Stop(id string) error {
	if id != s.machine.ID {
		return &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	s.stopCalls++
	s.machine.State = models.MachineStateStopped
	return nil
}

func (s *powerMachineService) Start(context.Context, string) (*models.Machine, error) {
	s.startCalls++
	s.machine.State = models.MachineStateRunning
	clone := *s.machine
	return &clone, nil
}

func (s *powerMachineService) Delete(string) error {
	panic("not used")
}

func (s *powerMachineService) Exec(context.Context, string, string, io.Reader) (*execution.BufferedResult, error) {
	return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
}

func (s *powerMachineService) Execute(ctx context.Context, id string, request execution.Request) execution.Outcome {
	return executeTestAdapter(ctx, request, func(ctx context.Context, command string, stdin io.Reader) (*execution.BufferedResult, error) {
		return s.Exec(ctx, id, command, stdin)
	})
}

func (s *powerMachineService) Upload(context.Context, string, string, uint32, int64, io.Reader) error {
	panic("not used")
}

func (s *powerMachineService) DialShell(context.Context, string) (io.ReadWriteCloser, error) {
	panic("not used")
}

func (s *powerMachineService) RootDiskPath(string) (string, error) {
	return "", errors.New("root disk path should not be called")
}

func (s *powerMachineService) DialForward(context.Context, string, int) (io.ReadWriteCloser, error) {
	panic("not used")
}

func (s *powerMachineService) SSHKey(string) ([]byte, error) {
	panic("not used")
}

func (s *readinessMachineService) Create(context.Context, *models.CreateMachineRequest) (*models.Machine, error) {
	panic("not used")
}

func (s *readinessMachineService) Get(id string) (*models.Machine, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	clone := *s.machine
	return &clone, nil
}

func (s *readinessMachineService) List() ([]models.Machine, error) {
	panic("not used")
}

func (s *readinessMachineService) Stop(string) error {
	panic("not used")
}

func (s *readinessMachineService) Start(context.Context, string) (*models.Machine, error) {
	panic("not used")
}

func (s *readinessMachineService) Delete(string) error {
	panic("not used")
}

func (s *readinessMachineService) Exec(ctx context.Context, id, command string, _ io.Reader) (*execution.BufferedResult, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	s.execCalls++
	return s.execFunc(ctx, id, command)
}

func (s *readinessMachineService) Execute(ctx context.Context, id string, request execution.Request) execution.Outcome {
	return executeTestAdapter(ctx, request, func(ctx context.Context, command string, stdin io.Reader) (*execution.BufferedResult, error) {
		return s.Exec(ctx, id, command, stdin)
	})
}

func (s *readinessMachineService) Upload(context.Context, string, string, uint32, int64, io.Reader) error {
	panic("not used")
}

func (s *readinessMachineService) DialShell(context.Context, string) (io.ReadWriteCloser, error) {
	panic("not used")
}

func (s *readinessMachineService) RootDiskPath(string) (string, error) {
	return "", errors.New("root disk path should not be called")
}

func (s *readinessMachineService) DialForward(_ context.Context, _ string, port int) (io.ReadWriteCloser, error) {
	s.forwardPorts = append(s.forwardPorts, port)
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func (s *readinessMachineService) SSHKey(string) ([]byte, error) {
	panic("not used")
}

type recordingProfileImageService struct {
	runtimeFingerprint string
	buildCalls         int
	buildImage         *models.Image
	buildErr           error
	getImage           *models.Image
	getErr             error
}

func (s *recordingProfileImageService) Build(context.Context, *models.BuildImageRequest) (*models.Image, error) {
	s.buildCalls++
	if s.buildErr != nil {
		return nil, s.buildErr
	}
	if s.buildImage != nil {
		return s.buildImage, nil
	}
	return nil, errors.New("local profile image build should not be called")
}

func (s *recordingProfileImageService) ImportRootfs(context.Context, *models.ImportImageRequest) (*models.Image, error) {
	return nil, errors.New("import rootfs should not be called")
}

func (s *recordingProfileImageService) Get(string) (*models.Image, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getImage != nil {
		return s.getImage, nil
	}
	return nil, errors.New("get image should not be called")
}

func (s *recordingProfileImageService) Delete(string) error {
	return errors.New("delete image should not be called")
}

func (s *recordingProfileImageService) RuntimeFingerprint() string {
	return s.runtimeFingerprint
}
