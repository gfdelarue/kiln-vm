package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	thirdparty "github.com/gfdelarue/kiln-vm/third_party"
)

// Builder handles Dockerfile → ext4 rootfs conversion.
type Builder struct {
	dataDir        string
	buildDir       string
	minFreeMB      int64
	agentBinPath   string
	targetPlatform string
}

const DefaultRootfsSizeMB int64 = 1024

func NewBuilder(dataDir, agentBinPath string) *Builder {
	return &Builder{
		dataDir:      dataDir,
		buildDir:     filepath.Join(dataDir, "build"),
		minFreeMB:    defaultMinFreeMB,
		agentBinPath: agentBinPath,
	}
}

// Build creates an ext4 rootfs from a Dockerfile.
// Steps: docker build -> docker create -> sparse ext4 -> mount -> streamed docker export + extract.
func (b *Builder) Build(ctx context.Context, imageID, name, tag, dockerfile, contextDir string, rootfsSizeMB int64) (string, error) {
	finalDir := filepath.Join(b.dataDir, "images", imageID)
	if err := os.MkdirAll(filepath.Dir(finalDir), 0750); err != nil {
		return "", fmt.Errorf("create image parent dir: %w", err)
	}
	if err := checkMaterializationPreflight(b.buildDir, rootfsSizeMB, b.minFreeMB); err != nil {
		return "", err
	}
	buildParent := filepath.Join(b.buildDir, "images", "staging")
	if err := os.MkdirAll(buildParent, 0750); err != nil {
		return "", fmt.Errorf("create image staging parent: %w", err)
	}
	contextParent := filepath.Join(b.buildDir, "contexts")
	if err := os.MkdirAll(contextParent, 0750); err != nil {
		return "", fmt.Errorf("create image build context parent: %w", err)
	}
	stagingDir, err := os.MkdirTemp(buildParent, imageID+"-")
	if err != nil {
		return "", fmt.Errorf("create image staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	dockerTag := fmt.Sprintf("kiln-vm-build-%s:%s", name, tag)

	buildContext, dockerfilePath, cleanup, err := prepareBuildContext(stagingDir, contextParent, dockerfile, contextDir)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// docker build
	slog.Info("building docker image", "tag", dockerTag)
	cmd := exec.CommandContext(ctx, "docker", dockerBuildArgs(dockerTag, dockerfilePath, buildContext, b.targetPlatform)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build: %w", err)
	}
	defer exec.Command("docker", "image", "rm", "-f", dockerTag).Run()

	// docker create (don't start)
	containerID, err := execOutputCtx(ctx, "docker", "create", dockerTag)
	if err != nil {
		return "", fmt.Errorf("docker create: %w", err)
	}
	defer exec.Command("docker", "rm", "-f", containerID).Run()

	// Materialize container rootfs into a sparse ext4 image without writing an intermediate tar.
	stagedRootfsPath := filepath.Join(stagingDir, "rootfs.ext4")
	if err := containerToExt4(ctx, containerID, stagedRootfsPath, rootfsSizeMB); err != nil {
		return "", fmt.Errorf("container to ext4: %w", err)
	}

	if err := injectRuntime(stagedRootfsPath, b.agentBinPath); err != nil {
		return "", fmt.Errorf("inject runtime: %w", err)
	}

	if err := promoteStagingDir(stagingDir, finalDir); err != nil {
		return "", err
	}
	rootfsPath := filepath.Join(finalDir, "rootfs.ext4")

	slog.Info("image built", "id", imageID, "path", rootfsPath)
	return rootfsPath, nil
}

func (b *Builder) SetBuildDir(buildDir string) {
	if strings.TrimSpace(buildDir) != "" {
		b.buildDir = buildDir
	}
}

func (b *Builder) SetMinFreeMB(minFreeMB int64) {
	b.minFreeMB = minFreeMB
}

func (b *Builder) SetTargetPlatform(platform string) {
	b.targetPlatform = strings.TrimSpace(platform)
}

func (b *Builder) CleanupStaging() {
	_ = os.RemoveAll(filepath.Join(b.buildDir, "images", "staging"))
	_ = os.RemoveAll(filepath.Join(b.buildDir, "contexts"))
}

func dockerBuildArgs(dockerTag, dockerfilePath, buildContext, targetPlatform string) []string {
	args := []string{"build", "-t", dockerTag, "-f", dockerfilePath}
	if strings.TrimSpace(targetPlatform) != "" {
		args = append(args, "--platform", strings.TrimSpace(targetPlatform))
	}
	return append(args, buildContext)
}

func prepareBuildContext(imageDir, tempParent, dockerfile, contextArchive string) (string, string, func(), error) {
	if contextArchive == "" {
		dockerfilePath := filepath.Join(imageDir, "Dockerfile")
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0640); err != nil {
			return "", "", nil, fmt.Errorf("write Dockerfile: %w", err)
		}
		return imageDir, dockerfilePath, func() {}, nil
	}

	buildContext, err := os.MkdirTemp(tempParent, "kiln-vm-build-context-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("create temp build context: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(buildContext)
	}

	if err := unpackBase64Tar(contextArchive, buildContext); err != nil {
		cleanup()
		return "", "", nil, err
	}

	dockerfilePath := filepath.Join(buildContext, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0640); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("write Dockerfile: %w", err)
	}

	return buildContext, dockerfilePath, cleanup, nil
}

