package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/vm"
	thirdparty "github.com/gfdelarue/kiln-vm/third_party"
)

type metadata struct {
	SchemaVersion    int    `json:"schema_version"`
	Profile          string `json:"profile"`
	ProfileVersion   int    `json:"profile_version"`
	Fingerprint      string `json:"fingerprint"`
	Arch             string `json:"arch"`
	RootfsSizeMB     int64  `json:"rootfs_size_mb"`
	Artifact         string `json:"artifact"`
	CompressedSHA256 string `json:"compressed_sha256"`
	RootfsSHA256     string `json:"rootfs_sha256"`
	LayerMediaType   string `json:"layer_media_type"`
	CreatedAt        string `json:"created_at"`
}

func main() {
	var (
		profileName  = flag.String("profile", "alpine", "VM profile name")
		rootfsSizeMB = flag.Int64("rootfs-size-mb", 0, "Rootfs size in MiB")
		outPath      = flag.String("out", "", "Output rootfs.ext4.gz path")
		metadataPath = flag.String("metadata", "", "Output metadata JSON path")
		agentBin     = flag.String("agent-bin", "", "kiln-vm-agent binary path")
		arch         = flag.String("arch", runtime.GOARCH, "Target architecture label")
		platform     = flag.String("platform", "", "Docker target platform, default derived from --arch")
		timeout      = flag.Duration("timeout", 45*time.Minute, "Build timeout")
		fingerprint  = flag.Bool("fingerprint", false, "Print profile fingerprint JSON and exit without building a rootfs")
	)
	flag.Parse()

	profile, err := vm.GetProfile(*profileName)
	if err != nil {
		fatalf("%v", err)
	}
	if *rootfsSizeMB == 0 {
		*rootfsSizeMB = profile.DefaultDiskSizeMB
	}
	if err := verifyGuestRuntimeBinary(*agentBin, "kiln-vm-agent", *arch); err != nil {
		fatalf("%v", err)
	}
	runtimeFingerprint := image.RuntimeFingerprint()
	if *fingerprint {
		writeFingerprint(profile, runtimeFingerprint, *arch)
		return
	}
	if *outPath == "" || *metadataPath == "" {
		fatalf("--out and --metadata are required")
	}

	workDir, err := os.MkdirTemp("", "kiln-vm-profile-artifact-")
	if err != nil {
		fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(workDir)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	builder := image.NewBuilder(workDir, *agentBin)
	targetPlatform := *platform
	if targetPlatform == "" {
		var ok bool
		targetPlatform, ok = dockerPlatformForArch(*arch)
		if !ok {
			fatalf("unsupported profile artifact architecture %q", *arch)
		}
	}
	builder.SetTargetPlatform(targetPlatform)
	rootfsPath, _, err := builder.Build(
		ctx,
		uuid.NewString(),
		"vm-profile-"+profile.Name,
		fmt.Sprintf("v%d-%dmb", profile.Version, *rootfsSizeMB),
		profile.Dockerfile,
		"",
		*rootfsSizeMB,
	)
	if err != nil {
		fatalf("build profile rootfs: %v", err)
	}
	if err := verifyRootfsContract(rootfsPath, *arch); err != nil {
		fatalf("%v", err)
	}

	rootfsDigest, compressedDigest, err := gzipRootfs(rootfsPath, *outPath)
	if err != nil {
		fatalf("compress profile rootfs: %v", err)
	}
	meta := metadata{
		SchemaVersion:    1,
		Profile:          profile.Name,
		ProfileVersion:   profile.Version,
		Fingerprint:      vm.CatalogFingerprint(profile, runtimeFingerprint),
		Arch:             *arch,
		RootfsSizeMB:     *rootfsSizeMB,
		Artifact:         filepath.Base(*outPath),
		CompressedSHA256: "sha256:" + compressedDigest,
		RootfsSHA256:     "sha256:" + rootfsDigest,
		LayerMediaType:   image.OCIRootfsGzipMediaType,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		fatalf("encode metadata: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*metadataPath), 0750); err != nil {
		fatalf("create metadata dir: %v", err)
	}
	if err := os.WriteFile(*metadataPath, append(data, '\n'), 0644); err != nil {
		fatalf("write metadata: %v", err)
	}
	if err := handoffOutputOwnership(*outPath, *metadataPath); err != nil {
		fatalf("%v", err)
	}
}

func writeFingerprint(profile vm.Profile, runtimeFingerprint, arch string) {
	data, err := json.MarshalIndent(struct {
		Profile        string `json:"profile"`
		ProfileVersion int    `json:"profile_version"`
		Fingerprint    string `json:"fingerprint"`
		Arch           string `json:"arch"`
	}{
		Profile:        profile.Name,
		ProfileVersion: profile.Version,
		Fingerprint:    vm.CatalogFingerprint(profile, runtimeFingerprint),
		Arch:           arch,
	}, "", "  ")
	if err != nil {
		fatalf("encode fingerprint: %v", err)
	}
	fmt.Println(string(data))
}

func gzipRootfs(rootfsPath, outPath string) (string, string, error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0750); err != nil {
		return "", "", fmt.Errorf("create output directory: %w", err)
	}
	in, err := os.Open(rootfsPath)
	if err != nil {
		return "", "", fmt.Errorf("open rootfs: %w", err)
	}
	defer in.Close()
	out, err := os.Create(outPath)
	if err != nil {
		return "", "", fmt.Errorf("create compressed rootfs: %w", err)
	}
	defer out.Close()

	rootfsHash := sha256.New()
	compressedHash := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(out, compressedHash))
	if _, err := io.Copy(gw, io.TeeReader(in, rootfsHash)); err != nil {
		_ = gw.Close()
		return "", "", fmt.Errorf("gzip rootfs: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", "", fmt.Errorf("finish gzip rootfs: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", "", fmt.Errorf("sync compressed rootfs: %w", err)
	}
	return hex.EncodeToString(rootfsHash.Sum(nil)), hex.EncodeToString(compressedHash.Sum(nil)), nil
}

func verifyGuestRuntimeBinary(path, label, arch string) error {
	if path == "" {
		return fmt.Errorf("%s path is required", label)
	}
	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s must be a Linux ELF executable for guest injection: %w", label, err)
	}
	defer file.Close()

	expectedMachine, ok := elfMachineForArch(arch)
	if !ok {
		return fmt.Errorf("unsupported profile artifact architecture %q", arch)
	}
	if file.FileHeader.Machine != expectedMachine {
		return fmt.Errorf("%s architecture mismatch: got %s, want %s for %s", label, file.FileHeader.Machine, expectedMachine, arch)
	}
	for _, prog := range file.Progs {
		if prog.Type == elf.PT_INTERP {
			return fmt.Errorf("%s must be statically linked for distro-independent guest profiles; rebuild it with CGO_ENABLED=0", label)
		}
	}
	return nil
}

func elfMachineForArch(arch string) (elf.Machine, bool) {
	switch arch {
	case "amd64", "x86_64":
		return elf.EM_X86_64, true
	case "arm64", "aarch64":
		return elf.EM_AARCH64, true
	default:
		return 0, false
	}
}

func dockerPlatformForArch(arch string) (string, bool) {
	switch arch {
	case "amd64", "x86_64":
		return "linux/amd64", true
	case "arm64", "aarch64":
		return "linux/arm64", true
	default:
		return "", false
	}
}

func verifyRootfsContract(rootfsPath, arch string) error {
	expectedMachine, ok := elfMachineForArch(arch)
	if !ok {
		return fmt.Errorf("unsupported profile artifact architecture %q", arch)
	}

	mountDir, err := os.MkdirTemp("", "kiln-vm-profile-rootfs-")
	if err != nil {
		return fmt.Errorf("create rootfs inspection mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if out, err := exec.Command("mount", "-o", "loop,ro", rootfsPath, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount profile rootfs for contract verification: %s: %w", out, err)
	}
	defer unmountRootfs(mountDir)

	if err := verifyMountedRootfsArchitecture(mountDir, expectedMachine, arch); err != nil {
		return err
	}
	return verifyMountedRootfsGoXNetNotice(mountDir)
}

func verifyMountedRootfsArchitecture(mountDir string, expectedMachine elf.Machine, arch string) error {
	for _, candidate := range []string{
		"bin/sh",
		"bin/busybox",
		"usr/bin/env",
		"usr/bin/bash",
	} {
		candidatePath := filepath.Join(mountDir, candidate)
		if _, err := os.Stat(candidatePath); err != nil {
			continue
		}
		file, err := elf.Open(candidatePath)
		if err != nil {
			continue
		}
		machine := file.FileHeader.Machine
		_ = file.Close()
		if machine != expectedMachine {
			return fmt.Errorf("profile rootfs architecture mismatch: %s is %s, want %s for %s", candidate, machine, expectedMachine, arch)
		}
		return nil
	}
	return fmt.Errorf("profile rootfs architecture could not be verified: no inspectable ELF executable found")
}

func verifyMountedRootfsGoXNetNotice(mountDir string) error {
	noticePath := filepath.Join(mountDir, filepath.FromSlash(thirdparty.GoXNetGuestLicensePath))
	info, err := os.Lstat(noticePath)
	if err != nil {
		return fmt.Errorf("profile rootfs missing kiln-vm-agent golang.org/x/net notice: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("profile rootfs kiln-vm-agent golang.org/x/net notice is not a regular file")
	}
	got, err := os.ReadFile(noticePath)
	if err != nil {
		return fmt.Errorf("read profile rootfs kiln-vm-agent golang.org/x/net notice: %w", err)
	}
	if !bytes.Equal(got, thirdparty.GoXNetLicense()) {
		return fmt.Errorf("profile rootfs kiln-vm-agent golang.org/x/net notice does not match the source notice")
	}
	return nil
}

func unmountRootfs(mountDir string) {
	if out, err := exec.Command("umount", mountDir).CombinedOutput(); err != nil {
		_, _ = exec.Command("umount", "-l", mountDir).CombinedOutput()
		fmt.Fprintf(os.Stderr, "warning: failed to unmount %s: %s: %v\n", mountDir, out, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
