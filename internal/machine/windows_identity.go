package machine

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const (
	windowsHostKeyFile       = "ssh_host_ed25519_key.pub"
	windowsHostKeyMarker     = "kiln-vm-host-key "
	windowsIdentityTimeout   = 10 * time.Minute
	windowsSSHReadyTimeout   = 3 * time.Minute
	windowsGuestPollInterval = 2 * time.Second
	windowsPowerShellPath    = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	windowsComputerNameLimit = 15
)

type windowsGuestAgent interface {
	Command(ctx context.Context, execute string, arguments any, result any) error
	Reboot(ctx context.Context) error
}

type virshGuestAgent struct {
	connectURI string
	domain     string
}

func (a virshGuestAgent) Command(ctx context.Context, execute string, arguments any, result any) error {
	request := map[string]any{"execute": execute}
	if arguments != nil {
		request["arguments"] = arguments
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode guest agent %s: %w", execute, err)
	}
	out, err := libvirtOutput(ctx, a.connectURI, "qemu-agent-command", a.domain, "--timeout", "30", string(payload))
	if err != nil {
		return fmt.Errorf("guest agent %s: %w", execute, err)
	}
	var response struct {
		Return json.RawMessage `json:"return"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return fmt.Errorf("decode guest agent %s response: %w", execute, err)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Return, result); err != nil {
		return fmt.Errorf("decode guest agent %s result: %w", execute, err)
	}
	return nil
}

func (a virshGuestAgent) Reboot(ctx context.Context) error {
	return libvirt(ctx, a.connectURI, "reboot", a.domain, "--mode", "agent")
}

// windowsComputerName returns a NetBIOS-safe name: at most 15 characters of
// letters, digits and hyphens, never all digits.
func windowsComputerName(machine *models.Machine) string {
	suffix := strings.ToLower(strings.ReplaceAll(machine.ID, "-", ""))
	if len(suffix) > 4 {
		suffix = suffix[:4]
	}
	var slug strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(machine.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			slug.WriteByte('-')
			lastHyphen = true
		}
	}
	prefix := slug.String()
	if limit := windowsComputerNameLimit - len(suffix) - 1; len(prefix) > limit {
		prefix = prefix[:limit]
	}
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "kv"
	}
	return prefix + "-" + suffix
}

func windowsIdentityScript(computerName, authorizedKey string) string {
	return strings.NewReplacer(
		"{{COMPUTER_NAME}}", computerName,
		"{{AUTHORIZED_KEY}}", authorizedKey,
		"{{SSH_USER}}", windowsSSHUser,
		"{{HOST_KEY_MARKER}}", windowsHostKeyMarker,
	).Replace(windowsIdentityTemplate)
}

const windowsIdentityTemplate = `$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$computerName = '{{COMPUTER_NAME}}'
$authorizedKey = '{{AUTHORIZED_KEY}}'
$sshDir = Join-Path $env:ProgramData "ssh"

function Set-PrivateAcl {
  param([Parameter(Mandatory=$true)][string]$Path)
  icacls.exe $Path /inheritance:r /grant "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "icacls $Path exited with $LASTEXITCODE" }
}

$adminKeys = Join-Path $sshDir "administrators_authorized_keys"
[IO.File]::WriteAllText($adminKeys, $authorizedKey + [Environment]::NewLine, [Text.Encoding]::ASCII)
Set-PrivateAcl -Path $adminKeys

Stop-Service -Name sshd
Remove-Item -Path (Join-Path $sshDir "ssh_host_*") -Force
& "C:\tools\OpenSSH-Win64\ssh-keygen.exe" -A
if ($LASTEXITCODE -ne 0) { throw "ssh-keygen -A exited with $LASTEXITCODE" }
foreach ($key in Get-ChildItem -Path $sshDir -Filter "ssh_host_*_key") {
  Set-PrivateAcl -Path $key.FullName
}
Start-Service -Name sshd

$bytes = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
Set-LocalUser -Name '{{SSH_USER}}' -Password (ConvertTo-SecureString ([Convert]::ToBase64String($bytes) + "aA1!") -AsPlainText -Force)

$pendingName = (Get-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control\ComputerName\ComputerName").ComputerName
if ($pendingName -ne $computerName) {
  Rename-Computer -NewName $computerName -Force -WarningAction SilentlyContinue
}

$hostKey = (Get-Content -Path (Join-Path $sshDir "ssh_host_ed25519_key.pub") -Raw).Trim()
Write-Output "{{HOST_KEY_MARKER}}$hostKey"
`

// applyWindowsIdentity replaces the identity a Windows VM inherits from its
// prepared image. It authorizes only the VM's own SSH key, regenerates the SSH
// host keys, rotates the account password and renames the computer, then
// reboots and waits for the new name. The returned host key arrives over the
// guest agent channel, so the caller can pin it.
func applyWindowsIdentity(ctx context.Context, agent windowsGuestAgent, computerName, authorizedKey string, poll time.Duration) (ssh.PublicKey, error) {
	if err := waitWindowsGuestAgent(ctx, agent, poll); err != nil {
		return nil, err
	}
	stdout, err := runWindowsGuestPowerShell(ctx, agent, windowsIdentityScript(computerName, authorizedKey), poll)
	if err != nil {
		return nil, fmt.Errorf("apply windows identity: %w", err)
	}
	hostKey, err := parseWindowsHostKey(stdout)
	if err != nil {
		return nil, err
	}
	if err := agent.Reboot(ctx); err != nil {
		return nil, fmt.Errorf("reboot windows guest after identity change: %w", err)
	}
	if err := waitWindowsComputerName(ctx, agent, computerName, poll); err != nil {
		return nil, err
	}
	return hostKey, nil
}

func waitWindowsGuestAgent(ctx context.Context, agent windowsGuestAgent, poll time.Duration) error {
	var lastErr error
	for {
		if lastErr = agent.Command(ctx, "guest-ping", nil, nil); lastErr == nil {
			return nil
		}
		if err := sleepContext(ctx, poll); err != nil {
			return fmt.Errorf("wait for windows guest agent: %w; last error: %w", err, lastErr)
		}
	}
}

func waitWindowsComputerName(ctx context.Context, agent windowsGuestAgent, computerName string, poll time.Duration) error {
	var lastName string
	for {
		var result struct {
			HostName string `json:"host-name"`
		}
		if err := agent.Command(ctx, "guest-get-host-name", nil, &result); err == nil {
			lastName = result.HostName
			if strings.EqualFold(result.HostName, computerName) {
				return nil
			}
		}
		if err := sleepContext(ctx, poll); err != nil {
			return fmt.Errorf("wait for windows computer name %s (last seen %q): %w", computerName, lastName, err)
		}
	}
}

func runWindowsGuestPowerShell(ctx context.Context, agent windowsGuestAgent, script string, poll time.Duration) (string, error) {
	var started struct {
		PID int `json:"pid"`
	}
	if err := agent.Command(ctx, "guest-exec", map[string]any{
		"path":           windowsPowerShellPath,
		"arg":            []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(utf16LE(script))},
		"capture-output": true,
	}, &started); err != nil {
		return "", fmt.Errorf("start powershell: %w", err)
	}
	for {
		var status struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		}
		if err := agent.Command(ctx, "guest-exec-status", map[string]any{"pid": started.PID}, &status); err != nil {
			return "", fmt.Errorf("poll powershell: %w", err)
		}
		if status.Exited {
			stdout, _ := base64.StdEncoding.DecodeString(status.OutData)
			if status.ExitCode != 0 {
				stderr, _ := base64.StdEncoding.DecodeString(status.ErrData)
				return "", fmt.Errorf("powershell exited with %d: %s", status.ExitCode, truncateForError(string(stdout)+string(stderr)))
			}
			return string(stdout), nil
		}
		if err := sleepContext(ctx, poll); err != nil {
			return "", fmt.Errorf("wait for guest process %d: %w", started.PID, err)
		}
	}
}

func parseWindowsHostKey(stdout string) (ssh.PublicKey, error) {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		encoded, ok := strings.CutPrefix(line, windowsHostKeyMarker)
		if !ok {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(encoded))
		if err != nil {
			return nil, fmt.Errorf("parse windows host key: %w", err)
		}
		if key.Type() != ssh.KeyAlgoED25519 {
			return nil, fmt.Errorf("windows host key has type %s, want %s", key.Type(), ssh.KeyAlgoED25519)
		}
		return key, nil
	}
	return nil, fmt.Errorf("windows identity script did not report a host key: %s", truncateForError(stdout))
}

func truncateForError(text string) string {
	text = strings.TrimSpace(text)
	const limit = 2048
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait canceled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func windowsHostKeyPath(dataDir, machineID string) string {
	return filepath.Join(dataDir, "machines", machineID, windowsHostKeyFile)
}

func readWindowsHostKey(dataDir, machineID string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(windowsHostKeyPath(dataDir, machineID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("windows VM %s has no pinned SSH host key; stop and start it to finish setup: %w", machineID, err)
	}
	if err != nil {
		return nil, fmt.Errorf("read pinned windows host key: %w", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse pinned windows host key: %w", err)
	}
	return key, nil
}

func (m *Manager) ensureWindowsIdentity(ctx context.Context, machine *models.Machine, domain string) error {
	if _, err := os.Stat(windowsHostKeyPath(m.cfg.DataDir, machine.ID)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check pinned windows host key: %w", err)
	}
	signer, err := m.windowsSSHSigner(machine)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, windowsIdentityTimeout)
	defer cancel()
	authorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	hostKey, err := applyWindowsIdentity(ctx, virshGuestAgent{connectURI: m.cfg.LibvirtConnectURI, domain: domain}, windowsComputerName(machine), authorizedKey, windowsGuestPollInterval)
	if err != nil {
		return err
	}
	path := windowsHostKeyPath(m.cfg.DataDir, machine.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, ssh.MarshalAuthorizedKey(hostKey), 0644); err != nil {
		return fmt.Errorf("write pinned windows host key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pin windows host key: %w", err)
	}
	return nil
}

func (m *Manager) windowsSSHSigner(machine *models.Machine) (ssh.Signer, error) {
	keyBytes, err := os.ReadFile(filepath.Join(m.cfg.DataDir, "machines", machine.ID, "id_ed25519"))
	if err != nil {
		return nil, fmt.Errorf("read windows ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse windows ssh key: %w", err)
	}
	return signer, nil
}

func windowsSSHClientConfig(signer ssh.Signer, hostKey ssh.PublicKey) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:              windowsSSHUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   ssh.FixedHostKey(hostKey),
		HostKeyAlgorithms: []string{hostKey.Type()},
		Timeout:           10 * time.Second,
	}
}

func (m *Manager) waitWindowsSSH(ctx context.Context, machine *models.Machine) error {
	ctx, cancel := context.WithTimeout(ctx, windowsSSHReadyTimeout)
	defer cancel()
	var lastErr error
	for {
		client, err := m.windowsSSHClient(ctx, machine)
		if err == nil {
			_ = client.Close()
			return nil
		}
		lastErr = err
		if err := sleepContext(ctx, windowsGuestPollInterval); err != nil {
			return fmt.Errorf("wait for windows ssh on %s: %w; last error: %w", machine.IP, err, lastErr)
		}
	}
}
