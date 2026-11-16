package machine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	imagepkg "github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/libvirtaccess"
	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/libvirtnet"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/naming"
)

const (
	libvirtBackend = "libvirt-qemu"
	windowsGuestOS = "windows"
	windowsSSHUser = "kiln-vm"
)

func (m *Manager) createLibvirtMachine(ctx context.Context, req *models.CreateMachineRequest, img *models.Image) (*models.Machine, error) {
	spec, err := libvirtdomain.Normalize(req.Libvirt)
	if err != nil {
		return nil, fmt.Errorf("normalize libvirt domain for machine %s: %w", req.Name, err)
	}
	if !imagepkg.IsBootableQCOW2BootMode(img.BootMode) {
		return nil, &apierr.ValidationError{
			Field:   "image_id",
			Message: "libvirt-qemu machines require a bootable qcow2 image",
		}
	}
	spec.Network.Source = libvirtNetworkSource(m.cfg.LibvirtNetwork, spec.Network)
	if err := libvirtdomain.Validate(spec); err != nil {
		return nil, fmt.Errorf("validate libvirt domain for machine %s: %w", req.Name, err)
	}

	now := time.Now().UTC()
	machine := &models.Machine{
		ID:        uuidString(),
		Name:      req.Name,
		Backend:   req.Backend,
		GuestOS:   req.GuestOS,
		Libvirt:   spec,
		ImageID:   img.ID,
		State:     models.MachineStateStopped,
		VCPUs:     req.VCPUs,
		CPU:       *req.CPU,
		MemoryMB:  req.MemoryMB,
		CreatedAt: now,
		UpdatedAt: now,
	}
	machineDir := filepath.Join(m.cfg.DataDir, "machines", machine.ID)
	if err := os.MkdirAll(machineDir, 0750); err != nil {
		return nil, fmt.Errorf("create libvirt machine dir: %w", err)
	}
	srcDisk := img.RootfsPath
	dstDisk := filepath.Join(machineDir, "disk.qcow2")
	if err := createLibvirtQCOW2Overlay(ctx, srcDisk, dstDisk); err != nil {
		_ = os.RemoveAll(machineDir)
		return nil, fmt.Errorf("create libvirt qcow2 overlay: %w", err)
	}
	if _, err := GenerateSSHKeyPair(machineDir); err != nil {
		_ = os.RemoveAll(machineDir)
		return nil, fmt.Errorf("generate windows ssh key: %w", err)
	}
	if err := m.db.CreateMachine(machine); err != nil {
		_ = os.RemoveAll(machineDir)
		return nil, fmt.Errorf("save libvirt machine: %w", err)
	}
	started, err := m.startLibvirtMachine(ctx, machine)
	if err != nil {
		return nil, err
	}
	return started, nil
}

