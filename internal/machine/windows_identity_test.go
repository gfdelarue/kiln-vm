package machine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestWindowsComputerNameIsNetBIOSSafeAndPerVM(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	allDigits := regexp.MustCompile(`^[0-9]+$`)
	for _, tc := range []struct {
		name, id, want string
	}{
		{"smoke", "1e1a794e-0000-4000-8000-000000000000", "smoke-1e1a"},
		{"Build Agent #2", "abcd1234-0000-4000-8000-000000000000", "build-agen-abcd"},
		{"very-long-windows-builder", "0f0f0f0f-0000-4000-8000-000000000000", "very-long-0f0f"},
		{"---", "12345678-0000-4000-8000-000000000000", "kv-1234"},
		{"123456789", "98765432-0000-4000-8000-000000000000", "123456789-9876"},
	} {
		got := windowsComputerName(&models.Machine{Name: tc.name, ID: tc.id})
		if got != tc.want {
			t.Errorf("windowsComputerName(%q, %q) = %q, want %q", tc.name, tc.id, got, tc.want)
		}
		if len(got) > windowsComputerNameLimit || !valid.MatchString(got) || allDigits.MatchString(got) {
			t.Errorf("windowsComputerName(%q) = %q is not a valid NetBIOS name", tc.name, got)
		}
	}
	first := windowsComputerName(&models.Machine{Name: "win", ID: "aaaa0000-0000-4000-8000-000000000000"})
	second := windowsComputerName(&models.Machine{Name: "win", ID: "bbbb0000-0000-4000-8000-000000000000"})
	if first == second {
		t.Fatalf("two VMs named win share the computer name %q", first)
	}
}

func TestApplyWindowsIdentityPinsTheReportedHostKeyAfterTheRename(t *testing.T) {
	hostKey := testSSHPublicKey(t)
	agent := &fakeWindowsGuestAgent{
		t:              t,
		pingFailures:   2,
		pendingPolls:   1,
		stdout:         "ssh-keygen: generating new host keys: RSA ECDSA ED25519\r\n" + windowsHostKeyMarker + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))) + " system@kiln-vm-win\r\n",
		nameBefore:     "KILN-VM-WIN",
		nameAfterBoots: "SMOKE-1E1A",
	}
	got, err := applyWindowsIdentity(context.Background(), agent, "smoke-1e1a", "ssh-ed25519 AAAAvmkey", time.Millisecond)
	if err != nil {
		t.Fatalf("applyWindowsIdentity: %v", err)
	}
	if !bytes.Equal(got.Marshal(), hostKey.Marshal()) {
		t.Fatalf("pinned host key = %s, want the key the guest reported", ssh.FingerprintSHA256(got))
	}
	for _, want := range []string{
		"$computerName = 'smoke-1e1a'",
		"$authorizedKey = 'ssh-ed25519 AAAAvmkey'",
		"[IO.File]::WriteAllText($adminKeys, $authorizedKey + [Environment]::NewLine",
		`Remove-Item -Path (Join-Path $sshDir "ssh_host_*") -Force`,
		`ssh-keygen.exe" -A`,
		"Set-LocalUser -Name 'kiln-vm' -Password",
		"Rename-Computer -NewName $computerName",
		`Write-Output "kiln-vm-host-key $hostKey"`,
	} {
		if !strings.Contains(agent.script, want) {
			t.Errorf("identity script missing %q:\n%s", want, agent.script)
		}
	}
	if want := []string{"guest-ping", "guest-ping", "guest-ping", "guest-exec", "guest-exec-status", "guest-exec-status", "reboot", "guest-get-host-name", "guest-get-host-name"}; strings.Join(agent.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("guest agent calls = %v, want %v", agent.calls, want)
	}
}

func TestApplyWindowsIdentityStopsWhenTheScriptFails(t *testing.T) {
	agent := &fakeWindowsGuestAgent{
		t:        t,
		exitCode: 1,
		stderr:   "Stop-Service : Cannot find any service with service name 'sshd'.",
	}
	_, err := applyWindowsIdentity(context.Background(), agent, "smoke-1e1a", "ssh-ed25519 AAAAvmkey", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "powershell exited with 1") || !strings.Contains(err.Error(), "Cannot find any service") {
		t.Fatalf("applyWindowsIdentity error = %v, want the script's exit code and stderr", err)
	}
	for _, call := range agent.calls {
		if call == "reboot" {
			t.Fatalf("guest rebooted after the identity script failed: %v", agent.calls)
		}
	}
}

func TestApplyWindowsIdentityTimesOutWithoutTheNewName(t *testing.T) {
	agent := &fakeWindowsGuestAgent{
		t:              t,
		stdout:         windowsHostKeyMarker + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testSSHPublicKey(t)))),
		nameBefore:     "KILN-VM-WIN",
		nameAfterBoots: "KILN-VM-WIN",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := applyWindowsIdentity(ctx, agent, "smoke-1e1a", "ssh-ed25519 AAAAvmkey", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `last seen "KILN-VM-WIN"`) {
		t.Fatalf("applyWindowsIdentity error = %v, want a deadline naming the last computer name", err)
	}
}

