package vm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

type vmImageMachineService struct {
	roleCreateMachineService
	machines    map[string]*models.Machine
	nextID      int
	rootDisk    string
	rootDiskFor string
}

func newVMImageMachineService(db *store.DB) *vmImageMachineService {
	return &vmImageMachineService{
		roleCreateMachineService: roleCreateMachineService{db: db},
		machines:                 map[string]*models.Machine{},
	}
}

func (s *vmImageMachineService) Create(_ context.Context, req *models.CreateMachineRequest) (*models.Machine, error) {
	clone := *req
	s.createReq = &clone
	s.nextID++
	now := time.Now().UTC()
	m := &models.Machine{
		ID:        fmt.Sprintf("machine-vm-image-%d", s.nextID),
		Name:      req.Name,
		Backend:   req.Backend,
		GuestOS:   req.GuestOS,
		ImageID:   req.ImageID,
		State:     models.MachineStateRunning,
		VCPUs:     1,
		CPU:       models.CPUConfig{Kind: "shared", VCPUs: 1},
		MemoryMB:  req.MemoryMB,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.CreateMachine(m); err != nil {
		return nil, fmt.Errorf("create machine: %w", err)
	}
	s.machines[m.ID] = m
	return m, nil
}

func (s *vmImageMachineService) addMachine(t *testing.T, m *models.Machine) {
	t.Helper()
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if m.CPU.VCPUs == 0 {
		m.VCPUs = 1
		m.CPU = models.CPUConfig{Kind: "shared", VCPUs: 1}
	}
	if err := s.db.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	s.machines[m.ID] = m
}

func (s *vmImageMachineService) Get(id string) (*models.Machine, error) {
	m, ok := s.machines[id]
	if !ok {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	clone := *m
	return &clone, nil
}

func (s *vmImageMachineService) Delete(id string) error {
	delete(s.machines, id)
	return s.roleCreateMachineService.Delete(id)
}

func (s *vmImageMachineService) RootDiskPath(id string) (string, error) {
	s.rootDiskFor = id
	return s.rootDisk, nil
}

type vmImageImageService struct {
	recordingProfileImageService
	db        *store.DB
	importReq *models.ImportImageRequest
	buildReq  *models.BuildImageRequest
	buildErr  error
	resolved  map[string]string
	deleted   []string
}

func (s *vmImageImageService) createBacking(tag string, sizeMB int64) (*models.Image, error) {
	img := &models.Image{
		ID:         "image-" + tag,
		Name:       vmImageBackingName,
		Tag:        tag,
		BootMode:   image.BootModePersistent,
		SizeMB:     sizeMB,
		RootfsPath: "/images/" + tag + "/rootfs.ext4",
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.db.CreateImage(img); err != nil {
		return nil, fmt.Errorf("create backing image: %w", err)
	}
	return img, nil
}

func (s *vmImageImageService) ImportRootfs(_ context.Context, req *models.ImportImageRequest) (*models.Image, error) {
	clone := *req
	s.importReq = &clone
	return s.createBacking(req.Tag, req.RootfsSizeMB)
}

func (s *vmImageImageService) Build(_ context.Context, req *models.BuildImageRequest) (*models.Image, error) {
	clone := *req
	s.buildReq = &clone
	if s.buildErr != nil {
		return nil, s.buildErr
	}
	img, err := s.createBacking(req.Tag, req.RootfsSizeMB)
	if err != nil {
		return nil, err
	}
	img.BaseImages = s.resolved
	return img, nil
}

func (s *vmImageImageService) Delete(id string) error {
	s.deleted = append(s.deleted, id)
	if err := s.db.DeleteImage(id); err != nil {
		return fmt.Errorf("delete backing image: %w", err)
	}
	return nil
}

type vmImageFixture struct {
	db       *store.DB
	machines *vmImageMachineService
	images   *vmImageImageService
	svc      *Service
}

func newVMImageFixture(t *testing.T) *vmImageFixture {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	machines := newVMImageMachineService(db)
	images := &vmImageImageService{db: db}
	return &vmImageFixture{db: db, machines: machines, images: images, svc: NewService(db, images, machines)}
}

func (f *vmImageFixture) addVM(t *testing.T, name, backend string, state models.MachineState) *models.VMRecord {
	t.Helper()
	now := time.Now().UTC()
	img := &models.Image{ID: "image-" + name, Name: "alpine", Tag: name, RootfsPath: "/tmp/" + name, CreatedAt: now}
	if err := f.db.CreateImage(img); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	f.machines.addMachine(t, &models.Machine{ID: "machine-" + name, Name: name, Backend: backend, GuestOS: "linux", ImageID: img.ID, State: state, MemoryMB: 256})
	record := &models.VMRecord{
		ID: "vm-" + name, Name: name, Profile: "alpine", MachineID: "machine-" + name,
		ImageID: img.ID, DiskSizeMB: 1024, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.db.CreateVM(record); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	return record
}

func (f *vmImageFixture) addVMImage(t *testing.T, name, profile string, diskSizeMB int64) *models.VMImage {
	t.Helper()
	img, err := f.images.createBacking("saved-"+name, diskSizeMB)
	if err != nil {
		t.Fatalf("create backing image: %v", err)
	}
	vmImage := &models.VMImage{
		ID: "vmimg-" + name, Name: name, Profile: profile, Source: models.VMImageSourceVM,
		ImageID: img.ID, DiskSizeMB: diskSizeMB, CreatedAt: time.Now().UTC(),
	}
	if err := f.db.CreateVMImage(vmImage); err != nil {
		t.Fatalf("CreateVMImage: %v", err)
	}
	return vmImage
}

func writeRootDisk(t *testing.T, sizeBytes int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create root disk: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatalf("size root disk: %v", err)
	}
	return path
}

func assertValidationField(t *testing.T, err error, field string) {
	t.Helper()
	var validation *apierr.ValidationError
	if !errors.As(err, &validation) || validation.Field != field {
		t.Fatalf("error = %v, want validation error on %q", err, field)
	}
}

func TestCreateImageRejectsInvalidRequests(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVM(t, "source", "firecracker", models.MachineStateStopped)
	f.addVMImage(t, "taken", "alpine", 1024)

	tests := []struct {
		name  string
		req   models.CreateVMImageRequest
		field string
	}{
		{"missing name", models.CreateVMImageRequest{Name: "  ", FromVM: "source"}, "name"},
		{"neither source", models.CreateVMImageRequest{Name: "env"}, "from_vm"},
		{"both sources", models.CreateVMImageRequest{Name: "env", FromVM: "source", Dockerfile: "FROM alpine"}, "from_vm"},
		{"from_vm with profile", models.CreateVMImageRequest{Name: "env", FromVM: "source", Profile: "debian"}, "from_vm"},
		{"from_vm with disk", models.CreateVMImageRequest{Name: "env", FromVM: "source", DiskSizeMB: 2048}, "from_vm"},
		{"from_vm with context", models.CreateVMImageRequest{Name: "env", FromVM: "source", Context: "AA=="}, "from_vm"},
		{"unknown from_vm", models.CreateVMImageRequest{Name: "env", FromVM: "missing"}, "from_vm"},
		{"bad label key", models.CreateVMImageRequest{Name: "env", FromVM: "source", Labels: map[string]string{"Bad": "x"}}, "labels"},
		{"windows profile", models.CreateVMImageRequest{Name: "env", Dockerfile: "FROM alpine", Profile: "windows11"}, "profile"},
		{"unknown profile", models.CreateVMImageRequest{Name: "env", Dockerfile: "FROM alpine", Profile: "missing"}, "profile"},
		{"bad context", models.CreateVMImageRequest{Name: "env", Dockerfile: "FROM alpine", Context: "not base64!"}, "context"},
		{"disk too small", models.CreateVMImageRequest{Name: "env", Dockerfile: "FROM alpine", DiskSizeMB: 1}, "disk_size_mb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.CreateImage(context.Background(), &tt.req)
			assertValidationField(t, err, tt.field)
		})
	}

	_, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "taken", FromVM: "source"})
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("duplicate name error = %v, want conflict", err)
	}
	if f.images.importReq != nil || f.images.buildReq != nil {
		t.Fatalf("invalid requests reached the image service: import=%+v build=%+v", f.images.importReq, f.images.buildReq)
	}
}

