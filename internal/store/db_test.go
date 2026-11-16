package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenCreatesDBAndRunsMigrations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatalf("database not functional: %v", err)
	}

	// Verify tables exist by querying them
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM machines").Scan(&count); err != nil {
		t.Fatalf("machines table not created: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM images").Scan(&count); err != nil {
		t.Fatalf("images table not created: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil {
		t.Fatalf("tokens table not created: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM vms").Scan(&count); err != nil {
		t.Fatalf("vms table not created: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM vm_profile_images").Scan(&count); err != nil {
		t.Fatalf("vm_profile_images table not created: %v", err)
	}
}

// --- Machine CRUD ---

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()

	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("Open newer schema error = %v, want version refusal", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		db, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		db.Close()
	}
}

func TestMachineCreate(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	m := &models.Machine{
		ID:        "m-1",
		Name:      "test-vm",
		ImageID:   "img-1",
		State:     models.MachineStateCreating,
		VCPUs:     2,
		MemoryMB:  1024,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := db.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	got, err := db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.Name != "test-vm" {
		t.Errorf("Name = %q, want %q", got.Name, "test-vm")
	}
	if got.ImageID != "img-1" {
		t.Errorf("ImageID = %q, want %q", got.ImageID, "img-1")
	}
	if got.State != models.MachineStateCreating {
		t.Errorf("State = %q, want %q", got.State, models.MachineStateCreating)
	}
	if got.VCPUs != 2 {
		t.Errorf("VCPUs = %d, want 2", got.VCPUs)
	}
	if got.MemoryMB != 1024 {
		t.Errorf("MemoryMB = %d, want 1024", got.MemoryMB)
	}
}

func TestMachineCreatePersistsLibvirtDomainSpec(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	m := &models.Machine{
		ID:      "m-libvirt",
		Name:    "windows-vm",
		Backend: "libvirt-qemu",
		GuestOS: "linux",
		Libvirt: &models.LibvirtDomainSpec{
			OSInfo:   "linux2022",
			Firmware: "uefi",
			Disk:     models.LibvirtDiskSpec{Bus: "virtio", Format: "qcow2"},
			Network:  models.LibvirtNetworkSpec{Source: "kiln-vm-test-net", Model: "virtio"},
			Graphics: models.LibvirtGraphicsSpec{Type: "none"},
		},
		ImageID:   "img-linux",
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  1024,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := db.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	got, err := db.GetMachine("m-libvirt")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.Libvirt == nil {
		t.Fatal("Libvirt spec was not persisted")
	}
	if got.Libvirt.OSInfo != "linux2022" ||
		got.Libvirt.Firmware != "uefi" ||
		got.Libvirt.Disk.Bus != "virtio" ||
		got.Libvirt.Disk.Format != "qcow2" ||
		got.Libvirt.Network.Source != "kiln-vm-test-net" ||
		got.Libvirt.Network.Model != "virtio" ||
		got.Libvirt.Graphics.Type != "none" {
		t.Fatalf("Libvirt spec = %+v", got.Libvirt)
	}

	list, err := db.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(list) != 1 || list[0].Libvirt == nil || list[0].Libvirt.Disk.Bus != "virtio" || list[0].Libvirt.Network.Source != "kiln-vm-test-net" {
		t.Fatalf("ListMachines libvirt spec = %+v", list)
	}
}

func TestMachineGetNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.GetMachine("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent machine, got nil")
	}
}

func TestMachineList(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Empty list
	machines, err := db.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines (empty): %v", err)
	}
	if len(machines) != 0 {
		t.Errorf("expected 0 machines, got %d", len(machines))
	}

	// Add two machines
	for _, id := range []string{"m-1", "m-2"} {
		m := &models.Machine{
			ID:        id,
			Name:      "vm-" + id,
			ImageID:   "img-1",
			State:     models.MachineStateCreating,
			VCPUs:     1,
			MemoryMB:  512,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := db.CreateMachine(m); err != nil {
			t.Fatalf("CreateMachine(%s): %v", id, err)
		}
	}

	machines, err = db.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(machines) != 2 {
		t.Errorf("expected 2 machines, got %d", len(machines))
	}
}

func TestMachineUpdateState(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	m := &models.Machine{
		ID:        "m-1",
		Name:      "test-vm",
		ImageID:   "img-1",
		State:     models.MachineStateCreating,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	if err := db.UpdateMachineState("m-1", models.MachineStateRunning, ""); err != nil {
		t.Fatalf("UpdateMachineState: %v", err)
	}

	got, err := db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.State != models.MachineStateRunning {
		t.Errorf("State = %q, want %q", got.State, models.MachineStateRunning)
	}

	// Update with error message
	if err := db.UpdateMachineState("m-1", models.MachineStateError, "something broke"); err != nil {
		t.Fatalf("UpdateMachineState (error): %v", err)
	}

	got, err = db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.State != models.MachineStateError {
		t.Errorf("State = %q, want %q", got.State, models.MachineStateError)
	}
	if got.Error != "something broke" {
		t.Errorf("Error = %q, want %q", got.Error, "something broke")
	}
}

func TestMachineUpdateStateWithDiagnosticsPersistsAndClears(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateMachine(&models.Machine{
		ID:        "m-1",
		Name:      "test-vm",
		ImageID:   "img-1",
		State:     models.MachineStateCreating,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	diagnostics := []models.Diagnostic{{
		Category:    "machine_boot",
		Phase:       "process_start",
		Resource:    "test-vm",
		Backend:     "firecracker",
		KernelPath:  "/bad/kernel",
		KernelType:  "missing",
		LogPath:     "/var/lib/kiln-vm/machines/m-1/console.log",
		Cleanup:     "released runtime network artifacts",
		Remediation: "check kernel",
		Error:       "start firecracker: exit status 1",
	}}
	if err := db.UpdateMachineStateWithDiagnostics("m-1", models.MachineStateError, "boot failed", diagnostics); err != nil {
		t.Fatalf("UpdateMachineStateWithDiagnostics: %v", err)
	}

	got, err := db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.Error != "boot failed" || len(got.Diagnostics) != 1 {
		t.Fatalf("machine error/diagnostics = %q %+v", got.Error, got.Diagnostics)
	}
	if got.Diagnostics[0].KernelPath != "/bad/kernel" || got.Diagnostics[0].Cleanup == "" {
		t.Fatalf("diagnostic = %+v", got.Diagnostics[0])
	}
	list, err := db.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(list) != 1 || len(list[0].Diagnostics) != 1 {
		t.Fatalf("listed diagnostics = %+v", list)
	}

	cleanupDiagnostics := []models.Diagnostic{{
		Category:     "provider_network",
		Phase:        "cleanup",
		Retryable:    true,
		NetworkCIDR:  "172.16.0.0/16",
		NetworkIndex: 7,
		GuestIP:      "172.16.0.30",
		GatewayIP:    "172.16.0.29",
		GuestMAC:     "02:FC:00:00:00:07",
		Netmask:      "255.255.255.252",
		TAPAlias:     "kiln-vm-public-network:v1:kv7:172.16.0.29:172.16.0.30:02:fc:00:00:00:07",
		Error:        "owned host cleanup incomplete",
	}}
	if err := db.UpdateMachineStateWithDiagnostics("m-1", models.MachineStateError, "cleanup pending", cleanupDiagnostics); err != nil {
		t.Fatalf("UpdateMachineStateWithDiagnostics(cleanup): %v", err)
	}
	got, err = db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine after provider cleanup diagnostic: %v", err)
	}
	if diagnostic := got.Diagnostics[0]; diagnostic.NetworkCIDR != cleanupDiagnostics[0].NetworkCIDR ||
		diagnostic.NetworkIndex != cleanupDiagnostics[0].NetworkIndex || diagnostic.GuestIP != cleanupDiagnostics[0].GuestIP ||
		diagnostic.GatewayIP != cleanupDiagnostics[0].GatewayIP || diagnostic.GuestMAC != cleanupDiagnostics[0].GuestMAC ||
		diagnostic.Netmask != cleanupDiagnostics[0].Netmask || diagnostic.TAPAlias != cleanupDiagnostics[0].TAPAlias {
		t.Fatalf("cleanup identity did not round-trip: got %+v want %+v", diagnostic, cleanupDiagnostics[0])
	}

	if err := db.UpdateMachineState("m-1", models.MachineStateRunning, ""); err != nil {
		t.Fatalf("UpdateMachineState(running): %v", err)
	}
	got, err = db.GetMachine("m-1")
	if err != nil {
		t.Fatalf("GetMachine after clear: %v", err)
	}
	if got.Error != "" || len(got.Diagnostics) != 0 {
		t.Fatalf("running machine kept stale diagnostics: error=%q diagnostics=%+v", got.Error, got.Diagnostics)
	}
}

func TestMachineNetworkIntentPersistsCompleteTupleAndCreatingStateAtomically(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{ID: "image-public-intent", Name: "image", RootfsPath: "/tmp/rootfs", CreatedAt: now}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	machine := &models.Machine{
		ID:        "machine-public-intent",
		Name:      "machine-public-intent",
		Backend:   "firecracker",
		GuestOS:   "linux",
		ImageID:   "image-public-intent",
		State:     models.MachineStateStopped,
		VCPUs:     1,
		CPU:       models.CPUConfig{Kind: "shared", Millis: 1000, VCPUs: 1},
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateMachine(machine); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	machine.IP = "172.16.0.6"
	machine.GatewayIP = "172.16.0.5"
	machine.TAPDevice = "kv1"
	machine.NetworkCIDR = "172.16.0.0/16"
	machine.NetworkIndex = 1
	machine.GuestMAC = "02:FC:00:00:00:01"
	machine.Netmask = "255.255.255.252"
	machine.State = models.MachineStateCreating
	machine.Error = ""
	if err := db.UpdateMachineNetworkIntent(machine); err != nil {
		t.Fatalf("UpdateMachineNetworkIntent: %v", err)
	}

	persisted, err := db.GetMachine(machine.ID)
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if persisted.State != models.MachineStateCreating || persisted.IP != machine.IP || persisted.GatewayIP != machine.GatewayIP ||
		persisted.TAPDevice != machine.TAPDevice || persisted.NetworkCIDR != machine.NetworkCIDR ||
		persisted.NetworkIndex != machine.NetworkIndex || persisted.GuestMAC != machine.GuestMAC ||
		persisted.Netmask != machine.Netmask {
		t.Fatalf("persisted public-network intent = %+v, want complete tuple and creating state from %+v", persisted, machine)
	}
}

func TestMachineDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	m := &models.Machine{
		ID:        "m-1",
		Name:      "test-vm",
		ImageID:   "img-1",
		State:     models.MachineStateCreating,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	if err := db.DeleteMachine("m-1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}

	_, err := db.GetMachine("m-1")
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
}

// --- Image CRUD ---

func TestImageCreate(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	img := &models.Image{
		ID:         "img-1",
		Name:       "ubuntu",
		Tag:        "22.04",
		SizeMB:     500,
		RootfsPath: "/var/lib/kiln-vm/images/img-1/rootfs.ext4",
		CreatedAt:  now,
	}

	if err := db.CreateImage(img); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	got, err := db.GetImage("img-1")
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if got.Name != "ubuntu" {
		t.Errorf("Name = %q, want %q", got.Name, "ubuntu")
	}
	if got.Tag != "22.04" {
		t.Errorf("Tag = %q, want %q", got.Tag, "22.04")
	}
	if got.BootMode != "persistent" {
		t.Errorf("BootMode = %q, want %q", got.BootMode, "persistent")
	}
	if got.SizeMB != 500 {
		t.Errorf("SizeMB = %d, want 500", got.SizeMB)
	}
	if got.RootfsPath != "/var/lib/kiln-vm/images/img-1/rootfs.ext4" {
		t.Errorf("RootfsPath = %q, want %q", got.RootfsPath, "/var/lib/kiln-vm/images/img-1/rootfs.ext4")
	}
}

// --- VM CRUD ---

func TestVMCreateGetListAndDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateImage(&models.Image{
		ID:         "img-vm-1",
		Name:       "vm-dev",
		Tag:        "alpine",
		BootMode:   "persistent",
		SizeMB:     1024,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-vm-1",
		Name:      "vm-dev",
		ImageID:   "img-vm-1",
		State:     models.MachineStateStopped,
		VCPUs:     2,
		MemoryMB:  1024,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}

	record := &models.VMRecord{
		ID:         "vm-1",
		Name:       "dev",
		Profile:    "alpine",
		MachineID:  "machine-vm-1",
		ImageID:    "img-vm-1",
		DiskSizeMB: 2048,
		Power: models.VMPower{
			Policy: models.VMPowerPolicy{Mode: models.VMPowerModeAlways},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateVM(record); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	got, err := db.GetVM("dev")
	if err != nil {
		t.Fatalf("GetVM by name: %v", err)
	}
	if got.ID != "vm-1" || got.MachineID != "machine-vm-1" || got.Power.Policy.Mode != models.VMPowerModeAlways {
		t.Fatalf("unexpected VM record: %+v", got)
	}

	got, err = db.GetVM("vm-1")
	if err != nil {
		t.Fatalf("GetVM by id: %v", err)
	}
	if got.Name != "dev" {
		t.Fatalf("GetVM name = %q, want dev", got.Name)
	}
	count, err := db.CountVMsByImageID("img-vm-1")
	if err != nil {
		t.Fatalf("CountVMsByImageID: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountVMsByImageID = %d, want 1", count)
	}

	daemonStart, err := db.ListPowerStartVMs()
	if err != nil {
		t.Fatalf("ListPowerStartVMs: %v", err)
	}
	if len(daemonStart) != 1 || daemonStart[0].ID != "vm-1" {
		t.Fatalf("unexpected daemon-start list: %+v", daemonStart)
	}

	vms, err := db.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("expected 1 VM, got %d", len(vms))
	}

	if err := db.DeleteVM("vm-1"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if _, err := db.GetVM("vm-1"); err == nil {
		t.Fatal("expected VM to be deleted")
	}
}

func TestVMCapabilityConditionCreateUpdateListAndCascade(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateImage(&models.Image{
		ID:         "img-vm-cap",
		Name:       "vm-cap",
		Tag:        "alpine",
		BootMode:   "persistent",
		SizeMB:     1024,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-vm-cap",
		Name:      "vm-cap",
		ImageID:   "img-vm-cap",
		State:     models.MachineStateRunning,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-cap",
		Name:       "cap",
		Profile:    "ubuntu-docker",
		MachineID:  "machine-vm-cap",
		ImageID:    "img-vm-cap",
		DiskSizeMB: 1024,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	checked := now.Add(time.Minute)
	if err := db.UpsertVMCapabilityCondition("vm-cap", models.VMCapabilityCondition{
		Capability: "docker",
		Status:     models.VMCapabilityStatusPending,
		Detail:     "waiting",
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("UpsertVMCapabilityCondition pending: %v", err)
	}
	if err := db.UpsertVMCapabilityCondition("vm-cap", models.VMCapabilityCondition{
		Capability: "docker",
		Status:     models.VMCapabilityStatusOK,
		Phase:      "docker_networking",
		Command:    "docker version",
		Detail:     "ok",
		CheckedAt:  &checked,
		UpdatedAt:  checked,
	}); err != nil {
		t.Fatalf("UpsertVMCapabilityCondition ok: %v", err)
	}

	conditions, err := db.ListVMCapabilityConditions("vm-cap")
	if err != nil {
		t.Fatalf("ListVMCapabilityConditions: %v", err)
	}
	if len(conditions) != 1 {
		t.Fatalf("conditions = %+v, want one", conditions)
	}
	got := conditions[0]
	if got.Capability != "docker" || got.Status != models.VMCapabilityStatusOK || got.Phase != "docker_networking" || got.CheckedAt == nil {
		t.Fatalf("condition = %+v", got)
	}

	if err := db.DeleteVM("vm-cap"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	conditions, err = db.ListVMCapabilityConditions("vm-cap")
	if err != nil {
		t.Fatalf("ListVMCapabilityConditions after delete: %v", err)
	}
	if len(conditions) != 0 {
		t.Fatalf("conditions after VM delete = %+v, want none", conditions)
	}
}

func TestVMCreateDefaultsToManualNoWake(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateImage(&models.Image{
		ID:         "img-vm-manual",
		Name:       "vm-manual",
		Tag:        "alpine",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-vm-manual",
		Name:      "vm-manual",
		ImageID:   "img-vm-manual",
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  256,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-manual",
		Name:       "manual",
		Profile:    "alpine",
		MachineID:  "machine-vm-manual",
		ImageID:    "img-vm-manual",
		DiskSizeMB: 128,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	got, err := db.GetVM("manual")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Power.Policy.Mode != models.VMPowerModeManual {
		t.Fatalf("default power policy = %+v, want manual", got.Power.Policy)
	}
}

func TestListPowerStartVMsResumesManualVMsTheDaemonStopped(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.CreateImage(&models.Image{
		ID:         "img-resume",
		Name:       "vm-resume",
		Tag:        "alpine",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	cases := []struct {
		name   string
		mode   models.VMPowerMode
		state  models.MachineState
		reason string
	}{
		{name: "manual-daemon-shutdown", mode: models.VMPowerModeManual, state: models.MachineStateStopped, reason: models.MachineStopReasonDaemonShutdown},
		{name: "manual-daemon-restarted", mode: models.VMPowerModeManual, state: models.MachineStateStopped, reason: models.MachineStopReasonDaemonRestarted},
		{name: "manual-user-stopped", mode: models.VMPowerModeManual, state: models.MachineStateStopped, reason: ""},
		{name: "manual-error", mode: models.VMPowerModeManual, state: models.MachineStateError, reason: models.MachineStopReasonDaemonShutdown},
		{name: "on-demand-daemon-shutdown", mode: models.VMPowerModeOnDemand, state: models.MachineStateStopped, reason: models.MachineStopReasonDaemonShutdown},
		{name: "always-user-stopped", mode: models.VMPowerModeAlways, state: models.MachineStateStopped, reason: ""},
	}
	for i, tc := range cases {
		createdAt := now.Add(time.Duration(i) * time.Second)
		if err := db.CreateMachine(&models.Machine{
			ID:        "machine-" + tc.name,
			Name:      tc.name,
			ImageID:   "img-resume",
			State:     tc.state,
			Error:     tc.reason,
			VCPUs:     1,
			MemoryMB:  256,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}); err != nil {
			t.Fatalf("CreateMachine %s: %v", tc.name, err)
		}
		if err := db.UpdateMachineState("machine-"+tc.name, tc.state, tc.reason); err != nil {
			t.Fatalf("UpdateMachineState %s: %v", tc.name, err)
		}
		policy := models.VMPowerPolicy{Mode: tc.mode}
		if tc.mode == models.VMPowerModeOnDemand {
			policy.IdleTimeoutSeconds = 600
		}
		if err := db.CreateVM(&models.VMRecord{
			ID:         "vm-" + tc.name,
			Name:       tc.name,
			Profile:    "alpine",
			MachineID:  "machine-" + tc.name,
			ImageID:    "img-resume",
			DiskSizeMB: 128,
			Power:      models.VMPower{Policy: policy},
			CreatedAt:  createdAt,
			UpdatedAt:  createdAt,
		}); err != nil {
			t.Fatalf("CreateVM %s: %v", tc.name, err)
		}
	}

	records, err := db.ListPowerStartVMs()
	if err != nil {
		t.Fatalf("ListPowerStartVMs: %v", err)
	}
	var got []string
	for _, record := range records {
		got = append(got, record.Name)
	}
	want := []string{"manual-daemon-shutdown", "manual-daemon-restarted", "always-user-stopped"}
	if !slices.Equal(got, want) {
		t.Fatalf("ListPowerStartVMs = %v, want %v", got, want)
	}
}

func TestVMDeleteCascadesFromMachine(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateImage(&models.Image{
		ID:         "img-vm-cascade",
		Name:       "vm-cascade",
		Tag:        "alpine",
		BootMode:   "persistent",
		SizeMB:     1024,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        "machine-vm-cascade",
		Name:      "vm-cascade",
		ImageID:   "img-vm-cascade",
		State:     models.MachineStateStopped,
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-cascade",
		Name:       "cascade",
		Profile:    "alpine",
		MachineID:  "machine-vm-cascade",
		ImageID:    "img-vm-cascade",
		DiskSizeMB: 1024,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	if err := db.DeleteMachine("machine-vm-cascade"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	if _, err := db.GetVM("vm-cascade"); err == nil {
		t.Fatal("expected VM to cascade when backing machine is deleted")
	}
}

func TestVMProfileImageCreateFindListAndDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.CreateImage(&models.Image{
		ID:         "img-profile-alpine",
		Name:       "vm-profile-alpine",
		Tag:        "v1-128mb",
		BootMode:   "persistent",
		SizeMB:     128,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	record := &models.VMProfileImage{
		ID:           "cache-1",
		Profile:      "alpine",
		ImageID:      "img-profile-alpine",
		Fingerprint:  "fingerprint-1",
		RootfsSizeMB: 128,
		CreatedAt:    now,
	}
	if err := db.CreateVMProfileImage(record); err != nil {
		t.Fatalf("CreateVMProfileImage: %v", err)
	}

	got, err := db.FindVMProfileImage("alpine", "fingerprint-1", 128)
	if err != nil {
		t.Fatalf("FindVMProfileImage: %v", err)
	}
	if got.ID != "cache-1" || got.ImageID != "img-profile-alpine" {
		t.Fatalf("unexpected profile image: %+v", got)
	}

	got, err = db.GetVMProfileImageByImageID("img-profile-alpine")
	if err != nil {
		t.Fatalf("GetVMProfileImageByImageID: %v", err)
	}
	if got.Profile != "alpine" {
		t.Fatalf("profile = %q, want alpine", got.Profile)
	}

	images, err := db.ListVMProfileImages()
	if err != nil {
		t.Fatalf("ListVMProfileImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 profile image, got %d", len(images))
	}

	if err := db.DeleteVMProfileImage("cache-1"); err != nil {
		t.Fatalf("DeleteVMProfileImage: %v", err)
	}
	if _, err := db.GetVMProfileImage("cache-1"); err == nil {
		t.Fatal("expected profile image record to be deleted")
	}
}

func TestVMProfileImageDeleteByProfile(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	for _, tc := range []struct {
		id      string
		profile string
		imageID string
	}{
		{id: "cache-alpine", profile: "alpine", imageID: "img-alpine"},
		{id: "cache-ubuntu", profile: "ubuntu", imageID: "img-ubuntu"},
	} {
		if err := db.CreateImage(&models.Image{
			ID:         tc.imageID,
			Name:       "vm-profile-" + tc.profile,
			Tag:        "latest",
			BootMode:   "persistent",
			SizeMB:     128,
			RootfsPath: "/tmp/rootfs.ext4",
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("CreateImage(%s): %v", tc.imageID, err)
		}
		if err := db.CreateVMProfileImage(&models.VMProfileImage{
			ID:           tc.id,
			Profile:      tc.profile,
			ImageID:      tc.imageID,
			Fingerprint:  "fingerprint-" + tc.profile,
			RootfsSizeMB: 128,
			CreatedAt:    now,
		}); err != nil {
			t.Fatalf("CreateVMProfileImage(%s): %v", tc.id, err)
		}
	}

	deleted, err := db.DeleteVMProfileImagesByProfile("alpine")
	if err != nil {
		t.Fatalf("DeleteVMProfileImagesByProfile: %v", err)
	}
	if len(deleted) != 1 || deleted[0].ID != "cache-alpine" {
		t.Fatalf("unexpected deleted records: %+v", deleted)
	}
	if _, err := db.GetVMProfileImage("cache-alpine"); err == nil {
		t.Fatal("expected alpine cache to be deleted")
	}
	if _, err := db.GetVMProfileImage("cache-ubuntu"); err != nil {
		t.Fatalf("expected ubuntu cache to remain: %v", err)
	}
}

func TestImageList(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	images, err := db.ListImages()
	if err != nil {
		t.Fatalf("ListImages (empty): %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images, got %d", len(images))
	}

	img := &models.Image{
		ID:         "img-1",
		Name:       "ubuntu",
		Tag:        "latest",
		SizeMB:     300,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}
	if err := db.CreateImage(img); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	images, err = db.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].ID != "img-1" {
		t.Errorf("ID = %q, want %q", images[0].ID, "img-1")
	}
}

func TestImageDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	img := &models.Image{
		ID:         "img-1",
		Name:       "ubuntu",
		Tag:        "latest",
		SizeMB:     300,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}
	if err := db.CreateImage(img); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	if err := db.DeleteImage("img-1"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}

	_, err := db.GetImage("img-1")
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
}

// --- Token CRUD ---

func TestTokenCreate(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	tok := &models.Token{
		ID:        "tok-1",
		Name:      "admin",
		Hash:      "abc123hash",
		CreatedAt: now,
	}

	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	got, err := db.GetTokenByHash("abc123hash")
	if err != nil {
		t.Fatalf("GetTokenByHash: %v", err)
	}
	if got.ID != "tok-1" {
		t.Errorf("ID = %q, want %q", got.ID, "tok-1")
	}
	if got.Name != "admin" {
		t.Errorf("Name = %q, want %q", got.Name, "admin")
	}
}

func TestTokenGetByHashNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.GetTokenByHash("nonexistent-hash")
	if err == nil {
		t.Fatal("expected error for nonexistent hash, got nil")
	}
}

func TestTokenList(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	tokens, err := db.ListTokens()
	if err != nil {
		t.Fatalf("ListTokens (empty): %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens, got %d", len(tokens))
	}

	tok := &models.Token{
		ID:        "tok-1",
		Name:      "admin",
		Hash:      "hash1",
		CreatedAt: now,
	}
	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	tokens, err = db.ListTokens()
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}
}

func TestTokenDelete(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	tok := &models.Token{
		ID:        "tok-1",
		Name:      "admin",
		Hash:      "hash1",
		CreatedAt: now,
	}
	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := db.DeleteToken("tok-1"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}

	_, err := db.GetTokenByHash("hash1")
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
}

func TestTokenCount(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	count, err := db.TokenCount()
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	tok := &models.Token{
		ID:        "tok-1",
		Name:      "admin",
		Hash:      "hash1",
		CreatedAt: now,
	}
	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	count, err = db.TokenCount()
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
}

func TestTokenDeleteExpired(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Create an expired token
	past := now.Add(-1 * time.Hour)
	tok1 := &models.Token{
		ID:        "tok-expired",
		Name:      "expired",
		Hash:      "hash-expired",
		CreatedAt: now,
		ExpiresAt: &past,
	}
	if err := db.CreateToken(tok1); err != nil {
		t.Fatalf("CreateToken (expired): %v", err)
	}

	// Create a non-expired token
	future := now.Add(24 * time.Hour)
	tok2 := &models.Token{
		ID:        "tok-valid",
		Name:      "valid",
		Hash:      "hash-valid",
		CreatedAt: now,
		ExpiresAt: &future,
	}
	if err := db.CreateToken(tok2); err != nil {
		t.Fatalf("CreateToken (valid): %v", err)
	}

	// Create a token with no expiry
	tok3 := &models.Token{
		ID:        "tok-noexpiry",
		Name:      "noexpiry",
		Hash:      "hash-noexpiry",
		CreatedAt: now,
	}
	if err := db.CreateToken(tok3); err != nil {
		t.Fatalf("CreateToken (no expiry): %v", err)
	}

	if err := db.DeleteExpiredTokens(); err != nil {
		t.Fatalf("DeleteExpiredTokens: %v", err)
	}

	count, err := db.TokenCount()
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 tokens remaining, got %d", count)
	}

	// The expired token should be gone
	_, err = db.GetTokenByHash("hash-expired")
	if err == nil {
		t.Fatal("expected expired token to be deleted")
	}

	// The valid one should still be there
	_, err = db.GetTokenByHash("hash-valid")
	if err != nil {
		t.Fatalf("valid token should still exist: %v", err)
	}

	// The no-expiry one should still be there
	_, err = db.GetTokenByHash("hash-noexpiry")
	if err != nil {
		t.Fatalf("no-expiry token should still exist: %v", err)
	}
}

func TestMaxAllocatedNetworkIndex(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	idx, err := db.MaxAllocatedNetworkIndex()
	if err != nil {
		t.Fatalf("MaxAllocatedNetworkIndex (empty): %v", err)
	}
	if idx != 0 {
		t.Errorf("expected 0, got %d", idx)
	}

	for _, tc := range []struct {
		id    string
		index uint32
	}{
		{"m-1", 3},
		{"m-2", 7},
		{"m-3", 5},
	} {
		m := &models.Machine{
			ID:           tc.id,
			Name:         "vm-" + tc.id,
			ImageID:      "img-1",
			State:        models.MachineStateRunning,
			VCPUs:        1,
			MemoryMB:     512,
			NetworkIndex: tc.index,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := db.CreateMachine(m); err != nil {
			t.Fatalf("CreateMachine(%s): %v", tc.id, err)
		}
	}

	idx, err = db.MaxAllocatedNetworkIndex()
	if err != nil {
		t.Fatalf("MaxAllocatedNetworkIndex: %v", err)
	}
	if idx != 7 {
		t.Errorf("expected 7, got %d", idx)
	}
}

func TestTokenWithExpiry(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(1 * time.Hour)

	tok := &models.Token{
		ID:        "tok-1",
		Name:      "temp",
		Hash:      "hash1",
		CreatedAt: now,
		ExpiresAt: &exp,
	}
	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	got, err := db.GetTokenByHash("hash1")
	if err != nil {
		t.Fatalf("GetTokenByHash: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set")
	}
}