func (m *Manager) startLibvirtMachine(ctx context.Context, machine *models.Machine) (*models.Machine, error) {
	spec, err := libvirtdomain.Normalize(machine.Libvirt)
	if err != nil {
		return nil, fmt.Errorf("normalize libvirt domain for machine %s: %w", machine.Name, err)
	}
	spec.Network.Source = libvirtNetworkSource(m.cfg.LibvirtNetwork, spec.Network)
	if err := libvirtdomain.Validate(spec); err != nil {
		return nil, fmt.Errorf("validate libvirt domain for machine %s: %w", machine.Name, err)
	}
	if err := m.checkResourceAdmission(machine.ID, &models.CreateMachineRequest{
		Name:     machine.Name,
		Backend:  machine.Backend,
		GuestOS:  machine.GuestOS,
		Libvirt:  spec,
		ImageID:  machine.ImageID,
		VCPUs:    machine.VCPUs,
		CPU:      &machine.CPU,
		MemoryMB: machine.MemoryMB,
	}); err != nil {
		return nil, err
	}
	machineDir := filepath.Join(m.cfg.DataDir, "machines", machine.ID)
	diskPath := filepath.Join(machineDir, "disk.qcow2")
	if _, err := os.Stat(diskPath); err != nil {
		return nil, fmt.Errorf("libvirt qcow2 not found: %w", err)
	}
	if err := libvirtaccess.PrepareWritableFile(diskPath); err != nil {
		return nil, fmt.Errorf("prepare libvirt qcow2: %w", err)
	}
	if err := m.libvirtNetwork(spec.Network.Source).Ensure(ctx); err != nil {
		return nil, fmt.Errorf("prepare libvirt network: %w", err)
	}
	domain := libvirtDomainName(machine)
	if err := m.db.UpdateMachineState(machine.ID, models.MachineStateCreating, ""); err != nil {
		return nil, fmt.Errorf("update libvirt machine state: %w", err)
	}
	if libvirtDomainExists(ctx, m.cfg.LibvirtConnectURI, domain) {
		state, _ := libvirtDomainState(ctx, m.cfg.LibvirtConnectURI, domain)
		if state != "running" {
			if err := libvirt(ctx, m.cfg.LibvirtConnectURI, "start", domain); err != nil {
				_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
				return nil, fmt.Errorf("start libvirt domain: %w", err)
			}
		}
	} else {
		args := libvirtVirtInstallArgs(m.cfg.LibvirtConnectURI, domain, diskPath, machine, spec)
		if err := runHost(ctx, nil, "virt-install", args...); err != nil {
			_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
			return nil, fmt.Errorf("create libvirt domain: %w", err)
		}
	}
	ip, err := waitLibvirtDomainIP(ctx, m.cfg.LibvirtConnectURI, spec.Network.Source, domain, 2*time.Minute)
	if err != nil {
		_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
		return nil, fmt.Errorf("wait for libvirt domain IP: %w", err)
	}
	if err := m.db.UpdateMachineNetwork(machine.ID, ip, "", ""); err != nil {
		_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
		return nil, fmt.Errorf("update libvirt machine IP: %w", err)
	}
	machine.Libvirt = spec
	machine.IP = ip
	if err := m.ensureWindowsIdentity(ctx, machine, domain); err != nil {
		_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
		return nil, fmt.Errorf("set up windows identity: %w", err)
	}
	if err := m.waitWindowsSSH(ctx, machine); err != nil {
		_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, err.Error())
		return nil, err
	}
	machine.State = models.MachineStateRunning
	machine.Error = ""
	if err := m.db.UpdateMachineState(machine.ID, models.MachineStateRunning, ""); err != nil {
		return nil, fmt.Errorf("update libvirt machine state running: %w", err)
	}
	m.runStartHooks(machine.ID)
	return machine, nil
}

const (
	LibvirtGuestAgentChannel = "unix,target.type=virtio,name=org.qemu.guest_agent.0"
	libvirtShutdownTimeout   = 2 * time.Minute
)

func (m *Manager) stopLibvirtMachine(id string) error {
	machine, err := m.getMachine(id)
	if err != nil {
		return err
	}
	if machine.State == models.MachineStateStopped {
		return nil
	}
	domain := libvirtDomainName(machine)
	_ = m.db.UpdateMachineState(machine.ID, models.MachineStateStopping, "")
	if err := shutdownLibvirtDomain(context.Background(), m.cfg.LibvirtConnectURI, domain, libvirtShutdownTimeout); err != nil {
		return err
	}
	_ = m.db.UpdateMachineNetwork(machine.ID, "", "", "")
	if err := m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, ""); err != nil {
		return fmt.Errorf("update libvirt machine stopped: %w", err)
	}
	m.runStopHooks(machine.ID)
	return nil
}