func TestSaveVMImageRequiresStoppedFirecrackerVM(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVM(t, "running", "firecracker", models.MachineStateRunning)
	f.addVM(t, "windows", "libvirt-qemu", models.MachineStateStopped)
	f.machines.rootDisk = writeRootDisk(t, 4<<20)

	_, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "running"})
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("running VM error = %v, want conflict", err)
	}
	_, err = f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "windows"})
	assertValidationField(t, err, "from_vm")
	if f.images.importReq != nil {
		t.Fatalf("rejected saves imported a disk: %+v", f.images.importReq)
	}
	if images, err := f.svc.ListImages(); err != nil || len(images) != 0 {
		t.Fatalf("ListImages = %+v, %v; want none", images, err)
	}
}

func TestSaveVMImageCopiesStoppedRootDisk(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVM(t, "source", "firecracker", models.MachineStateStopped)
	f.machines.rootDisk = writeRootDisk(t, 3<<20)
	labels := map[string]string{"kiln-run/env": "abc"}

	saved, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "source", Labels: labels})
	if err != nil {
		t.Fatalf("CreateImage from_vm: %v", err)
	}
	if f.machines.rootDiskFor != "machine-source" {
		t.Fatalf("root disk requested for %q, want machine-source", f.machines.rootDiskFor)
	}
	req := f.images.importReq
	if req == nil || req.RootfsPath != f.machines.rootDisk || req.RootfsSizeMB != 3 || req.BootMode != image.BootModePersistent || req.Name != vmImageBackingName || req.Tag != saved.ID {
		t.Fatalf("import request = %+v", req)
	}
	if saved.Name != "env" || saved.Profile != "alpine" || saved.Source != models.VMImageSourceVM || saved.DiskSizeMB != 3 ||
		saved.ImageID != "image-"+saved.ID || !maps.Equal(saved.Labels, labels) || saved.Resolved != nil {
		t.Fatalf("saved image = %+v", saved)
	}
	got, err := f.svc.GetImage("env")
	if err != nil || got.ID != saved.ID {
		t.Fatalf("GetImage(env) = %+v, %v", got, err)
	}
	if got, err := f.svc.GetImage(saved.ID[:8]); err != nil || got.Name != "env" {
		t.Fatalf("GetImage(prefix) = %+v, %v", got, err)
	}
}