func unpackBase64Tar(encoded, targetDir string) error {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode build context: %w", err)
	}

	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read build context: %w", err)
		}

		targetPath := filepath.Join(targetDir, filepath.Clean(header.Name))
		if !isWithin(targetDir, targetPath) {
			return fmt.Errorf("invalid build context path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return fmt.Errorf("create context dir: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
				return fmt.Errorf("create context parent dir: %w", err)
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create context file: %w", err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write context file: %w", err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close context file: %w", err)
			}
		}
	}
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && filepath.Dir(rel) != ".."
}

func containerToExt4(ctx context.Context, containerID, ext4Path string, sizeMB int64) error {
	if sizeMB <= 0 {
		sizeMB = DefaultRootfsSizeMB
	}
	if err := createSparseFile(ext4Path, sizeMB); err != nil {
		return err
	}
	if err := runMaterializerCommand(ctx, "mkfs.ext4", ext4Path, sizeMB, "mkfs.ext4", "-F", ext4Path); err != nil {
		return err
	}

	mountDir := ext4Path + ".mount"
	os.MkdirAll(mountDir, 0750)

	if err := runMaterializerCommand(ctx, "mount", ext4Path, sizeMB, "mount", "-o", "loop", ext4Path, mountDir); err != nil {
		return err
	}
	defer cleanupMountedRootfsDir(mountDir, ext4Path)

	if err := streamContainerToMountedRootfs(ctx, containerID, mountDir, ext4Path, sizeMB); err != nil {
		return err
	}

	return nil
}

func cleanupMountedRootfsDir(mountDir, rootfsPath string) {
	if err := unmountRootfsDir(mountDir); err != nil {
		slog.Warn("leaving rootfs mountpoint in place because unmount failed", "rootfs", rootfsPath, "mount_dir", mountDir, "error", err)
		return
	}
	if err := os.RemoveAll(mountDir); err != nil {
		slog.Warn("remove rootfs mountpoint failed", "rootfs", rootfsPath, "mount_dir", mountDir, "error", err)
	}
}

func unmountRootfsDir(mountDir string) error {
	if out, err := exec.Command("umount", mountDir).CombinedOutput(); err == nil {
		return nil
	} else if lazyOut, lazyErr := exec.Command("umount", "-l", mountDir).CombinedOutput(); lazyErr != nil {
		return fmt.Errorf("umount: %s: %w; lazy umount: %s: %v", out, err, lazyOut, lazyErr)
	}
	return nil
}

func injectRuntime(rootfsPath, agentBinPath string) error {
	mountDir := rootfsPath + ".mount"
	os.MkdirAll(mountDir, 0750)

	if out, err := exec.Command("mount", "-o", "loop", rootfsPath, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %s: %w", out, err)
	}
	defer cleanupMountedRootfsDir(mountDir, rootfsPath)

	if err := os.MkdirAll(filepath.Join(mountDir, "sbin"), 0755); err != nil {
		return fmt.Errorf("create sbin dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mountDir, "sbin", "kiln-vm-init"), []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write kiln-vm-init: %w", err)
	}

	if err := injectAgentBinary(mountDir, agentBinPath); err != nil {
		return err
	}

	if err := ensureOpenSSHHostKeys(mountDir); err != nil {
		return err
	}

	return nil
}

func injectAgentBinary(mountDir, agentBinPath string) error {
	if agentBinPath == "" {
		return fmt.Errorf("kiln-vm agent path is not configured")
	}

	agentBytes, err := os.ReadFile(agentBinPath)
	if err != nil {
		return fmt.Errorf("read kiln-vm-agent binary: %w", err)
	}

	targetPath := filepath.Join(mountDir, "sbin", "kiln-vm-agent")
	if err := os.WriteFile(targetPath, agentBytes, 0755); err != nil {
		return fmt.Errorf("write kiln-vm-agent: %w", err)
	}

	licensePath := filepath.Join(mountDir, filepath.FromSlash(thirdparty.GoXNetGuestLicensePath))
	if err := os.MkdirAll(filepath.Dir(licensePath), 0755); err != nil {
		return fmt.Errorf("create kiln-vm-agent third-party notice dir: %w", err)
	}
	if err := os.WriteFile(licensePath, thirdparty.GoXNetLicense(), 0444); err != nil {
		return fmt.Errorf("write kiln-vm-agent golang.org/x/net notice: %w", err)
	}
	if err := os.Chmod(licensePath, 0444); err != nil {
		return fmt.Errorf("set kiln-vm-agent golang.org/x/net notice permissions: %w", err)
	}
	return nil
}