func TestParseWindowsHostKeyRequiresAnEd25519Key(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWindowsHostKey(windowsHostKeyMarker + string(ssh.MarshalAuthorizedKey(rsaPub))); err == nil || !strings.Contains(err.Error(), "ssh-rsa") {
		t.Fatalf("parseWindowsHostKey(rsa) error = %v, want a key type error", err)
	}
	if _, err := parseWindowsHostKey("ssh-keygen: generating new host keys\r\n"); err == nil || !strings.Contains(err.Error(), "did not report a host key") {
		t.Fatalf("parseWindowsHostKey(no marker) error = %v, want a missing key error", err)
	}
}

func TestWindowsSSHClientConfigRejectsAnyOtherHostKey(t *testing.T) {
	clientKey := testSSHSigner(t)
	pinned := testSSHSigner(t)
	other := testSSHSigner(t)

	if err := dialTestSSHServer(t, pinned, windowsSSHClientConfig(clientKey, pinned.PublicKey())); err != nil {
		t.Fatalf("dial with the pinned host key: %v", err)
	}
	err := dialTestSSHServer(t, other, windowsSSHClientConfig(clientKey, pinned.PublicKey()))
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("dial with another host key error = %v, want a host key mismatch", err)
	}
}

func TestReadWindowsHostKeyExplainsAMissingPin(t *testing.T) {
	dataDir := t.TempDir()
	_, err := readWindowsHostKey(dataDir, "vm-1")
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "stop and start it") {
		t.Fatalf("readWindowsHostKey error = %v, want a missing pin with a fix", err)
	}
	key := testSSHPublicKey(t)
	if err := os.MkdirAll(filepath.Join(dataDir, "machines", "vm-1"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(windowsHostKeyPath(dataDir, "vm-1"), ssh.MarshalAuthorizedKey(key), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := readWindowsHostKey(dataDir, "vm-1")
	if err != nil || !bytes.Equal(got.Marshal(), key.Marshal()) {
		t.Fatalf("readWindowsHostKey = %v, %v, want the pinned key", got, err)
	}
}

type fakeWindowsGuestAgent struct {
	t              *testing.T
	calls          []string
	pingFailures   int
	pendingPolls   int
	statusPolls    int
	exitCode       int
	stdout         string
	stderr         string
	script         string
	nameBefore     string
	nameAfterBoots string
	rebooted       bool
	downPolls      int
}

func (a *fakeWindowsGuestAgent) Command(_ context.Context, execute string, arguments any, result any) error {
	a.calls = append(a.calls, execute)
	switch execute {
	case "guest-ping":
		if a.pingFailures > 0 {
			a.pingFailures--
			return errors.New("QEMU guest agent is not connected")
		}
		return nil
	case "guest-exec":
		args := arguments.(map[string]any)
		if args["path"] != windowsPowerShellPath || args["capture-output"] != true {
			a.t.Fatalf("guest-exec arguments = %v", args)
		}
		argv := args["arg"].([]string)
		if len(argv) != 4 || argv[2] != "-EncodedCommand" {
			a.t.Fatalf("guest-exec argv = %v", argv)
		}
		a.script = decodeUTF16LEBase64(a.t, argv[3])
		return decodeInto(a.t, `{"pid":4864}`, result)
	case "guest-exec-status":
		if pid := arguments.(map[string]any)["pid"]; pid != 4864 {
			a.t.Fatalf("guest-exec-status pid = %v", pid)
		}
		a.statusPolls++
		if a.statusPolls <= a.pendingPolls {
			return decodeInto(a.t, `{"exited":false}`, result)
		}
		status, _ := json.Marshal(map[string]any{
			"exited":   true,
			"exitcode": a.exitCode,
			"out-data": base64.StdEncoding.EncodeToString([]byte(a.stdout)),
			"err-data": base64.StdEncoding.EncodeToString([]byte(a.stderr)),
		})
		return decodeInto(a.t, string(status), result)
	case "guest-get-host-name":
		name := a.nameBefore
		if a.rebooted {
			if a.downPolls > 0 {
				a.downPolls--
				return errors.New("QEMU guest agent is not connected")
			}
			name = a.nameAfterBoots
		}
		return decodeInto(a.t, `{"host-name":"`+name+`"}`, result)
	}
	a.t.Fatalf("unexpected guest agent command %s", execute)
	return nil
}

func (a *fakeWindowsGuestAgent) Reboot(context.Context) error {
	a.calls = append(a.calls, "reboot")
	a.rebooted = true
	a.downPolls = 1
	return nil
}

func decodeInto(t *testing.T, data string, result any) error {
	t.Helper()
	if err := json.Unmarshal([]byte(data), result); err != nil {
		t.Fatalf("decode fake guest agent result: %v", err)
	}
	return nil
}

func decodeUTF16LEBase64(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("decode -EncodedCommand: %v", err)
	}
	units := make([]rune, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		units = append(units, rune(raw[i])|rune(raw[i+1])<<8)
	}
	return string(units)
}

func testSSHSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testSSHPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	return testSSHSigner(t).PublicKey()
}

func dialTestSSHServer(t *testing.T, hostKey ssh.Signer, config *ssh.ClientConfig) error {
	t.Helper()
	serverConfig := &ssh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(hostKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, chans, reqs, err := ssh.NewServerConn(conn, serverConfig)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for newChannel := range chans {
			_ = newChannel.Reject(ssh.Prohibited, "test server")
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), config)
	if err != nil {
		return fmt.Errorf("dial test server: %w", err)
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("close test client: %w", err)
	}
	return nil
}