func TestSaveVMImageRejectsPartialMiBDisk(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVM(t, "source", "firecracker", models.MachineStateStopped)
	f.machines.rootDisk = writeRootDisk(t, 3<<20+1)

	if _, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "source"}); err == nil {
		t.Fatal("CreateImage accepted a root disk that is not a whole number of MiB")
	}
	if f.images.importReq != nil {
		t.Fatalf("import was called: %+v", f.images.importReq)
	}
}

func TestBuildVMImageUsesDebianDefaults(t *testing.T) {
	f := newVMImageFixture(t)
	f.images.resolved = map[string]string{"alpine:3.20": "alpine@sha256:abc"}
	dockerfile := "FROM alpine:3.20\nRUN echo hi > /hi\n"

	built, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{
		Name: "built", Dockerfile: dockerfile, Context: "AAAA", Labels: map[string]string{"team": "ci"},
	})
	if err != nil {
		t.Fatalf("CreateImage dockerfile: %v", err)
	}
	debian := profiles["debian"]
	req := f.images.buildReq
	if req == nil || req.Dockerfile != dockerfile || req.Context != "AAAA" || req.RootfsSizeMB != debian.DefaultDiskSizeMB ||
		req.BootMode != image.BootModePersistent || req.Name != vmImageBackingName || req.Tag != built.ID {
		t.Fatalf("build request = %+v", req)
	}
	if built.Profile != "debian" || built.Source != models.VMImageSourceDockerfile || built.DiskSizeMB != debian.DefaultDiskSizeMB ||
		!maps.Equal(built.Resolved, f.images.resolved) || built.Labels["team"] != "ci" {
		t.Fatalf("built image = %+v", built)
	}

	sized, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{
		Name: "sized", Dockerfile: dockerfile, Profile: "alpine", DiskSizeMB: 2048,
	})
	if err != nil {
		t.Fatalf("CreateImage sized: %v", err)
	}
	if sized.Profile != "alpine" || sized.DiskSizeMB != 2048 || f.images.buildReq.RootfsSizeMB != 2048 {
		t.Fatalf("sized image = %+v, build request = %+v", sized, f.images.buildReq)
	}
}