func shutdownLibvirtDomain(ctx context.Context, connectURI, domain string, timeout time.Duration) error {
	if !libvirtDomainExists(ctx, connectURI, domain) {
		return nil
	}
	stopped := []string{"shut off", "shutoff"}
	if state, err := libvirtDomainState(ctx, connectURI, domain); err == nil && slices.Contains(stopped, state) {
		return nil
	}
	if err := libvirt(ctx, connectURI, "shutdown", domain, "--mode", "agent,acpi"); err == nil {
		if waitLibvirtState(ctx, connectURI, domain, stopped, timeout) == nil {
			return nil
		}
	}
	if err := libvirt(ctx, connectURI, "destroy", domain); err != nil {
		if state, stateErr := libvirtDomainState(ctx, connectURI, domain); stateErr == nil && slices.Contains(stopped, state) {
			return nil
		}
		return fmt.Errorf("destroy libvirt domain after shutdown timeout: %w", err)
	}
	return nil
}

func (m *Manager) deleteLibvirtMachine(id string) error {
	machine, err := m.getMachine(id)
	if err != nil {
		return err
	}
	domain := libvirtDomainName(machine)
	if libvirtDomainExists(context.Background(), m.cfg.LibvirtConnectURI, domain) {
		state, _ := libvirtDomainState(context.Background(), m.cfg.LibvirtConnectURI, domain)
		if state == "running" || state == "paused" {
			_ = libvirt(context.Background(), m.cfg.LibvirtConnectURI, "destroy", domain)
		}
		_ = libvirt(context.Background(), m.cfg.LibvirtConnectURI, "undefine", domain, "--nvram", "--snapshots-metadata")
	}
	m.runDeleteHooks(machine.ID)
	machineDir := filepath.Join(m.cfg.DataDir, "machines", machine.ID)
	_ = os.RemoveAll(machineDir)
	if err := m.db.DeleteMachine(machine.ID); err != nil {
		return fmt.Errorf("delete libvirt machine record: %w", err)
	}
	return nil
}

func (m *Manager) libvirtNetwork(name string) libvirtnet.Network {
	return libvirtnet.Network{ConnectURI: m.cfg.LibvirtConnectURI, Name: name, CIDR: m.cfg.LibvirtCIDR}
}

func libvirtVirtInstallArgs(connectURI, domain, diskPath string, machine *models.Machine, spec *models.LibvirtDomainSpec) []string {
	args := []string{
		"--connect", connectURI,
		"--name", domain,
		"--memory", fmt.Sprintf("%d", machine.MemoryMB),
		"--vcpus", fmt.Sprintf("%d", machine.VCPUs),
		"--cpu", "host-passthrough",
		"--osinfo", spec.OSInfo,
	}
	if spec.SMM {
		args = append(args, "--features", "smm.state=on")
	}
	args = append(args,
		"--disk", libvirtDiskArg(diskPath, spec.Disk),
		"--network", libvirtNetworkArg(spec.Network),
		"--graphics", libvirtGraphicsArg(spec.Graphics),
		"--channel", LibvirtGuestAgentChannel,
	)
	if spec.TPM != nil {
		args = append(args, "--tpm", libvirtTPMArg(spec.TPM))
	}
	if boot := libvirtBootArg(spec); boot != "" {
		args = append(args, "--boot", boot)
	}
	args = append(args, "--import", "--noautoconsole")
	return args
}

func libvirtDiskArg(diskPath string, spec models.LibvirtDiskSpec) string {
	parts := []string{
		"path=" + diskPath,
		"bus=" + spec.Bus,
		"format=" + spec.Format,
	}
	if spec.Cache != "" {
		parts = append(parts, "cache="+spec.Cache)
	}
	return strings.Join(parts, ",")
}

func libvirtNetworkArg(spec models.LibvirtNetworkSpec) string {
	return fmt.Sprintf("network=%s,model=%s", libvirtNetworkSource("", spec), spec.Model)
}

