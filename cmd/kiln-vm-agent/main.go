package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/gfdelarue/kiln-vm/internal/dnsprobe"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/guestexec"
)

const rndAddEntropy = 0x40085203

type randPoolInfo struct {
	entropyCount int32
	bufSize      int32
	buf          [64]uint32
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "seed-entropy":
			if err := seedEntropy(); err != nil {
				log.Fatalf("kiln-vm-agent seed entropy: %v", err)
			}
			log.Printf("kiln-vm-agent seeded kernel entropy")
			return
		case "dns-probe":
			os.Exit(runDNSProbe(os.Args[2:], os.Stdout, os.Stderr))
		}
	}

	execDuplexListener, err := listen(guestagent.ExecDuplexPort)
	if err != nil {
		log.Fatalf("kiln-vm-agent exec duplex listen: %v", err)
	}
	defer execDuplexListener.Close()

	log.Printf("kiln-vm-agent listening on vsock port %d", guestagent.ExecDuplexPort)
	go serveExecDuplex(execDuplexListener)

	uploadListener, err := listen(guestagent.UploadPort)
	if err != nil {
		log.Fatalf("kiln-vm-agent upload listen: %v", err)
	}
	defer uploadListener.Close()

	log.Printf("kiln-vm-agent listening on vsock port %d", guestagent.UploadPort)
	go serveUpload(uploadListener)

	forwardListener, err := listenVsock(guestagent.ForwardPort, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
	if err != nil {
		log.Fatalf("kiln-vm-agent forward listen: %v", err)
	}
	defer forwardListener.Close()

	log.Printf("kiln-vm-agent listening on vsock port %d", guestagent.ForwardPort)
	go serveForward(forwardListener)

	shellListener, err := listenVsock(guestagent.ShellPort, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
	if err != nil {
		log.Fatalf("kiln-vm-agent shell listen: %v", err)
	}
	defer shellListener.Close()

	log.Printf("kiln-vm-agent listening on vsock port %d", guestagent.ShellPort)
	serveShell(shellListener)
}

func runDNSProbe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("kiln-vm-agent dns-probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", "", "DNS server IPv4 address, optionally followed by a port")
	name := flags.String("name", dnsprobe.DefaultName, "external fully-qualified A-record name to resolve")
	totalTimeout := flags.Duration("timeout", dnsprobe.DefaultTotalTimeout, "hard total probe timeout")
	transportTimeout := flags.Duration("transport-timeout", dnsprobe.DefaultTransportTimeout, "per-transport timeout")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: kiln-vm-agent dns-probe --server <IPv4[:port]> [--name example.com.] [--timeout 8s] [--transport-timeout 3s]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return dnsprobe.ExitOK
		}
		return writeDNSProbeResult(stdout, dnsprobe.InvalidResult("", dnsprobe.DefaultName, err), dnsprobe.ExitUsage)
	}
	if flags.NArg() != 0 {
		err := fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
		return writeDNSProbeResult(stdout, dnsprobe.InvalidResult(*server, *name, err), dnsprobe.ExitUsage)
	}
	result := dnsprobe.Probe(context.Background(), dnsprobe.Options{
		Server:           *server,
		Name:             *name,
		TotalTimeout:     *totalTimeout,
		TransportTimeout: *transportTimeout,
	})
	return writeDNSProbeResult(stdout, result, result.ExitCode())
}

func writeDNSProbeResult(writer io.Writer, result dnsprobe.Result, exitCode int) int {
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		return 1
	}
	return exitCode
}

func seedEntropy() error {
	file, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/random: %w", err)
	}
	defer file.Close()

	seedMaterial := fmt.Sprintf("%d:%d:%d:%s:%s", time.Now().UnixNano(), os.Getpid(), os.Getppid(), runtime.GOOS, runtime.GOARCH)
	if bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		seedMaterial += ":" + string(bootID)
	}
	if uptime, err := os.ReadFile("/proc/uptime"); err == nil {
		seedMaterial += ":" + string(uptime)
	}
	sum := sha256.Sum256([]byte(seedMaterial))

	var pool randPoolInfo
	pool.entropyCount = int32(len(sum) * 8)
	pool.bufSize = int32(len(sum))
	for i := 0; i < len(sum); i += 4 {
		pool.buf[i/4] = uint32(sum[i]) | uint32(sum[i+1])<<8 | uint32(sum[i+2])<<16 | uint32(sum[i+3])<<24
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&pool))); errno != 0 {
		return fmt.Errorf("add entropy to /dev/random: %w", errno)
	}
	return nil
}

