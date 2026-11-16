package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	thirdparty "github.com/gfdelarue/kiln-vm/third_party"
)

func TestVerifyGuestRuntimeBinaryRejectsDynamicELF(t *testing.T) {
	path := writeMinimalELF64(t, true)

	err := verifyGuestRuntimeBinary(path, "kiln-vm-agent", "amd64")
	if err == nil {
		t.Fatal("expected dynamic ELF rejection")
	}
	if !strings.Contains(err.Error(), "CGO_ENABLED=0") {
		t.Fatalf("error = %v, want static rebuild guidance", err)
	}
}

func TestVerifyGuestRuntimeBinaryAcceptsStaticELF(t *testing.T) {
	path := writeMinimalELF64(t, false)

	if err := verifyGuestRuntimeBinary(path, "kiln-vm-agent", "amd64"); err != nil {
		t.Fatalf("verifyGuestRuntimeBinary: %v", err)
	}
}

func TestVerifyGuestRuntimeBinaryAcceptsARM64StaticELF(t *testing.T) {
	path := writeMinimalELF64ForMachine(t, false, elf.EM_AARCH64)

	if err := verifyGuestRuntimeBinary(path, "kiln-vm-agent", "arm64"); err != nil {
		t.Fatalf("verifyGuestRuntimeBinary: %v", err)
	}
}

func TestVerifyGuestRuntimeBinaryRejectsArchitectureMismatch(t *testing.T) {
	path := writeMinimalELF64(t, false)

	err := verifyGuestRuntimeBinary(path, "kiln-vm-agent", "arm64")
	if err == nil {
		t.Fatal("expected architecture mismatch")
	}
	if !strings.Contains(err.Error(), "architecture mismatch") {
		t.Fatalf("error = %v, want architecture mismatch", err)
	}
}

func TestDockerPlatformForArch(t *testing.T) {
	for _, tc := range []struct {
		arch     string
		platform string
	}{
		{arch: "amd64", platform: "linux/amd64"},
		{arch: "x86_64", platform: "linux/amd64"},
		{arch: "arm64", platform: "linux/arm64"},
		{arch: "aarch64", platform: "linux/arm64"},
	} {
		got, ok := dockerPlatformForArch(tc.arch)
		if !ok || got != tc.platform {
			t.Fatalf("dockerPlatformForArch(%q) = %q, %v; want %q, true", tc.arch, got, ok, tc.platform)
		}
	}
	if _, ok := dockerPlatformForArch("riscv64"); ok {
		t.Fatal("dockerPlatformForArch(riscv64) ok = true, want false")
	}
}