func libvirtNetworkSource(fallback string, spec models.LibvirtNetworkSpec) string {
	source := strings.TrimSpace(spec.Source)
	if source != "" {
		return source
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return "kiln-vm"
}

func libvirtGraphicsArg(spec models.LibvirtGraphicsSpec) string {
	if spec.Type == "none" {
		return "none"
	}
	if spec.Listen == "" {
		return spec.Type
	}
	return fmt.Sprintf("%s,listen=%s", spec.Type, spec.Listen)
}

func libvirtTPMArg(spec *models.LibvirtTPMSpec) string {
	parts := []string{
		"backend.type=" + spec.BackendType,
		"model=" + spec.Model,
	}
	if spec.BackendVersion != "" {
		parts = append(parts[:1], append([]string{"backend.version=" + spec.BackendVersion}, parts[1:]...)...)
	}
	return strings.Join(parts, ",")
}

func libvirtBootArg(spec *models.LibvirtDomainSpec) string {
	switch spec.Firmware {
	case "uefi":
		if spec.SecureBoot {
			return "uefi,loader.secure=yes"
		}
		return "uefi"
	default:
		return ""
	}
}

func createLibvirtQCOW2Overlay(ctx context.Context, backingDisk, dstDisk string) error {
	backingDisk = strings.TrimSpace(backingDisk)
	if backingDisk == "" {
		return fmt.Errorf("backing qcow2 path is required")
	}
	absBacking, err := filepath.Abs(backingDisk)
	if err != nil {
		return fmt.Errorf("resolve backing qcow2 path: %w", err)
	}
	if err := libvirtaccess.PrepareReadOnlyFile(absBacking); err != nil {
		return fmt.Errorf("prepare backing qcow2 for libvirt: %w", err)
	}
	if err := runHost(ctx, nil, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", absBacking, dstDisk); err != nil {
		return fmt.Errorf("create qcow2 overlay: %w", err)
	}
	if err := libvirtaccess.PrepareWritableFile(dstDisk); err != nil {
		return fmt.Errorf("prepare qcow2 overlay for libvirt: %w", err)
	}
	return nil
}

func (m *Manager) executeLibvirtWindows(ctx context.Context, machine *models.Machine, request execution.Request) execution.Outcome {
	if machine.IP == "" {
		return execution.OutcomeFromError(fmt.Errorf("windows machine has no IP: %w", apierr.ErrConflict))
	}
	client, err := m.windowsSSHClient(ctx, machine)
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	defer client.Close()
	return executeWindowsSSH(ctx, client, request)
}

func (m *Manager) uploadLibvirtWindows(ctx context.Context, machine *models.Machine, path string, mode uint32, size int64, reader io.Reader) error {
	client, err := m.windowsSSHClient(ctx, machine)
	if err != nil {
		return err
	}
	defer client.Close()
	sftpClient, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true))
	if err != nil {
		return fmt.Errorf("create windows sftp client: %w", err)
	}
	defer sftpClient.Close()
	windowsPath := normalizeWindowsPath(path)
	parent := windowsDir(windowsPath)
	if parent != "" {
		if err := sftpClient.MkdirAll(parent); err != nil {
			return fmt.Errorf("create windows upload parent: %w", err)
		}
	}
	file, err := sftpClient.OpenFile(windowsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("open windows upload file: %w", err)
	}
	defer file.Close()
	written, err := io.Copy(file, reader)
	if err != nil {
		return fmt.Errorf("stream windows upload: %w", err)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("windows upload wrote %d bytes, expected %d", written, size)
	}
	return nil
}

func (m *Manager) dialLibvirtForward(ctx context.Context, machine *models.Machine, port int) (io.ReadWriteCloser, error) {
	if !guestagent.ValidForwardTargetPort(port) {
		return nil, &apierr.ValidationError{
			Field:   "port",
			Message: fmt.Sprintf("must be between %d and %d", guestagent.MinForwardTargetPort, guestagent.MaxForwardTargetPort),
		}
	}
	if machine.IP == "" {
		return nil, fmt.Errorf("windows machine has no IP: %w", apierr.ErrConflict)
	}
	client, err := m.windowsSSHClient(ctx, machine)
	if err != nil {
		return nil, err
	}
	return dialSSHForward(ctx, client, port)
}