func serveExecDuplex(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("kiln-vm-agent exec duplex accept: %v", err)
			continue
		}
		go handleExecDuplexConn(conn)
	}
}

func serveShell(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("kiln-vm-agent shell accept: %v", err)
			continue
		}
		go handleShellConn(conn)
	}
}

func serveUpload(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("kiln-vm-agent upload accept: %v", err)
			continue
		}
		go handleUploadConn(conn)
	}
}

func handleExecDuplexConn(conn net.Conn) {
	defer conn.Close()
	codec := guestagent.NewExecDuplexCodec(conn, conn)
	if err := codec.WriteResponse(guestagent.ExecDuplexResponseFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameReady,
	}); err != nil {
		return
	}
	start, err := codec.ReadRequest()
	if err != nil {
		writeExecDuplexTerminal(codec, execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryProtocolError,
			Message:  fmt.Sprintf("decode start frame: %v", err),
			Cause:    err,
		}))
		return
	}
	if start.Type != guestagent.ExecDuplexFrameStart || start.Start == nil {
		writeExecDuplexTerminal(codec, execution.Outcome{
			Category: execution.CategoryProtocolError,
			Message:  "expected one start frame",
		})
		return
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	writer := newQueuedExecDuplexWriter(cancel, codec)
	defer writer.stop()
	stdinReader, stdinWriter := io.Pipe()
	runDone := make(chan struct{})
	go readExecDuplexInput(ctx, cancel, runDone, codec, stdinWriter)

	outcome := (guestexec.Supervisor{}).Execute(ctx, execution.Request{
		Command: start.Start.Command,
		Stdin:   stdinReader,
		Stdout:  execDuplexStreamWriter{writer: writer, frameType: guestagent.ExecDuplexFrameStdout},
		Stderr:  execDuplexStreamWriter{writer: writer, frameType: guestagent.ExecDuplexFrameStderr},
		Started: func() error {
			return writer.send(guestagent.ExecDuplexResponseFrame{
				Version: guestagent.ExecDuplexProtocolVersion,
				Type:    guestagent.ExecDuplexFrameStarted,
			})
		},
	})
	close(runDone)
	_ = stdinReader.Close()
	_ = stdinWriter.Close()
	interruptExecRead(conn)
	_ = writer.send(terminalExecDuplexFrame(outcome))
}

func readExecDuplexInput(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	runDone <-chan struct{},
	codec *guestagent.ExecDuplexCodec,
	stdin *io.PipeWriter,
) {
	defer stdin.Close()
	stdinEOF := false
	for {
		frame, err := codec.ReadRequest()
		if runFinished(runDone) {
			return
		}
		if err != nil {
			cause := error(context.Canceled)
			if !errors.Is(err, io.EOF) {
				cause = &execution.Error{
					Category: execution.CategoryProtocolError,
					Message:  execution.Diagnostic(err.Error()),
					Cause:    err,
				}
			}
			_ = stdin.CloseWithError(cause)
			cancel(cause)
			return
		}
		switch frame.Type {
		case guestagent.ExecDuplexFrameStdin:
			if stdinEOF {
				err := errors.New("stdin received after EOF")
				_ = stdin.CloseWithError(err)
				cancel(&execution.Error{Category: execution.CategoryProtocolError, Message: err.Error(), Cause: err})
				return
			}
			if len(frame.Data) > 0 {
				if _, err := stdin.Write(frame.Data); err != nil && ctx.Err() == nil {
					cancel(err)
					return
				}
			}
		case guestagent.ExecDuplexFrameStdinEOF:
			if stdinEOF {
				err := errors.New("duplicate stdin EOF")
				_ = stdin.CloseWithError(err)
				cancel(&execution.Error{Category: execution.CategoryProtocolError, Message: err.Error(), Cause: err})
				return
			}
			stdinEOF = true
			_ = stdin.Close()
		default:
			err := fmt.Errorf("unexpected exec duplex frame %q", frame.Type)
			_ = stdin.CloseWithError(err)
			cancel(&execution.Error{Category: execution.CategoryProtocolError, Message: err.Error(), Cause: err})
			return
		}
	}
}

func runFinished(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func interruptExecRead(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now())
}

type queuedExecDuplexWrite struct {
	frame guestagent.ExecDuplexResponseFrame
	done  chan error
}

