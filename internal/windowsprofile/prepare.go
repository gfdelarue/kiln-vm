package windowsprofile

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/isofs"
	"github.com/gfdelarue/kiln-vm/internal/libvirtaccess"
	"github.com/gfdelarue/kiln-vm/internal/libvirtnet"
	"github.com/gfdelarue/kiln-vm/internal/machine"
)

const (
	DefaultAdminUser       = "kiln-vm"
	DefaultVirtIOVersion   = "0.1.302"
	DefaultVirtIOURL       = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso"
	DefaultVirtIOSHA256    = "303f7ae40dad495d6ae474fdc571df58958a4dbc5c37a522d80f9a203867949d"
	DefaultOpenSSHTag      = "10.0.0.0p2-Preview"
	DefaultOpenSSHZip      = "OpenSSH-Win64.zip"
	DefaultOpenSSHSHA256   = "23f50f3458c4c5d0b12217c6a5ddfde0137210a30fa870e98b29827f7b43aba5"
	DefaultProductKey      = "W269N-WFGWX-YVC9B-4J6C9-T83GX"
	installBootKeyWindow   = time.Minute
	installBootKeyInterval = time.Second
	maxInstallDomainStarts = 5
	minIPProvisionGrace    = 20 * time.Minute
	timeoutShutdownReserve = 10 * time.Minute
	sshProbeTimeout        = 30 * time.Second
	sshReadinessGrace      = 15 * time.Minute
)

type PrepareRequest struct {
	ProfileName        string
	MediaPath          string
	VirtIOMediaPath    string
	ProductKey         string
	ProductKeyFile     string
	ActivationMode     string
	Refresh            bool
	DiskSizeMB         int64
	MemoryMB           int
	VCPUs              int
	WorkDir            string
	ConnectURI         string
	Network            string
	NetworkCIDR        string
	WaitTimeout        time.Duration
	WindowsProfileUser string
}

type PreparedImage struct {
	DiskPath       string
	PrivateKeyPath string
	PublicKey      string
	DiskSizeMB     int64
	Username       string
}

type manifest struct {
	ProfileName  string `json:"profile_name"`
	DiskSizeMB   int64  `json:"disk_size_mb"`
	VirtIOSHA256 string `json:"virtio_sha256"`
	OpenSSHTag   string `json:"openssh_tag"`
	Username     string `json:"username"`
	CreatedAt    string `json:"created_at"`
}