func dialSSHForward(ctx context.Context, client *ssh.Client, port int) (io.ReadWriteCloser, error) {
	conn, err := client.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = client.Close()
		return nil, &apierr.ValidationError{
			Field:   "port",
			Message: fmt.Sprintf("guest port %d: %v", port, err),
		}
	}
	return &sshForwardConn{Conn: conn, client: client}, nil
}

type sshForwardConn struct {
	net.Conn
	client *ssh.Client
}

func (c *sshForwardConn) Close() error {
	err := c.Conn.Close()
	_ = c.client.Close()
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("close guest forward: %w", err)
}

func (m *Manager) windowsSSHClient(ctx context.Context, machine *models.Machine) (*ssh.Client, error) {
	signer, err := m.windowsSSHSigner(machine)
	if err != nil {
		return nil, err
	}
	hostKey, err := readWindowsHostKey(m.cfg.DataDir, machine.ID)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(machine.IP, "22")
	config := windowsSSHClientConfig(signer, hostKey)
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial windows ssh %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(config.Timeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("windows ssh handshake %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), nil
}

func (m *Manager) shellLibvirtWindows(ctx context.Context, machine *models.Machine) (io.ReadWriteCloser, error) {
	client, err := m.windowsSSHClient(ctx, machine)
	if err != nil {
		return nil, err
	}
	return newWindowsShellConn(ctx, client), nil
}

type windowsShellConn struct {
	frameW   *io.PipeWriter
	outR     *io.PipeReader
	client   *ssh.Client
	closeMux sync.Mutex
	closed   bool
}

func newWindowsShellConn(ctx context.Context, client *ssh.Client) *windowsShellConn {
	frameR, frameW := io.Pipe()
	outR, outW := io.Pipe()
	conn := &windowsShellConn{
		frameW: frameW,
		outR:   outR,
		client: client,
	}
	go conn.serve(ctx, frameR, outW)
	return conn
}

func (c *windowsShellConn) Read(p []byte) (int, error) {
	n, err := c.outR.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return n, io.ErrClosedPipe
	}
	if err != nil {
		return n, fmt.Errorf("read windows shell stream: %w", err)
	}
	return n, nil
}

func (c *windowsShellConn) Write(p []byte) (int, error) {
	c.closeMux.Lock()
	closed := c.closed
	c.closeMux.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	n, err := c.frameW.Write(p)
	if errors.Is(err, io.ErrClosedPipe) {
		return n, io.ErrClosedPipe
	}
	if err != nil {
		return n, fmt.Errorf("write windows shell frame: %w", err)
	}
	return n, nil
}

func (c *windowsShellConn) Close() error {
	c.closeMux.Lock()
	if c.closed {
		c.closeMux.Unlock()
		return nil
	}
	c.closed = true
	c.closeMux.Unlock()
	_ = c.frameW.Close()
	_ = c.outR.Close()
	if err := c.client.Close(); err != nil {
		return fmt.Errorf("close windows shell SSH connection: %w", err)
	}
	return nil
}

func (c *windowsShellConn) serve(ctx context.Context, frameR *io.PipeReader, outW *io.PipeWriter) {
	defer frameR.Close()
	defer outW.Close()
	defer c.client.Close()

	encoder := json.NewEncoder(outW)
	var encodeMu sync.Mutex
	writeFrame := func(frame guestagent.ShellFrame) bool {
		frame.Version = guestagent.ProtocolVersion
		encodeMu.Lock()
		defer encodeMu.Unlock()
		return encoder.Encode(&frame) == nil
	}

	decoder := json.NewDecoder(frameR)
	var start guestagent.ShellFrame
	if err := decoder.Decode(&start); err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("decode start frame: %v", err)})
		return
	}
	if start.Type != "start" {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: "expected start frame"})
		return
	}
	rows := start.Rows
	cols := start.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	session, err := c.client.NewSession()
	if err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("create windows shell session: %v", err)})
		return
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("open windows shell stdin: %v", err)})
		return
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("open windows shell stdout: %v", err)})
		return
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("open windows shell stderr: %v", err)})
		return
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", int(rows), int(cols), modes); err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("request windows shell pty: %v", err)})
		return
	}
	if err := session.Shell(); err != nil {
		writeFrame(guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("start windows shell: %v", err)})
		return
	}

	copyOutput := func(stream string, reader io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				if !writeFrame(guestagent.ShellFrame{Type: stream, Data: data}) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go copyOutput("stdout", stdout)
	go copyOutput("stderr", stderr)

	waitCh := make(chan guestagent.ShellFrame, 1)
	go func() {
		err := session.Wait()
		resp := guestagent.ShellFrame{Type: "exit"}
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				resp.ExitCode = exitErr.ExitStatus()
			} else {
				resp = guestagent.ShellFrame{Type: "error", Error: err.Error()}
			}
		}
		waitCh <- resp
	}()

	frameCh := make(chan guestagent.ShellFrame, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			var frame guestagent.ShellFrame
			if err := decoder.Decode(&frame); err != nil {
				errCh <- err
				return
			}
			frameCh <- frame
		}
	}()

	for {
		select {
		case <-ctx.Done():
			_ = session.Close()
			writeFrame(guestagent.ShellFrame{Type: "error", Error: ctx.Err().Error()})
			return
		case resp := <-waitCh:
			writeFrame(resp)
			return
		case err := <-errCh:
			_ = session.Close()
			if err != io.EOF && !errors.Is(err, io.ErrClosedPipe) {
				writeFrame(guestagent.ShellFrame{Type: "error", Error: err.Error()})
			}
			return
		case frame := <-frameCh:
			switch frame.Type {
			case "stdin":
				if len(frame.Data) > 0 {
					if _, err := stdin.Write(frame.Data); err != nil {
						_ = session.Close()
						writeFrame(guestagent.ShellFrame{Type: "error", Error: err.Error()})
						return
					}
				}
			case "resize":
				rows := frame.Rows
				cols := frame.Cols
				if rows == 0 {
					rows = 24
				}
				if cols == 0 {
					cols = 80
				}
				_ = session.WindowChange(int(rows), int(cols))
			}
		}
	}
}