type queuedExecDuplexWriter struct {
	queue chan queuedExecDuplexWrite
	stopC chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newQueuedExecDuplexWriter(
	cancel context.CancelCauseFunc,
	codec *guestagent.ExecDuplexCodec,
) *queuedExecDuplexWriter {
	writer := &queuedExecDuplexWriter{
		queue: make(chan queuedExecDuplexWrite, execution.ResponseQueueCapacity),
		stopC: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go func() {
		defer close(writer.done)
		for {
			select {
			case <-writer.stopC:
				return
			case item := <-writer.queue:
				err := codec.WriteResponse(item.frame)
				item.done <- err
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return writer
}

func (w *queuedExecDuplexWriter) send(frame guestagent.ExecDuplexResponseFrame) error {
	item := queuedExecDuplexWrite{frame: frame, done: make(chan error, 1)}
	select {
	case <-w.done:
		return io.ErrClosedPipe
	case w.queue <- item:
	}
	select {
	case <-w.done:
		return io.ErrClosedPipe
	case err := <-item.done:
		return err
	}
}

func (w *queuedExecDuplexWriter) stop() {
	w.once.Do(func() { close(w.stopC) })
	<-w.done
}

type execDuplexStreamWriter struct {
	writer    *queuedExecDuplexWriter
	frameType string
}

func (w execDuplexStreamWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		size := min(len(data), execution.MaxDataBytes)
		if err := w.writer.send(guestagent.ExecDuplexResponseFrame{
			Version: guestagent.ExecDuplexProtocolVersion,
			Type:    w.frameType,
			Data:    append([]byte(nil), data[:size]...),
		}); err != nil {
			return written, err
		}
		written += size
		data = data[size:]
	}
	return written, nil
}

func terminalExecDuplexFrame(outcome execution.Outcome) guestagent.ExecDuplexResponseFrame {
	return guestagent.ExecDuplexResponseFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameTerminal,
		Terminal: &guestagent.ExecDuplexTerminal{
			ExitCode:  outcome.ExitCode,
			Category:  outcome.Category,
			Message:   execution.Diagnostic(outcome.Message),
			Retryable: outcome.Retryable,
			Ambiguous: outcome.Ambiguous,
		},
	}
}

func writeExecDuplexTerminal(codec *guestagent.ExecDuplexCodec, outcome execution.Outcome) {
	_ = codec.WriteResponse(terminalExecDuplexFrame(outcome))
}

func handleUploadConn(conn net.Conn) {
	defer conn.Close()

	var req guestagent.UploadRequest
	reader := bufio.NewReader(conn)
	header, err := reader.ReadBytes('\n')
	if err != nil {
		writeUploadResponse(conn, fmt.Sprintf("read request header: %v", err))
		return
	}
	if err := json.Unmarshal(header, &req); err != nil {
		writeUploadResponse(conn, fmt.Sprintf("decode request header: %v", err))
		return
	}
	if req.Version != guestagent.ProtocolVersion {
		writeUploadResponse(conn, fmt.Sprintf("protocol mismatch: got %d want %d", req.Version, guestagent.ProtocolVersion))
		return
	}
	if req.Path == "" || !filepath.IsAbs(req.Path) {
		writeUploadResponse(conn, "path must be absolute")
		return
	}
	if req.Size < 0 {
		writeUploadResponse(conn, "size must be non-negative")
		return
	}
	if err := os.MkdirAll(filepath.Dir(req.Path), 0755); err != nil {
		writeUploadResponse(conn, fmt.Sprintf("create parent directory: %v", err))
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(req.Path), "."+filepath.Base(req.Path)+".tmp-*")
	if err != nil {
		writeUploadResponse(conn, fmt.Sprintf("create temp file: %v", err))
		return
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.CopyN(tmp, reader, req.Size); err != nil {
		_ = tmp.Close()
		writeUploadResponse(conn, fmt.Sprintf("read upload body: %v", err))
		return
	}
	if err := tmp.Chmod(os.FileMode(req.Mode)); err != nil {
		_ = tmp.Close()
		writeUploadResponse(conn, fmt.Sprintf("chmod upload: %v", err))
		return
	}
	if err := tmp.Close(); err != nil {
		writeUploadResponse(conn, fmt.Sprintf("close upload: %v", err))
		return
	}
	if err := os.Rename(tmpPath, req.Path); err != nil {
		writeUploadResponse(conn, fmt.Sprintf("install upload: %v", err))
		return
	}
	cleanup = false
	writeUploadResponse(conn, "")
}

func writeUploadResponse(w io.Writer, message string) {
	resp := guestagent.UploadResponse{
		Version: guestagent.ProtocolVersion,
		Error:   message,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("kiln-vm-agent upload write response: %v", err)
	}
}

func handleShellConn(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(bufio.NewReader(conn))
	frameCh := make(chan guestagent.ShellFrame, 32)
	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	go func() {
		encoder := json.NewEncoder(conn)
		for {
			select {
			case frame := <-frameCh:
				if err := encoder.Encode(&frame); err != nil {
					log.Printf("kiln-vm-agent shell encode: %v", err)
					closeDone()
					return
				}
			case <-done:
				return
			}
		}
	}()

	var start guestagent.ShellFrame
	if err := decoder.Decode(&start); err != nil {
		frameCh <- guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("decode start frame: %v", err)}
		closeDone()
		return
	}
	if start.Type != "start" {
		frameCh <- guestagent.ShellFrame{Type: "error", Error: "expected start frame"}
		closeDone()
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

	cmd := exec.Command("/bin/sh")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		frameCh <- guestagent.ShellFrame{Type: "error", Error: fmt.Sprintf("start shell: %v", err)}
		closeDone()
		return
	}
	defer ptmx.Close()

	output := make(chan struct{})
	go func() {
		defer close(output)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				select {
				case frameCh <- guestagent.ShellFrame{Type: "stdout", Data: data}:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for {
			var frame guestagent.ShellFrame
			if err := decoder.Decode(&frame); err != nil {
				closeDone()
				return
			}

			switch frame.Type {
			case "stdin":
				if len(frame.Data) > 0 {
					if _, err := ptmx.Write(frame.Data); err != nil {
						closeDone()
						return
					}
				}
			case "resize":
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: frame.Rows, Cols: frame.Cols})
			}
		}
	}()

	err = cmd.Wait()
	select {
	case <-output:
	case <-time.After(time.Second):
	}
	resp := guestagent.ShellFrame{Type: "exit"}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			frameCh <- guestagent.ShellFrame{Type: "error", Error: err.Error()}
			closeDone()
			return
		}
	}
	frameCh <- resp
	time.Sleep(100 * time.Millisecond)
	closeDone()
}