func TestBuildVMImageFailureRecordsNothing(t *testing.T) {
	f := newVMImageFixture(t)
	f.images.buildErr = errors.New("docker build failed")

	_, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "broken", Dockerfile: "FROM alpine"})
	if !errors.Is(err, f.images.buildErr) {
		t.Fatalf("CreateImage error = %v, want build error", err)
	}
	if _, err := f.svc.GetImage("broken"); !errors.Is(err, apierr.ErrNotFound) {
		t.Fatalf("GetImage after failed build error = %v, want not found", err)
	}
}

func TestCreateVMFromImage(t *testing.T) {
	f := newVMImageFixture(t)
	vmImage := f.addVMImage(t, "env", "debian", 2048)
	labels := map[string]string{"kiln-run/env": "abc", "owner": "kiln-run"}

	created, err := f.svc.Create(context.Background(), &models.CreateVMRequest{Name: "from-image", Image: "env", Labels: labels})
	if err != nil {
		t.Fatalf("Create from image: %v", err)
	}
	if created.Image != "env" || created.Profile != "debian" || created.DiskSizeMB != 2048 || created.ImageID != vmImage.ImageID || !maps.Equal(created.Labels, labels) {
		t.Fatalf("created VM = %+v", created)
	}
	req := f.machines.createReq
	if req.ImageID != vmImage.ImageID || req.RootfsSizeMB != 2048 || req.Backend != "firecracker" {
		t.Fatalf("machine request = %+v", req)
	}
	if f.images.buildCalls != 0 {
		t.Fatalf("profile image build calls = %d, want 0", f.images.buildCalls)
	}

	larger, err := f.svc.Create(context.Background(), &models.CreateVMRequest{Name: "larger", Image: vmImage.ID, DiskSizeMB: 4096})
	if err != nil {
		t.Fatalf("Create larger from image: %v", err)
	}
	if larger.DiskSizeMB != 4096 || f.machines.createReq.RootfsSizeMB != 4096 {
		t.Fatalf("larger VM disk = %d, machine rootfs = %d", larger.DiskSizeMB, f.machines.createReq.RootfsSizeMB)
	}

	got, err := f.svc.Get("from-image")
	if err != nil || got.Image != "env" || !maps.Equal(got.Labels, labels) {
		t.Fatalf("Get(from-image) = %+v, %v", got, err)
	}
}

func TestCreateVMFromImageRejectsInvalidRequests(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVMImage(t, "env", "debian", 2048)

	tests := []struct {
		name  string
		req   models.CreateVMRequest
		field string
	}{
		{"image and profile", models.CreateVMRequest{Name: "vm", Image: "env", Profile: "debian"}, "image"},
		{"unknown image", models.CreateVMRequest{Name: "vm", Image: "missing"}, "image"},
		{"disk smaller than image", models.CreateVMRequest{Name: "vm", Image: "env", DiskSizeMB: 1024}, "disk_size_mb"},
		{"too many labels", models.CreateVMRequest{Name: "vm", Image: "env", Labels: manyLabels(MaxLabels + 1)}, "labels"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Create(context.Background(), &tt.req)
			assertValidationField(t, err, tt.field)
		})
	}
	if f.machines.createReq != nil {
		t.Fatalf("invalid requests created a machine: %+v", f.machines.createReq)
	}
}

func manyLabels(n int) map[string]string {
	labels := make(map[string]string, n)
	for i := range n {
		labels[fmt.Sprintf("k%d", i)] = "v"
	}
	return labels
}