func ensureOpenSSHHostKeys(mountDir string) error {
	sshdPath := filepath.Join(mountDir, "usr", "sbin", "sshd")
	sshKeygenPath := filepath.Join(mountDir, "usr", "bin", "ssh-keygen")
	if _, err := os.Stat(sshdPath); err != nil {
		return nil
	}
	if _, err := os.Stat(sshKeygenPath); err != nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Join(mountDir, "etc", "ssh"), 0755); err != nil {
		return fmt.Errorf("create ssh config dir: %w", err)
	}

	if out, err := exec.Command("ssh-keygen", "-A", "-f", mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("generate openssh host keys: %s: %w", out, err)
	}
	return nil
}

const initScript = `#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

mount -t proc proc /proc >/dev/null 2>&1 || true
mount -t sysfs sysfs /sys >/dev/null 2>&1 || true
mount -t devtmpfs devtmpfs /dev >/dev/null 2>&1 || true
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts >/dev/null 2>&1 || true
mkdir -p /run /tmp /var/run /root/.ssh
chmod 700 /root/.ssh 2>/dev/null || true
ln -snf /proc/self/fd /dev/fd
ln -snf /proc/self/fd/0 /dev/stdin
ln -snf /proc/self/fd/1 /dev/stdout
ln -snf /proc/self/fd/2 /dev/stderr

mount_cgroup() {
  name="$1"
  mkdir -p "/sys/fs/cgroup/$name"
  mount -t cgroup -o "$name" cgroup "/sys/fs/cgroup/$name" >/dev/null 2>&1 || true
}

mkdir -p /sys/fs/cgroup
mount -t tmpfs cgroup_root /sys/fs/cgroup >/dev/null 2>&1 || true
for subsystem in cpuset cpu cpuacct blkio memory devices freezer net_cls perf_event pids; do
  mount_cgroup "$subsystem"
done

if command -v ip >/dev/null 2>&1; then
  ip link set lo up 2>/dev/null || true
  ip link set eth0 up 2>/dev/null || true
fi

if [ -x /sbin/kiln-vm-agent ]; then
  echo "kiln-vm-init: seeding kernel entropy"
  /sbin/kiln-vm-agent seed-entropy >/dev/console 2>&1 || echo "kiln-vm-init: kernel entropy seed failed"
fi

if [ -x /sbin/kiln-vm-agent ]; then
  echo "kiln-vm-init: starting kiln-vm-agent"
  /sbin/kiln-vm-agent >/dev/console 2>&1 &
fi

run_with_timeout() {
  timeout_s="$1"
  shift
  "$@" >/dev/console 2>&1 &
  cmd_pid=$!
  elapsed=0
  while kill -0 "${cmd_pid}" 2>/dev/null; do
    if [ "${elapsed}" -ge "${timeout_s}" ]; then
      kill "${cmd_pid}" >/dev/null 2>&1 || true
      wait "${cmd_pid}" >/dev/null 2>&1 || true
      return 124
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  wait "${cmd_pid}"
}

ensure_ssh_host_keys() {
  [ -f /etc/ssh/ssh_host_ed25519_key ] || [ -f /etc/ssh/ssh_host_rsa_key ] || return 1
  return 0
}

if command -v sshd >/dev/null 2>&1; then
  if ! ensure_ssh_host_keys && command -v ssh-keygen >/dev/null 2>&1; then
    echo "kiln-vm-init: generating ssh host keys"
    run_with_timeout 10 ssh-keygen -A || echo "kiln-vm-init: ssh host key generation timed out"
  fi
  mkdir -p /run/sshd
  run_with_timeout 5 /usr/sbin/sshd || echo "kiln-vm-init: sshd failed"
elif command -v dropbear >/dev/null 2>&1; then
  mkdir -p /etc/dropbear
  if command -v dropbearkey >/dev/null 2>&1; then
    [ -f /etc/dropbear/dropbear_rsa_host_key ] || dropbearkey -t rsa -f /etc/dropbear/dropbear_rsa_host_key >/dev/null 2>&1 || true
  fi
  mkdir -p /var/run/dropbear
  dropbear -R >/dev/null 2>&1 || true
fi

if [ -x /etc/kiln-vm/boot.sh ]; then
  echo "kiln-vm-init: starting persistent boot hook"
  /etc/kiln-vm/boot.sh >/dev/console 2>&1 &
fi

echo "kiln-vm-init: persistent VM ready"
while true; do
  sleep 3600 &
  wait "$!" || true
done
`

func execOutputCtx(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	// Trim trailing newline
	s := string(out)
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s, nil
}
