package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	thirdparty "github.com/gfdelarue/kiln-vm/third_party"
)

func TestUnpackBase64TarRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "../escape",
		Typeflag: tar.TypeReg,
		Mode:     0600,
		Size:     int64(len("bad")),
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte("bad")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := unpackBase64Tar(base64.StdEncoding.EncodeToString(buf.Bytes()), t.TempDir())
	if err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

func TestUnpackBase64TarRejectsNestedPathTraversal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "context")
	if err := os.MkdirAll(target, 0750); err != nil {
		t.Fatal(err)
	}
	archive := testTar(t, []tar.Header{{Name: "../outside/file", Typeflag: tar.TypeReg, Mode: 0600}})
	if err := unpackBase64Tar(archive, target); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside")); !os.IsNotExist(err) {
		t.Fatalf("extraction escaped the context: %v", err)
	}
}

func TestUnpackBase64TarKeepsSymlinksButNotWritesThroughThem(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "context")
	if err := os.MkdirAll(target, 0750); err != nil {
		t.Fatal(err)
	}
	archive := testTar(t, []tar.Header{
		{Name: "app.txt", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "app.txt"},
	})
	if err := unpackBase64Tar(archive, target); err != nil {
		t.Fatalf("unpackBase64Tar: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(target, "alias")); err != nil || link != "app.txt" {
		t.Fatalf("alias = %q, %v; want a symlink to app.txt", link, err)
	}

	escape := filepath.Join(dir, "escape")
	if err := os.MkdirAll(escape, 0750); err != nil {
		t.Fatal(err)
	}
	archive = testTar(t, []tar.Header{
		{Name: "up", Typeflag: tar.TypeSymlink, Linkname: escape},
		{Name: "up/file", Typeflag: tar.TypeReg, Mode: 0600},
	})
	if err := unpackBase64Tar(archive, target); err == nil {
		t.Fatal("expected an error writing through a symlink out of the context")
	}
	if entries, _ := os.ReadDir(escape); len(entries) != 0 {
		t.Fatalf("extraction wrote %d entries outside the context", len(entries))
	}
}

func testTar(t *testing.T, headers []tar.Header) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestPrepareBuildContextWritesDockerfileIntoExtractedContext(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "app.txt",
		Typeflag: tar.TypeReg,
		Mode:     0600,
		Size:     int64(len(content)),
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tempDir := t.TempDir()
	buildContext, dockerfilePath, cleanup, err := prepareBuildContext(tempDir, tempDir, "FROM scratch\n", base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("prepareBuildContext: %v", err)
	}
	defer cleanup()

	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(Dockerfile): %v", err)
	}
	if string(dockerfile) != "FROM scratch\n" {
		t.Fatalf("Dockerfile = %q, want FROM scratch", dockerfile)
	}

	appFile, err := os.ReadFile(filepath.Join(buildContext, "app.txt"))
	if err != nil {
		t.Fatalf("ReadFile(app.txt): %v", err)
	}
	if string(appFile) != "hello" {
		t.Fatalf("app.txt = %q, want hello", appFile)
	}
}

func TestRuntimeFingerprintUsesCompatibilityVersion(t *testing.T) {
	if got, want := RuntimeFingerprint(), runtimeCompatibilityFingerprint(8, guestagent.ProtocolVersion, guestagent.ExecDuplexProtocolVersion, thirdparty.GoXNetLicense()); got != want {
		t.Fatalf("RuntimeFingerprint = %s, want %s", got, want)
	}
}