func executeWindowsSSH(ctx context.Context, client *ssh.Client, request execution.Request) execution.Outcome {
	session, err := client.NewSession()
	if err != nil {
		return execution.OutcomeFromError(fmt.Errorf("create windows ssh session: %w", err))
	}
	defer session.Close()
	session.Stdin = request.Stdin
	if request.Stdout == nil {
		request.Stdout = io.Discard
	}
	if request.Stderr == nil {
		request.Stderr = io.Discard
	}
	session.Stdout = request.Stdout
	session.Stderr = request.Stderr
	if err := session.Start(windowsExecCommand(request.Command)); err != nil {
		return execution.OutcomeFromError(fmt.Errorf("start windows command: %w", err))
	}
	if request.Started != nil {
		if err := request.Started(); err != nil {
			_ = session.Close()
			return execution.OutcomeFromError(err)
		}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- session.Wait() }()
	select {
	case err := <-waitDone:
		if err == nil {
			return execution.Outcome{}
		}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return execution.Outcome{ExitCode: exitErr.ExitStatus()}
		}
		return execution.OutcomeFromError(fmt.Errorf("run windows command: %w", err))
	case <-ctx.Done():
		_ = session.Close()
		_ = client.Close()
		<-waitDone
		return execution.OutcomeFromError(ctx.Err())
	}
}