func TestListFiltersVMsAndImagesByLabel(t *testing.T) {
	f := newVMImageFixture(t)
	f.addVMImage(t, "env", "alpine", 1024)
	for name, labels := range map[string]map[string]string{
		"a": {"owner": "kiln-run", "env": "x"},
		"b": {"owner": "kiln-run"},
		"c": nil,
	} {
		if _, err := f.svc.Create(context.Background(), &models.CreateVMRequest{Name: name, Image: "env", Labels: labels}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	names := func(selectors ...LabelSelector) []string {
		vms, err := f.svc.List(selectors...)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out := map[string]bool{}
		for _, vm := range vms {
			out[vm.Name] = true
		}
		result := make([]string, 0, len(out))
		for _, n := range []string{"a", "b", "c"} {
			if out[n] {
				result = append(result, n)
			}
		}
		return result
	}
	owner := LabelSelector{Key: "owner", Value: "kiln-run", HasValue: true}
	env := LabelSelector{Key: "env"}
	if got := names(); fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("List() = %v", got)
	}
	if got := names(owner); fmt.Sprint(got) != "[a b]" {
		t.Fatalf("List(owner=kiln-run) = %v", got)
	}
	if got := names(owner, env); fmt.Sprint(got) != "[a]" {
		t.Fatalf("List(owner=kiln-run, env) = %v", got)
	}

	built, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "labelled", Dockerfile: "FROM alpine", Labels: map[string]string{"env": "x"}})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	images, err := f.svc.ListImages(env)
	if err != nil || len(images) != 1 || images[0].ID != built.ID {
		t.Fatalf("ListImages(env) = %+v, %v", images, err)
	}
	all, err := f.svc.ListImages()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListImages() = %+v, %v", all, err)
	}
}

func TestDeleteImageKeepsBackingDiskWhileVMsUseIt(t *testing.T) {
	f := newVMImageFixture(t)
	vmImage := f.addVMImage(t, "shared-env", "alpine", 1024)
	if _, err := f.svc.Create(context.Background(), &models.CreateVMRequest{Name: "user", Image: "shared-env"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := f.svc.DeleteImage("shared-env"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if _, err := f.svc.GetImage("shared-env"); !errors.Is(err, apierr.ErrNotFound) {
		t.Fatalf("GetImage after delete error = %v, want not found", err)
	}
	if len(f.images.deleted) != 0 {
		t.Fatalf("backing image deleted while a VM uses it: %v", f.images.deleted)
	}
	if err := f.svc.Delete("user"); err != nil {
		t.Fatalf("Delete VM: %v", err)
	}
	if fmt.Sprint(f.images.deleted) != fmt.Sprint([]string{vmImage.ImageID}) {
		t.Fatalf("deleted backing images = %v, want %s", f.images.deleted, vmImage.ImageID)
	}
	if err := f.svc.DeleteImage("shared-env"); !errors.Is(err, apierr.ErrNotFound) {
		t.Fatalf("second DeleteImage error = %v, want not found", err)
	}
}

func TestDeleteVMKeepsImageOwnedByVMImage(t *testing.T) {
	f := newVMImageFixture(t)
	vmImage := f.addVMImage(t, "shared-env", "alpine", 1024)
	if _, err := f.svc.Create(context.Background(), &models.CreateVMRequest{Name: "user", Image: "shared-env"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.svc.Delete("user"); err != nil {
		t.Fatalf("Delete VM: %v", err)
	}
	if len(f.images.deleted) != 0 {
		t.Fatalf("VM delete removed the VM image's backing disk: %v", f.images.deleted)
	}

	if err := f.svc.DeleteImage("shared-env"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if fmt.Sprint(f.images.deleted) != fmt.Sprint([]string{vmImage.ImageID}) {
		t.Fatalf("deleted backing images = %v, want %s", f.images.deleted, vmImage.ImageID)
	}
}

func TestSaveAndStartReservationsExcludeEachOther(t *testing.T) {
	f := newVMImageFixture(t)
	record := f.addVM(t, "source", "firecracker", models.MachineStateStopped)

	releaseSave, err := f.svc.reserveSave(record)
	if err != nil {
		t.Fatalf("reserveSave: %v", err)
	}
	if _, err := f.svc.reserveSave(record); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("second reserveSave error = %v, want conflict", err)
	}
	if _, err := f.svc.Start(context.Background(), "source"); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("Start during save error = %v, want conflict", err)
	}
	if err := f.svc.Delete("source"); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("Delete during save error = %v, want conflict", err)
	}
	releaseSave()

	releaseStart, err := f.svc.reserveStart(record)
	if err != nil {
		t.Fatalf("reserveStart: %v", err)
	}
	f.machines.rootDisk = writeRootDisk(t, 1<<20)
	if _, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "source"}); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("save during start error = %v, want conflict", err)
	}
	releaseStart()
	if _, err := f.svc.CreateImage(context.Background(), &models.CreateVMImageRequest{Name: "env", FromVM: "source"}); err != nil {
		t.Fatalf("save after start released: %v", err)
	}
}