func TestRuntimeFingerprintChangesWithCompatibilityVersion(t *testing.T) {
	license := thirdparty.GoXNetLicense()
	baseline := runtimeCompatibilityFingerprint(1, 1, 1, license)
	if got := runtimeCompatibilityFingerprint(2, 1, 1, license); got == baseline {
		t.Fatalf("runtime compatibility fingerprint did not change with profile runtime compatibility version: %s", got)
	}
	if got := runtimeCompatibilityFingerprint(1, 2, 1, license); got == baseline {
		t.Fatalf("runtime compatibility fingerprint did not change with guest-agent protocol: %s", got)
	}
	if got := runtimeCompatibilityFingerprint(1, 1, 2, license); got == baseline {
		t.Fatalf("runtime compatibility fingerprint did not change with exec duplex protocol: %s", got)
	}
	changedLicense := append(append([]byte(nil), license...), '\n')
	if got := runtimeCompatibilityFingerprint(1, 1, 1, changedLicense); got == baseline {
		t.Fatalf("runtime compatibility fingerprint did not change with the embedded golang.org/x/net notice: %s", got)
	}
}

func TestWriteRuntimeIncludesExactGoXNetNotice(t *testing.T) {
	mountDir := t.TempDir()
	agentPath := writeTestAgent(t)
	licensePath := filepath.Join(mountDir, filepath.FromSlash(thirdparty.GoXNetGuestLicensePath))
	if err := os.MkdirAll(filepath.Dir(licensePath), 0755); err != nil {
		t.Fatalf("MkdirAll(notice): %v", err)
	}
	if err := os.WriteFile(licensePath, []byte("stale notice"), 0600); err != nil {
		t.Fatalf("WriteFile(stale notice): %v", err)
	}

	if err := writeRuntime(openTestRoot(t, mountDir), agentPath); err != nil {
		t.Fatalf("writeRuntime: %v", err)
	}

	got, err := os.ReadFile(licensePath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", thirdparty.GoXNetGuestLicensePath, err)
	}
	if !bytes.Equal(got, thirdparty.GoXNetLicense()) {
		t.Fatal("guest golang.org/x/net notice differs from the embedded source notice")
	}
	info, err := os.Stat(licensePath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", thirdparty.GoXNetGuestLicensePath, err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0444); got != want {
		t.Fatalf("notice mode = %o, want %o", got, want)
	}
}

func TestWriteRuntimeReplacesSymlinksInsteadOfFollowingThem(t *testing.T) {
	dir := t.TempDir()
	hostFile := filepath.Join(dir, "host-file")
	if err := os.WriteFile(hostFile, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	mountDir := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(mountDir, "sbin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostFile, filepath.Join(mountDir, "sbin", "kiln-vm-init")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../host-file", filepath.Join(mountDir, "sbin", "kiln-vm-agent")); err != nil {
		t.Fatal(err)
	}

	if err := writeRuntime(openTestRoot(t, mountDir), writeTestAgent(t)); err != nil {
		t.Fatalf("writeRuntime: %v", err)
	}

	if got, _ := os.ReadFile(hostFile); string(got) != "host" {
		t.Fatalf("host file = %q, want it untouched", got)
	}
	for _, name := range []string{"kiln-vm-init", "kiln-vm-agent"} {
		info, err := os.Lstat(filepath.Join(mountDir, "sbin", name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("sbin/%s: info %v, err %v; want a regular file", name, info, err)
		}
	}
}

func TestWriteRuntimeRejectsDirectorySymlinksOutOfTheRootfs(t *testing.T) {
	dir := t.TempDir()
	hostDir := filepath.Join(dir, "host-dir")
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		t.Fatal(err)
	}
	mountDir := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostDir, filepath.Join(mountDir, "sbin")); err != nil {
		t.Fatal(err)
	}

	if err := writeRuntime(openTestRoot(t, mountDir), writeTestAgent(t)); err == nil {
		t.Fatal("writeRuntime followed sbin out of the rootfs")
	}
	if entries, _ := os.ReadDir(hostDir); len(entries) != 0 {
		t.Fatalf("host dir has %d entries, want none", len(entries))
	}
}