func Prepare(ctx context.Context, req PrepareRequest) (*PreparedImage, error) {
	if strings.TrimSpace(req.ProfileName) == "" {
		return nil, fmt.Errorf("profile name is required")
	}
	req.ProfileName = safeName(req.ProfileName)
	req.MediaPath = strings.TrimSpace(req.MediaPath)
	if req.MediaPath == "" {
		return nil, fmt.Errorf("windows installer media path is required")
	}
	if _, err := os.Stat(req.MediaPath); err != nil {
		return nil, fmt.Errorf("stat windows installer media: %w", err)
	}
	req.VirtIOMediaPath = strings.TrimSpace(req.VirtIOMediaPath)
	if req.VirtIOMediaPath != "" {
		if _, err := os.Stat(req.VirtIOMediaPath); err != nil {
			return nil, fmt.Errorf("stat virtio-win media: %w", err)
		}
	}
	if req.DiskSizeMB <= 0 {
		req.DiskSizeMB = 64 * 1024
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 4096
	}
	if req.VCPUs <= 0 {
		req.VCPUs = 2
	}
	if req.WorkDir == "" {
		return nil, fmt.Errorf("work dir is required")
	}
	if req.ConnectURI == "" {
		req.ConnectURI = "qemu:///system"
	}
	if req.Network == "" {
		req.Network = "kiln-vm"
	}
	if req.WaitTimeout <= 0 {
		req.WaitTimeout = 90 * time.Minute
	}
	if req.WindowsProfileUser == "" {
		req.WindowsProfileUser = DefaultAdminUser
	}
	if err := checkHostTools(); err != nil {
		return nil, err
	}
	if err := (libvirtnet.Network{ConnectURI: req.ConnectURI, Name: req.Network, CIDR: req.NetworkCIDR}).Ensure(ctx); err != nil {
		return nil, fmt.Errorf("prepare libvirt network: %w", err)
	}

	profileDir := filepath.Join(req.WorkDir, req.ProfileName)
	baseDisk := filepath.Join(profileDir, "base.qcow2")
	keyPath := filepath.Join(profileDir, "id_ed25519")
	manifestPath := filepath.Join(profileDir, "manifest.json")
	if !req.Refresh && fileExists(baseDisk) && fileExists(keyPath) && fileExists(manifestPath) {
		return &PreparedImage{
			DiskPath:       baseDisk,
			PrivateKeyPath: keyPath,
			DiskSizeMB:     req.DiskSizeMB,
			Username:       req.WindowsProfileUser,
		}, nil
	}

	if err := os.MkdirAll(req.WorkDir, 0750); err != nil {
		return nil, fmt.Errorf("create windows profile dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(req.WorkDir, "cache"), 0750); err != nil {
		return nil, fmt.Errorf("create windows profile cache dir: %w", err)
	}

	staging, err := os.MkdirTemp(req.WorkDir, req.ProfileName+"-prepare-")
	if err != nil {
		return nil, fmt.Errorf("create windows profile staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	keyPair, err := machine.GenerateSSHKeyPair(staging)
	if err != nil {
		return nil, fmt.Errorf("generate windows profile ssh key: %w", err)
	}
	prepareKeyPath := preparePrivateKeyPath(keyPair, keyPath)
	adminPassword, err := randomPassword()
	if err != nil {
		return nil, err
	}
	productKey, err := productKey(req)
	if err != nil {
		return nil, err
	}

	cacheDir := filepath.Join(req.WorkDir, "cache")
	opensshZip := filepath.Join(cacheDir, DefaultOpenSSHZip)
	if err := ensureArtifact(ctx, opensshZip, "https://github.com/PowerShell/Win32-OpenSSH/releases/download/"+DefaultOpenSSHTag+"/"+DefaultOpenSSHZip, DefaultOpenSSHSHA256); err != nil {
		return nil, err
	}
	virtioSource := req.VirtIOMediaPath
	if virtioSource == "" {
		virtioSource = filepath.Join(cacheDir, "virtio-win-"+DefaultVirtIOVersion+".iso")
		if err := ensureArtifact(ctx, virtioSource, DefaultVirtIOURL, DefaultVirtIOSHA256); err != nil {
			return nil, err
		}
	}
	virtioSHA, err := fileSHA256(virtioSource)
	if err != nil {
		return nil, err
	}

	slog.Info("staging windows install media", "profile", req.ProfileName, "media", req.MediaPath)
	configImg := filepath.Join(staging, "config.img")
	payloadISO := filepath.Join(staging, "payload.iso")
	diskPath := filepath.Join(staging, "base.qcow2")
	installMedia := installMedia{
		Windows: filepath.Join(staging, "install.iso"),
		VirtIO:  filepath.Join(staging, "virtio-win.iso"),
	}
	if err := stageISOForLibvirt(ctx, req.MediaPath, installMedia.Windows, req.WorkDir, 0644); err != nil {
		return nil, fmt.Errorf("stage windows installer media for libvirt: %w", err)
	}
	if err := stageISOForLibvirt(ctx, virtioSource, installMedia.VirtIO, req.WorkDir, 0644); err != nil {
		return nil, fmt.Errorf("stage virtio-win media for libvirt: %w", err)
	}
	if err := createConfigSet(ctx, configImg, keyPair.PublicKey, templateValues{
		AdminUser:     req.WindowsProfileUser,
		AdminPassword: adminPassword,
		ProductKey:    productKey,
		ComputerName:  "kiln-vm-win",
	}); err != nil {
		return nil, err
	}
	if err := createPayloadMedia(payloadISO, opensshZip); err != nil {
		return nil, err
	}
	if err := run(ctx, nil, "qemu-img", "create", "-f", "qcow2", diskPath, fmt.Sprintf("%dM", req.DiskSizeMB)); err != nil {
		return nil, err
	}
	if err := os.Chmod(diskPath, 0600); err != nil {
		return nil, fmt.Errorf("chmod windows qcow2: %w", err)
	}
	if err := prepareInstallMediaForLibvirt(diskPath, configImg, payloadISO, installMedia.Windows, installMedia.VirtIO); err != nil {
		return nil, err
	}

	domain := installDomainName()
	mac := randomMAC()
	cleanupDomain := func() {
		_ = virsh(context.Background(), req.ConnectURI, "destroy", domain)
		_ = virsh(context.Background(), req.ConnectURI, "undefine", domain, "--nvram", "--snapshots-metadata")
	}
	defer cleanupDomain()

	slog.Info("starting windows install", "profile", req.ProfileName, "domain", domain, "timeout", req.WaitTimeout.String())
	installStarted := time.Now()
	if err := run(ctx, nil, "virt-install", virtInstallArgs(req, domain, mac, diskPath, configImg, payloadISO, installMedia)...); err != nil {
		return nil, fmt.Errorf("start windows profile install VM: %w", err)
	}
	sendInstallBootKey(ctx, req.ConnectURI, domain)
	if err := waitProvisioned(ctx, req, domain, mac, prepareKeyPath, req.WaitTimeout); err != nil {
		return nil, err
	}
	slog.Info("windows install finished; shutting the install VM down", "profile", req.ProfileName, "domain", domain, "elapsed", time.Since(installStarted).Round(time.Second))
	if err := shutdownDomain(ctx, req.ConnectURI, domain, req.WaitTimeout); err != nil {
		return nil, err
	}
	if err := virsh(ctx, req.ConnectURI, "undefine", domain, "--nvram", "--snapshots-metadata"); err != nil {
		return nil, fmt.Errorf("undefine windows profile install VM: %w", err)
	}

	if err := os.RemoveAll(profileDir); err != nil {
		return nil, fmt.Errorf("remove old windows profile dir: %w", err)
	}
	if err := os.MkdirAll(profileDir, 0750); err != nil {
		return nil, fmt.Errorf("create prepared windows profile dir: %w", err)
	}
	if err := promotePreparedFile(diskPath, baseDisk, 0600); err != nil {
		return nil, fmt.Errorf("promote prepared windows profile disk: %w", err)
	}
	if err := promotePreparedFile(prepareKeyPath, filepath.Join(profileDir, "id_ed25519"), 0600); err != nil {
		return nil, fmt.Errorf("promote prepared windows profile key: %w", err)
	}
	meta := manifest{
		ProfileName:  req.ProfileName,
		DiskSizeMB:   req.DiskSizeMB,
		VirtIOSHA256: virtioSHA,
		OpenSSHTag:   DefaultOpenSSHTag,
		Username:     req.WindowsProfileUser,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal windows profile manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(payload, '\n'), 0640); err != nil {
		return nil, fmt.Errorf("write windows profile manifest: %w", err)
	}
	slog.Info("prepared windows profile", "profile", req.ProfileName, "disk", baseDisk)
	return &PreparedImage{
		DiskPath:       baseDisk,
		PrivateKeyPath: filepath.Join(profileDir, "id_ed25519"),
		PublicKey:      keyPair.PublicKey,
		DiskSizeMB:     req.DiskSizeMB,
		Username:       req.WindowsProfileUser,
	}, nil
}

type templateValues struct {
	AdminUser     string
	AdminPassword string
	ProductKey    string
	ComputerName  string
}

func createConfigSet(ctx context.Context, target string, publicKey string, values templateValues) error {
	tempDir, err := os.MkdirTemp(filepath.Dir(target), "config-staging-")
	if err != nil {
		return fmt.Errorf("create config staging: %w", err)
	}
	defer os.RemoveAll(tempDir)

	renderedAutounattend := renderTemplate(autounattendTemplate, values)
	renderedProvision := renderProvisionTemplate(provisionTemplate)
	files := map[string]string{
		"Autounattend.xml":  renderedAutounattend,
		"SetupComplete.cmd": setupCompleteTemplate,
		"provision.ps1":     renderedProvision,
		"authorized_keys":   strings.TrimSpace(publicKey) + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte(content), 0640); err != nil {
			return fmt.Errorf("write config set file %s: %w", name, err)
		}
	}
	if err := os.WriteFile(target, make([]byte, 1440*1024), 0600); err != nil {
		return fmt.Errorf("create config floppy image: %w", err)
	}
	if err := run(ctx, nil, "mformat", "-i", target, "-f", "1440", "::"); err != nil {
		return err
	}
	dirs := []string{
		"::/$OEM$",
		"::/$OEM$/$$",
		"::/$OEM$/$$/Setup",
		"::/$OEM$/$$/Setup/Scripts",
		"::/$OEM$/$1",
		"::/$OEM$/$1/kiln-vm",
	}
	for _, dir := range dirs {
		if err := run(ctx, nil, "mmd", "-i", target, dir); err != nil {
			return err
		}
	}
	copyPairs := [][2]string{
		{"Autounattend.xml", "::/Autounattend.xml"},
		{"SetupComplete.cmd", "::/SetupComplete.cmd"},
		{"provision.ps1", "::/provision.ps1"},
		{"authorized_keys", "::/authorized_keys"},
		{"SetupComplete.cmd", "::/$OEM$/$$/Setup/Scripts/SetupComplete.cmd"},
		{"provision.ps1", "::/$OEM$/$1/kiln-vm/provision.ps1"},
		{"authorized_keys", "::/$OEM$/$1/kiln-vm/authorized_keys"},
	}
	for _, pair := range copyPairs {
		if err := run(ctx, nil, "mcopy", "-i", target, filepath.Join(tempDir, pair[0]), pair[1]); err != nil {
			return err
		}
	}
	return nil
}

func createPayloadMedia(target, opensshZip string) error {
	tempDir, err := os.MkdirTemp(filepath.Dir(target), "payload-staging-")
	if err != nil {
		return fmt.Errorf("create payload staging: %w", err)
	}
	defer os.RemoveAll(tempDir)
	payloadManifest := map[string]any{
		"openssh": map[string]string{
			"tag":    DefaultOpenSSHTag,
			"file":   DefaultOpenSSHZip,
			"sha256": DefaultOpenSSHSHA256,
		},
	}
	manifestBytes, err := json.MarshalIndent(payloadManifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode payload manifest: %w", err)
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := os.WriteFile(manifestPath, append(manifestBytes, '\n'), 0640); err != nil {
		return fmt.Errorf("write payload manifest: %w", err)
	}
	if err := isofs.Write(target, "KILNVMDATA", []isofs.File{
		{Path: "payload/manifest.json", Source: manifestPath},
		{Path: "payload/" + DefaultOpenSSHZip, Source: opensshZip},
	}); err != nil {
		return fmt.Errorf("create payload media: %w", err)
	}
	return nil
}

func prepareInstallMediaForLibvirt(diskPath string, readOnly ...string) error {
	if err := libvirtaccess.PrepareWritableFile(diskPath); err != nil {
		return fmt.Errorf("prepare windows qcow2 for libvirt: %w", err)
	}
	for _, path := range readOnly {
		if err := libvirtaccess.PrepareReadOnlyFile(path); err != nil {
			return fmt.Errorf("prepare windows install media for libvirt: %w", err)
		}
	}
	return nil
}

func stageISOForLibvirt(ctx context.Context, source, target, workDir string, mode os.FileMode) error {
	if sourceWithinWorkDir(source, workDir) {
		if err := linkFileWithContext(ctx, source, target, mode); err == nil {
			return nil
		}
		_ = os.Remove(target)
	}
	return copyFileWithContext(ctx, source, target, mode)
}

func sourceWithinWorkDir(source, workDir string) bool {
	if strings.TrimSpace(workDir) == "" {
		return false
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return false
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absWorkDir, absSource)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func linkFileWithContext(ctx context.Context, source, target string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("link file canceled: %w", err)
	}
	if err := os.Link(source, target); err != nil {
		return fmt.Errorf("link %s to %s: %w", source, target, err)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("chmod linked file %s: %w", target, err)
	}
	return nil
}

func copyFileWithContext(ctx context.Context, source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open target file %s: %w", target, err)
	}
	defer out.Close()
	buf := make([]byte, 4*1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy file canceled: %w", err)
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return fmt.Errorf("write target file %s: %w", target, err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read source file %s: %w", source, readErr)
		}
	}
	if err := out.Chmod(mode); err != nil {
		return fmt.Errorf("chmod target file %s: %w", target, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync target file %s: %w", target, err)
	}
	return nil
}

func ensureArtifact(ctx context.Context, target, url, expectedSHA string) error {
	if fileExists(target) {
		return verifySHA256(target, expectedSHA)
	}
	tmp := target + ".tmp"
	slog.Info("downloading windows profile artifact", "url", url)
	if err := run(ctx, nil, "curl", "-fL", "--retry", "5", "--retry-all-errors", "--retry-delay", "2", "-C", "-", "-o", tmp, url); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := verifySHA256(tmp, expectedSHA); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("promote downloaded artifact %s: %w", target, err)
	}
	return nil
}

type installMedia struct {
	Windows string
	VirtIO  string
}

func virtInstallArgs(req PrepareRequest, domain, mac, diskPath, configImg, payloadISO string, media installMedia) []string {
	return []string{
		"--connect", req.ConnectURI,
		"--name", domain,
		"--memory", fmt.Sprintf("%d", req.MemoryMB),
		"--vcpus", fmt.Sprintf("%d", req.VCPUs),
		"--cpu", "host-passthrough",
		"--osinfo", "win11",
		"--features", "smm.state=on",
		"--disk", fmt.Sprintf("path=%s,bus=virtio,format=qcow2", diskPath),
		"--cdrom", media.Windows,
		"--disk", fmt.Sprintf("path=%s,device=cdrom,bus=sata,readonly=on", media.VirtIO),
		"--disk", fmt.Sprintf("path=%s,device=cdrom,bus=sata,readonly=on", payloadISO),
		"--disk", fmt.Sprintf("path=%s,device=floppy,readonly=on", configImg),
		"--network", fmt.Sprintf("network=%s,model=virtio,mac=%s", req.Network, mac),
		"--channel", machine.LibvirtGuestAgentChannel,
		"--graphics", "spice,listen=127.0.0.1",
		"--tpm", "backend.type=emulator,backend.version=2.0,model=tpm-crb",
		"--boot", "uefi,loader.secure=yes",
		"--noautoconsole",
	}
}

const installProgressInterval = 5 * time.Minute

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func waitProvisioned(ctx context.Context, req PrepareRequest, domain, mac, keyPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastErr error
	restarts := 0
	var firstIPAt time.Time
	var firstSSHReadyAt time.Time
	ipGrace := ipProvisionGrace(timeout)
	started := time.Now()
	lastReport := started
	for {
		if time.Since(lastReport) >= installProgressInterval {
			lastReport = time.Now()
			slog.Info("waiting for windows install", "domain", domain, "elapsed", time.Since(started).Round(time.Second), "ip_seen", !firstIPAt.IsZero(), "last_error", errorString(lastErr))
		}
		if state, err := domainState(ctx, req.ConnectURI, domain); err == nil && state != "" && state != "running" {
			if shouldRestartInstallDomain(state, restarts) {
				restarts++
				if err := virsh(ctx, req.ConnectURI, "start", domain); err != nil {
					lastErr = fmt.Errorf("restart windows profile install domain after %s: %w", state, err)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("wait for windows profile readiness: %w", ctx.Err())
				case <-time.After(20 * time.Second):
				}
				continue
			}
			return fmt.Errorf("windows profile install domain stopped before readiness (state=%s, restarts=%d): %w", state, restarts, lastErr)
		}
		ip, err := domainIP(ctx, req.ConnectURI, req.Network, domain, mac)
		if err == nil && ip != "" {
			if firstIPAt.IsZero() {
				firstIPAt = time.Now()
			}
			out, err := runPowerShellSSH(ctx, ip, req.WindowsProfileUser, keyPath, windowsReadinessCommand())
			if err == nil && strings.Contains(out, "kiln-vm-ready") {
				return nil
			}
			if err != nil {
				lastErr = err
				if localSSHKeyError(err) {
					return fmt.Errorf("windows profile local SSH key is unavailable during preparation: %w", err)
				}
				if time.Since(firstIPAt) > ipGrace {
					return fmt.Errorf("windows profile SSH/readiness did not complete after IP was visible for %s (operation timeout %s): %w", ipGrace, timeout, lastErr)
				}
			} else {
				if firstSSHReadyAt.IsZero() {
					firstSSHReadyAt = time.Now()
				}
				lastErr = fmt.Errorf("windows profile SSH reachable but not ready: %s", tailForError(out, 4000))
				if time.Since(firstSSHReadyAt) > sshReadinessGrace {
					return fmt.Errorf("windows profile SSH reachable but readiness did not complete after %s: %w", sshReadinessGrace, lastErr)
				}
			}
		} else if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait for windows profile readiness: %w; last error: %v", ctx.Err(), lastErr)
			}
			return fmt.Errorf("wait for windows profile readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func ipProvisionGrace(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 90*time.Minute - timeoutShutdownReserve
	}
	if timeout <= minIPProvisionGrace {
		return timeout
	}
	grace := timeout - timeoutShutdownReserve
	if grace < minIPProvisionGrace {
		return minIPProvisionGrace
	}
	return grace
}

func preparePrivateKeyPath(keyPair *machine.SSHKeyPair, finalKeyPath string) string {
	if keyPair != nil && strings.TrimSpace(keyPair.PrivateKeyPath) != "" {
		return keyPair.PrivateKeyPath
	}
	return finalKeyPath
}

func promotePreparedFile(source, target string, mode os.FileMode) error {
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("promote prepared file %s: %w", target, err)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("chmod prepared file %s: %w", target, err)
	}
	return nil
}

func localSSHKeyError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

func windowsReadinessCommand() string {
	return strings.Join([]string{
		"$ErrorActionPreference = 'Continue'",
		"$complete = Test-Path 'C:\\kiln-vm\\provision.complete'",
		"$agent = (Get-Service -Name QEMU-GA -ErrorAction SilentlyContinue).Status -eq 'Running'",
		"if ($complete -and $agent) { Write-Output 'kiln-vm-ready'; exit 0 }",
		"$provisionLog = Test-Path 'C:\\kiln-vm\\provision.log'",
		"$bootstrapLog = Test-Path 'C:\\kiln-vm\\bootstrap.log'",
		"Write-Output ('kiln-vm-not-ready complete={0} agent={1} provisionLog={2} bootstrapLog={3}' -f $complete,$agent,$provisionLog,$bootstrapLog)",
		"if ($provisionLog) { Write-Output '--- provision.log tail ---'; Get-Content -LiteralPath 'C:\\kiln-vm\\provision.log' -Tail 80 }",
		"if ($bootstrapLog) { Write-Output '--- bootstrap.log tail ---'; Get-Content -LiteralPath 'C:\\kiln-vm\\bootstrap.log' -Tail 80 }",
	}, "\n")
}

func shouldRestartInstallDomain(state string, restarts int) bool {
	normalized := strings.ToLower(strings.TrimSpace(state))
	if restarts >= maxInstallDomainStarts {
		return false
	}
	return normalized == "shut off" || normalized == "shutoff"
}

func tailForError(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[len(value)-maxLen:]
}

func shutdownDomain(ctx context.Context, connectURI, domain string, timeout time.Duration) error {
	_ = virsh(ctx, connectURI, "shutdown", domain)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		state, err := domainState(ctx, connectURI, domain)
		if err == nil && (state == "shut off" || state == "shutoff") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for windows install domain shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func sendInstallBootKey(ctx context.Context, connectURI, domain string) {
	for _, delay := range installBootKeyDelays() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		_ = virsh(ctx, connectURI, "send-key", domain, "KEY_ENTER")
	}
}

func installBootKeyDelays() []time.Duration {
	delays := make([]time.Duration, installBootKeyWindow/installBootKeyInterval)
	for i := range delays {
		delays[i] = installBootKeyInterval
	}
	return delays
}

func domainState(ctx context.Context, connectURI, domain string) (string, error) {
	out, err := virshOutput(ctx, connectURI, "domstate", domain)
	return strings.TrimSpace(out), err
}

func domainIP(ctx context.Context, connectURI, network, domain, mac string) (string, error) {
	out, err := virshOutput(ctx, connectURI, "domifaddr", domain, "--source", "lease")
	if err == nil {
		if ip := parseIPv4(out); ip != "" {
			return ip, nil
		}
	}
	out, err = virshOutput(ctx, connectURI, "net-dhcp-leases", network)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(strings.ToLower(line), strings.ToLower(mac)) {
			continue
		}
		if ip := parseIPv4(line); ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("domain IP not visible in libvirt leases")
}

var ipv4Re = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3})(?:/\d+)?\b`)

func parseIPv4(text string) string {
	for _, match := range ipv4Re.FindAllStringSubmatch(text, -1) {
		ip := net.ParseIP(match[1])
		if ip == nil || ip.To4() == nil {
			continue
		}
		return match[1]
	}
	return ""
}

func runPowerShellSSH(ctx context.Context, ip, user, keyPath, script string) (string, error) {
	if _, err := os.Stat(keyPath); err != nil {
		return "", fmt.Errorf("windows profile ssh key unavailable at %s: %w", keyPath, err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(utf16LE(script)))
	args := []string{
		"-i", keyPath,
		"-p", "22",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=8",
		user + "@" + ip,
		"powershell.exe", "-NoProfile", "-EncodedCommand", encoded,
	}
	probeCtx, cancel := context.WithTimeout(ctx, sshProbeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(probeCtx, "ssh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return stdout.String(), fmt.Errorf("ssh powershell timed out after %s: %s", sshProbeTimeout, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), fmt.Errorf("ssh powershell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func run(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func virsh(ctx context.Context, connectURI string, args ...string) error {
	_, err := virshOutput(ctx, connectURI, args...)
	return err
}

func virshOutput(ctx context.Context, connectURI string, args ...string) (string, error) {
	fullArgs := append([]string{"-c", connectURI}, args...)
	return runOutput(ctx, "virsh", fullArgs...)
}

func checkHostTools() error {
	var missing []string
	for _, tool := range []string{"qemu-img", "virt-install", "virsh", "mformat", "mmd", "mcopy", "curl", "ssh"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("windows profile preparation requires host tools: %s", strings.Join(missing, ", "))
	}
	return nil
}

func renderTemplate(template string, values templateValues) string {
	replacements := map[string]string{
		"{{ADMIN_USER}}":     values.AdminUser,
		"{{ADMIN_PASSWORD}}": values.AdminPassword,
		"{{PRODUCT_KEY}}":    values.ProductKey,
		"{{COMPUTER_NAME}}":  values.ComputerName,
		"{{DRIVER_PATHS}}":   winPEDriverPaths(),
	}
	out := template
	for old, newValue := range replacements {
		out = strings.ReplaceAll(out, old, newValue)
	}
	return out
}

func winPEDriverPaths() string {
	var b strings.Builder
	key := 0
	for _, letter := range "DEFGH" {
		for _, driver := range []string{"viostor", "NetKVM"} {
			key++
			fmt.Fprintf(&b, "        <PathAndCredentials wcm:action=\"add\" wcm:keyValue=\"%d\"><Path>%c:\\%s\\w11\\amd64</Path></PathAndCredentials>\n", key, letter, driver)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderProvisionTemplate(template string) string {
	replacements := map[string]string{
		"{{OPENSSH_TAG}}":    DefaultOpenSSHTag,
		"{{OPENSSH_ZIP}}":    DefaultOpenSSHZip,
		"{{OPENSSH_SHA256}}": DefaultOpenSSHSHA256,
	}
	out := template
	for old, newValue := range replacements {
		out = strings.ReplaceAll(out, old, newValue)
	}
	return out
}

func productKey(req PrepareRequest) (string, error) {
	if strings.TrimSpace(req.ProductKey) != "" {
		return strings.TrimSpace(req.ProductKey), nil
	}
	if strings.TrimSpace(req.ProductKeyFile) != "" {
		data, err := os.ReadFile(req.ProductKeyFile)
		if err != nil {
			return "", fmt.Errorf("read product key file: %w", err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("product key file is empty")
		}
		return key, nil
	}
	mode := strings.TrimSpace(req.ActivationMode)
	if mode == "" || mode == "deactivated" {
		return DefaultProductKey, nil
	}
	return "", fmt.Errorf("activation_mode %q requires product_key or product_key_file", mode)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for sha256 %s: %w", path, err)
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("hash file %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifySHA256(path, expected string) error {
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if actual != strings.ToLower(strings.TrimPrefix(expected, "sha256:")) {
		return fmt.Errorf("sha256 mismatch for %s: expected %s, got %s", path, expected, actual)
	}
	return nil
}

func randomPassword() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789!@$%+="
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate windows admin password: %w", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

func randomMAC() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "52:54:00:6b:69:6c"
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", buf[0], buf[1], buf[2])
}

func installDomainName() string {
	return "kiln-vm-prep-" + shortID()
}

func shortID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func safeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "windows"
	}
	if len(out) > 32 {
		return out[:32]
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func utf16LE(s string) []byte {
	buf := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r > 0xffff {
			r = '?'
		}
		buf = append(buf, byte(r), byte(r>>8))
	}
	return buf
}