func listen(port uint32) (net.Listener, error) {
	return listenVsock(port, 0)
}

func listenVsock(port uint32, acceptFlags int) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &vsockListener{fd: fd, port: port, acceptFlags: acceptFlags}, nil
}

type vsockListener struct {
	fd          int
	port        uint32
	acceptFlags int
}

func (l *vsockListener) Accept() (net.Conn, error) {
	connFD, _, err := unix.Accept4(l.fd, l.acceptFlags)
	if err != nil {
		return nil, fmt.Errorf("accept vsock connection: %w", err)
	}
	file := os.NewFile(uintptr(connFD), fmt.Sprintf("kiln-vm-agent-vsock-%d", connFD))
	return &vsockConn{File: file}, nil
}

func (l *vsockListener) Close() error {
	if err := unix.Close(l.fd); err != nil {
		return fmt.Errorf("close vsock listener: %w", err)
	}
	return nil
}

func (l *vsockListener) Addr() net.Addr {
	return vsockAddr{network: "vsock", cid: unix.VMADDR_CID_ANY, port: l.port}
}

type vsockConn struct {
	*os.File
}

func (c *vsockConn) LocalAddr() net.Addr {
	return vsockAddr{network: "vsock"}
}

func (c *vsockConn) RemoteAddr() net.Addr {
	return vsockAddr{network: "vsock"}
}

func (c *vsockConn) SetDeadline(t time.Time) error {
	if err := c.File.SetDeadline(t); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	return nil
}

func (c *vsockConn) SetReadDeadline(t time.Time) error {
	if err := c.File.SetReadDeadline(t); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	return nil
}

func (c *vsockConn) SetWriteDeadline(t time.Time) error {
	if err := c.File.SetWriteDeadline(t); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return nil
}

type vsockAddr struct {
	network string
	cid     uint32
	port    uint32
}

func (a vsockAddr) Network() string {
	return a.network
}

func (a vsockAddr) String() string {
	return fmt.Sprintf("%d:%d", a.cid, a.port)
}