func TestEnsureOpenSSHHostKeysAddsMissingPairsOnly(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	mountDir := t.TempDir()
	for _, name := range []string{"usr/sbin/sshd", "usr/bin/ssh-keygen", "etc/ssh/ssh_host_rsa_key"} {
		path := filepath.Join(mountDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("guest"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureOpenSSHHostKeys(openTestRoot(t, mountDir)); err != nil {
		t.Fatalf("ensureOpenSSHHostKeys: %v", err)
	}

	sshDir := filepath.Join(mountDir, "etc", "ssh")
	if got, _ := os.ReadFile(filepath.Join(sshDir, "ssh_host_rsa_key")); string(got) != "guest" {
		t.Fatalf("existing rsa key = %q, want it kept", got)
	}
	if _, err := os.Stat(filepath.Join(sshDir, "ssh_host_rsa_key.pub")); !os.IsNotExist(err) {
		t.Fatalf("rsa public key was added next to the guest's own private key: %v", err)
	}
	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub"} {
		if _, err := os.Stat(filepath.Join(sshDir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func writeTestAgent(t *testing.T) string {
	t.Helper()
	agentPath := filepath.Join(t.TempDir(), "kiln-vm-agent")
	if err := os.WriteFile(agentPath, []byte("agent-binary"), 0755); err != nil {
		t.Fatalf("WriteFile(kiln-vm-agent): %v", err)
	}
	return agentPath
}

func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestDockerBuildArgsIncludeTargetPlatformWhenProvided(t *testing.T) {
	got := dockerBuildArgs("kiln-vm-build-test:tag", "/tmp/Dockerfile", "/tmp/context", "linux/arm64")
	want := []string{"build", "-t", "kiln-vm-build-test:tag", "-f", "/tmp/Dockerfile", "--platform", "linux/arm64", "/tmp/context"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("dockerBuildArgs = %#v, want %#v", got, want)
	}
}

func TestDockerBuildArgsOmitTargetPlatformByDefault(t *testing.T) {
	got := dockerBuildArgs("kiln-vm-build-test:tag", "/tmp/Dockerfile", "/tmp/context", "")
	want := []string{"build", "-t", "kiln-vm-build-test:tag", "-f", "/tmp/Dockerfile", "/tmp/context"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("dockerBuildArgs = %#v, want %#v", got, want)
	}
}

func TestCreateSparseFileAllocatesLazily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := createSparseFile(path, 512); err != nil {
		t.Fatalf("createSparseFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(rootfs.ext4): %v", err)
	}
	if got, want := info.Size(), int64(512*1024*1024); got != want {
		t.Fatalf("rootfs size = %d, want %d", got, want)
	}
	allocated, err := allocatedBytes(path)
	if err != nil {
		t.Fatalf("allocatedBytes(rootfs.ext4): %v", err)
	}
	if allocated > 4*1024*1024 {
		t.Fatalf("sparse rootfs allocated %d bytes, want <= 4MiB", allocated)
	}
}

func TestMaterializationPreflightRejectsImpossibleMinimumFreeSpace(t *testing.T) {
	dir := t.TempDir()
	err := checkMaterializationPreflight(dir, 128, 1<<40)
	if err == nil {
		t.Fatal("expected preflight error, got nil")
	}
	if !errors.Is(err, apierr.ErrResourceExhausted) {
		t.Fatalf("preflight error is not resource exhausted: %v", err)
	}
	if !strings.Contains(err.Error(), "not enough host disk space") {
		t.Fatalf("preflight error is not actionable: %v", err)
	}
}

func TestKilledMaterializerCommandIsResourceExhausted(t *testing.T) {
	err := runMaterializerCommand(context.Background(), "test-kill", t.TempDir(), 128, "sh", "-c", "kill -9 $$")
	if err == nil {
		t.Fatal("expected killed command error, got nil")
	}
	if !errors.Is(err, apierr.ErrResourceExhausted) {
		t.Fatalf("killed command is not resource exhausted: %v", err)
	}
	var materializationErr *MaterializationError
	if !errors.As(err, &materializationErr) {
		t.Fatalf("expected MaterializationError, got %T", err)
	}
	if !materializationErr.ProcessKilled {
		t.Fatalf("expected ProcessKilled=true: %+v", materializationErr)
	}
	if !strings.Contains(err.Error(), "MemAvailable") || !strings.Contains(err.Error(), "SwapFree") {
		t.Fatalf("killed command error missing host diagnostics: %v", err)
	}
}

func TestCopySparseFilePreservesHoles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "dst.ext4")
	if err := createSparseFile(src, 256); err != nil {
		t.Fatalf("createSparseFile: %v", err)
	}
	f, err := os.OpenFile(src, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("OpenFile(src): %v", err)
	}
	if _, err := f.WriteAt([]byte("kiln-vm-sparse-copy"), 128*1024*1024); err != nil {
		f.Close()
		t.Fatalf("WriteAt(src): %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(src): %v", err)
	}

	if err := hostfs.CopyFilePreserveSparse(context.Background(), src, dst, 0600); err != nil {
		t.Fatalf("CopyFilePreserveSparse: %v", err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("Stat(src): %v", err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat(dst): %v", err)
	}
	if dstInfo.Size() != srcInfo.Size() {
		t.Fatalf("dst size = %d, want %d", dstInfo.Size(), srcInfo.Size())
	}
	data := make([]byte, len("kiln-vm-sparse-copy"))
	copied, err := os.Open(dst)
	if err != nil {
		t.Fatalf("Open(dst): %v", err)
	}
	if _, err := copied.ReadAt(data, 128*1024*1024); err != nil {
		copied.Close()
		t.Fatalf("ReadAt(dst): %v", err)
	}
	copied.Close()
	if string(data) != "kiln-vm-sparse-copy" {
		t.Fatalf("copied data = %q", data)
	}
	allocated, err := allocatedBytes(dst)
	if err != nil {
		t.Fatalf("allocatedBytes(dst): %v", err)
	}
	if allocated > 8*1024*1024 {
		t.Fatalf("copied sparse file allocated %d bytes, want <= 8MiB", allocated)
	}
}

func TestNormalizeRootfsSizeMB(t *testing.T) {
	tests := []struct {
		name    string
		input   int64
		want    int64
		wantErr bool
	}{
		{name: "default", input: 0, want: DefaultRootfsSizeMB},
		{name: "minimum", input: MinRootfsSizeMB, want: MinRootfsSizeMB},
		{name: "maximum", input: MaxRootfsSizeMB, want: MaxRootfsSizeMB},
		{name: "too small", input: MinRootfsSizeMB - 1, wantErr: true},
		{name: "too large", input: MaxRootfsSizeMB + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRootfsSizeMB(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRootfsSizeMB: %v", err)
			}
			if got != tt.want {
				t.Fatalf("size = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInitScriptStartsServicesAndKeepsGuestAlive(t *testing.T) {
	for _, want := range []string{
		"export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"mount -t cgroup -o \"$name\" cgroup \"/sys/fs/cgroup/$name\" >/dev/null 2>&1 || true",
		"for subsystem in cpuset cpu cpuacct blkio memory devices freezer net_cls perf_event pids; do",
		"/sbin/kiln-vm-agent seed-entropy >/dev/console 2>&1 || echo \"kiln-vm-init: kernel entropy seed failed\"",
		"/sbin/kiln-vm-agent >/dev/console 2>&1 &",
		"run_with_timeout 10 ssh-keygen -A",
		"run_with_timeout 5 /usr/sbin/sshd || echo \"kiln-vm-init: sshd failed\"",
		"dropbearkey -t rsa",
		"dropbear -R >/dev/null 2>&1 || true",
		"kiln-vm-init: starting persistent boot hook",
		"echo \"kiln-vm-init: persistent VM ready\"",
		"while true; do",
		"sleep 3600 &",
	} {
		if !strings.Contains(initScript, want) {
			t.Fatalf("kiln-vm init script missing %q", want)
		}
	}
	if strings.Contains(initScript, "poweroff") || strings.Contains(initScript, "launch.sh") {
		t.Fatal("kiln-vm init script must keep the guest running")
	}
}