func TestVerifyMountedRootfsGoXNetNoticeRequiresExactSourceBytes(t *testing.T) {
	mountDir := t.TempDir()
	noticePath := filepath.Join(mountDir, filepath.FromSlash(thirdparty.GoXNetGuestLicensePath))
	if err := os.MkdirAll(filepath.Dir(noticePath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(noticePath, thirdparty.GoXNetLicense(), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := verifyMountedRootfsGoXNetNotice(mountDir); err != nil {
		t.Fatalf("verifyMountedRootfsGoXNetNotice: %v", err)
	}

	mismatched := append(append([]byte(nil), thirdparty.GoXNetLicense()...), '\n')
	if bytes.Equal(mismatched, thirdparty.GoXNetLicense()) {
		t.Fatal("test fixture must differ from source notice")
	}
	if err := os.WriteFile(noticePath, mismatched, 0444); err != nil {
		t.Fatalf("WriteFile(mismatched): %v", err)
	}
	if err := verifyMountedRootfsGoXNetNotice(mountDir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched notice error = %v, want exact-byte rejection", err)
	}
}

func TestSudoOutputOwnerIgnoresSudoEnvWhenNotRoot(t *testing.T) {
	owner, ok, err := sudoOutputOwner(1000, mapGetenv(map[string]string{
		"SUDO_UID": "1000",
		"SUDO_GID": "100",
	}))
	if err != nil {
		t.Fatalf("sudoOutputOwner: %v", err)
	}
	if ok {
		t.Fatalf("sudoOutputOwner ok = true with owner %+v, want false", owner)
	}
}

func TestSudoOutputOwnerParsesRootSudoCaller(t *testing.T) {
	owner, ok, err := sudoOutputOwner(0, mapGetenv(map[string]string{
		"SUDO_UID": "1000",
		"SUDO_GID": "100",
	}))
	if err != nil {
		t.Fatalf("sudoOutputOwner: %v", err)
	}
	if !ok {
		t.Fatal("sudoOutputOwner ok = false, want true")
	}
	if owner.uid != 1000 || owner.gid != 100 {
		t.Fatalf("sudoOutputOwner = %+v, want uid 1000 gid 100", owner)
	}
}

func TestSudoOutputOwnerIgnoresDirectRootWithoutSudoCaller(t *testing.T) {
	owner, ok, err := sudoOutputOwner(0, mapGetenv(nil))
	if err != nil {
		t.Fatalf("sudoOutputOwner: %v", err)
	}
	if ok {
		t.Fatalf("sudoOutputOwner ok = true with owner %+v, want false", owner)
	}
}

func TestSudoOutputOwnerRejectsPartialSudoEnv(t *testing.T) {
	_, _, err := sudoOutputOwner(0, mapGetenv(map[string]string{
		"SUDO_UID": "1000",
	}))
	if err == nil || !strings.Contains(err.Error(), "SUDO_UID and SUDO_GID") {
		t.Fatalf("sudoOutputOwner error = %v, want partial env rejection", err)
	}
}

func TestSudoOutputOwnerRejectsInvalidIDs(t *testing.T) {
	_, _, err := sudoOutputOwner(0, mapGetenv(map[string]string{
		"SUDO_UID": "-1",
		"SUDO_GID": "100",
	}))
	if err == nil || !strings.Contains(err.Error(), "SUDO_UID") {
		t.Fatalf("sudoOutputOwner error = %v, want invalid uid rejection", err)
	}
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func writeMinimalELF64(t *testing.T, dynamic bool) string {
	return writeMinimalELF64ForMachine(t, dynamic, elf.EM_X86_64)
}

func writeMinimalELF64ForMachine(t *testing.T, dynamic bool, machine elf.Machine) string {
	t.Helper()

	const (
		elfHeaderSize    = 64
		programHeaderLen = 56
	)
	data := make([]byte, elfHeaderSize+programHeaderLen+64)
	copy(data[0:], []byte{0x7f, 'E', 'L', 'F'})
	data[4] = 2 // ELFCLASS64
	data[5] = 1 // little endian
	data[6] = 1 // EV_CURRENT
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], uint16(machine))
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[32:], elfHeaderSize)
	binary.LittleEndian.PutUint16(data[52:], elfHeaderSize)
	binary.LittleEndian.PutUint16(data[54:], programHeaderLen)
	binary.LittleEndian.PutUint16(data[56:], 1)

	prog := data[elfHeaderSize:]
	if dynamic {
		interp := []byte("/lib64/ld-linux-x86-64.so.2\x00")
		binary.LittleEndian.PutUint32(prog[0:], 3) // PT_INTERP
		binary.LittleEndian.PutUint32(prog[4:], 4) // read
		binary.LittleEndian.PutUint64(prog[8:], elfHeaderSize+programHeaderLen)
		binary.LittleEndian.PutUint64(prog[32:], uint64(len(interp)))
		binary.LittleEndian.PutUint64(prog[40:], uint64(len(interp)))
		binary.LittleEndian.PutUint64(prog[48:], 1)
		copy(data[elfHeaderSize+programHeaderLen:], interp)
	} else {
		binary.LittleEndian.PutUint32(prog[0:], 1) // PT_LOAD
		binary.LittleEndian.PutUint32(prog[4:], 5) // read+execute
	}

	path := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(path, data, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