func libvirtDomainName(machine *models.Machine) string {
	return naming.LibvirtDomainName(machine.Name, machine.ID)
}

func libvirtDomainExists(ctx context.Context, connectURI, domain string) bool {
	return libvirt(ctx, connectURI, "dominfo", domain) == nil
}

func libvirtDomainState(ctx context.Context, connectURI, domain string) (string, error) {
	out, err := libvirtOutput(ctx, connectURI, "domstate", domain)
	return strings.TrimSpace(out), err
}

func waitLibvirtState(ctx context.Context, connectURI, domain string, wanted []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, err := libvirtDomainState(ctx, connectURI, domain)
		if err == nil {
			for _, want := range wanted {
				if state == want {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for libvirt domain state %v: %w", wanted, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitLibvirtDomainIP(ctx context.Context, connectURI, network, domain string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		out, err := libvirtOutput(ctx, connectURI, "domifaddr", domain, "--source", "lease")
		if err == nil {
			if ip := parseLibvirtIPv4(out); ip != "" {
				return ip, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("wait for libvirt domain IP: %w; last error: %v", ctx.Err(), lastErr)
			}
			return "", fmt.Errorf("wait for libvirt domain IP: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func libvirt(ctx context.Context, connectURI string, args ...string) error {
	_, err := libvirtOutput(ctx, connectURI, args...)
	return err
}

func libvirtOutput(ctx context.Context, connectURI string, args ...string) (string, error) {
	fullArgs := append([]string{"-c", connectURI}, args...)
	return runHostOutput(ctx, "virsh", fullArgs...)
}

func runHost(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	_, err := runHostCombined(ctx, stdin, name, args...)
	return err
}

func runHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := runHostCombined(ctx, nil, name, args...)
	return out, err
}

func runHostCombined(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

var libvirtIPv4Re = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3})(?:/\d+)?\b`)

func parseLibvirtIPv4(text string) string {
	for _, match := range libvirtIPv4Re.FindAllStringSubmatch(text, -1) {
		ip := net.ParseIP(match[1])
		if ip != nil && ip.To4() != nil {
			return match[1]
		}
	}
	return ""
}

func normalizeWindowsPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	if strings.HasPrefix(path, "/") {
		path = strings.TrimPrefix(path, "/")
		if strings.HasPrefix(strings.ToLower(path), "workspace/") {
			return "C:/kiln-vm/workspaces/" + strings.TrimPrefix(path, "workspace/")
		}
		if strings.HasPrefix(strings.ToLower(path), "tmp/") {
			return "C:/kiln-vm/tmp/" + strings.TrimPrefix(path, "tmp/")
		}
		return "C:/kiln-vm/root/" + path
	}
	return path
}

func windowsDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return ""
	}
	return path[:idx]
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		buf = append(buf, byte(unit), byte(unit>>8))
	}
	return buf
}

// windowsExecCommand runs in the guest's OpenSSH DefaultShell, which
// provisioning sets to powershell.exe. $? is captured inside the script block
// because it is always true after & returns.
func windowsExecCommand(command string) string {
	encoded := base64.StdEncoding.EncodeToString(utf16LE(command + "\n$global:KilnVMExecOK = $?"))
	return strings.Join([]string{
		"$ProgressPreference = 'SilentlyContinue'",
		"[Console]::InputEncoding = [Console]::OutputEncoding = $OutputEncoding = New-Object Text.UTF8Encoding $false",
		"$global:KilnVMExecOK = $false",
		"& ([scriptblock]::Create([Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('" + encoded + "'))))",
		"if ($global:KilnVMExecOK) { exit 0 }",
		"if ($LASTEXITCODE) { exit $LASTEXITCODE }",
		"exit 1",
	}, "; ")
}

func uuidString() string {
	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return id.String()
}

func isLibvirtMachine(machine *models.Machine) bool {
	return machine != nil && machine.Backend == libvirtBackend
}
