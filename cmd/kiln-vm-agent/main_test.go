package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/gfdelarue/kiln-vm/internal/dnsprobe"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/guestexec"
)

func testShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func TestRunDNSProbeEmitsVersionedStructuredSuccess(t *testing.T) {
	server := startAgentDNSTestServer(t)
	var stdout, stderr bytes.Buffer

	exitCode := runDNSProbe([]string{
		"--server", server,
		"--timeout", "1s",
		"--transport-timeout", "300ms",
	}, &stdout, &stderr)

	if exitCode != dnsprobe.ExitOK {
		t.Fatalf("runDNSProbe exit = %d, stderr=%q stdout=%q", exitCode, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("runDNSProbe stderr = %q, want empty", stderr.String())
	}
	var result dnsprobe.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode dns-probe result: %v; output=%q", err, stdout.String())
	}
	if result.SchemaVersion != 1 || result.Name != "example.com." || result.QueryType != "A" || !result.PathOK || !result.ResolutionOK || !result.UDP.ResolutionOK || !result.TCP.ResolutionOK {
		t.Fatalf("dns-probe result = %+v, want versioned UDP+TCP success", result)
	}
}

func TestRunDNSProbeReturnsVersionedUsageFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runDNSProbe([]string{"--server", "resolver.invalid"}, &stdout, &stderr)

	if exitCode != dnsprobe.ExitUsage {
		t.Fatalf("runDNSProbe exit = %d, want %d", exitCode, dnsprobe.ExitUsage)
	}
	var result dnsprobe.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode dns-probe usage result: %v; output=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if result.SchemaVersion != 1 || result.FailureKind != "usage" || !strings.Contains(result.Error, "IPv4 address") {
		t.Fatalf("dns-probe usage result = %+v", result)
	}
}

func startAgentDNSTestServer(t *testing.T) string {
	t.Helper()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test DNS TCP: %v", err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		_ = tcp.Close()
		t.Fatalf("listen test DNS UDP: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, peer, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		_, _ = udp.WriteToUDP(agentDNSTestResponse(buf[:n]), peer)
	}()
	go func() {
		defer wg.Done()
		conn, err := tcp.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var frame [2]byte
		if _, err := io.ReadFull(conn, frame[:]); err != nil {
			return
		}
		query := make([]byte, int(binary.BigEndian.Uint16(frame[:])))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		response := agentDNSTestResponse(query)
		binary.BigEndian.PutUint16(frame[:], uint16(len(response)))
		_, _ = conn.Write(append(frame[:], response...))
	}()
	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
		wg.Wait()
	})
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func agentDNSTestResponse(query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return []byte{0}
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) != 1 {
		return []byte{0}
	}
	header.Response = true
	header.RecursionAvailable = true
	builder := dnsmessage.NewBuilder(nil, header)
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return []byte{0}
	}
	if err := builder.Question(questions[0]); err != nil {
		return []byte{0}
	}
	if err := builder.StartAnswers(); err != nil {
		return []byte{0}
	}
	if err := builder.AResource(dnsmessage.ResourceHeader{
		Name:  questions[0].Name,
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
		TTL:   60,
	}, dnsmessage.AResource{A: [4]byte{93, 184, 216, 34}}); err != nil {
		return []byte{0}
	}
	response, err := builder.Finish()
	if err != nil {
		return []byte{0}
	}
	return response
}

func TestGuestSupervisorStagesLargeCommands(t *testing.T) {
	command := strings.Repeat(": # padding\n", 40_000) + "printf ok"
	result, err := execution.RunBuffered(context.Background(), guestexec.Supervisor{}, command, nil)
	if err != nil {
		t.Fatalf("execute large command: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec exit = %d stderr=%q", result.ExitCode, result.Stderr)
	}
	if result.Stdout != "ok" {
		t.Fatalf("exec stdout = %q, want ok", result.Stdout)
	}
}

func TestExecDuplexFeedsExactInputAndEOF(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExecDuplexConn(server)
	}()
	codec := guestagent.NewExecDuplexCodec(client, client)
	requireExecDuplexReady(t, codec)

	payload := strings.Repeat("agent-exec-duplex\n", 4096)
	command := "tee /tmp/kiln-vm-agent-exec-duplex-test | wc -c; sha256sum /tmp/kiln-vm-agent-exec-duplex-test | cut -d' ' -f1; rm -f /tmp/kiln-vm-agent-exec-duplex-test"
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStart,
		Start:   &guestagent.ExecDuplexStart{Command: command},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() {
		for offset := 0; offset < len(payload); offset += 7000 {
			end := min(offset+7000, len(payload))
			if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
				Version: guestagent.ExecDuplexProtocolVersion,
				Type:    guestagent.ExecDuplexFrameStdin,
				Data:    []byte(payload[offset:end]),
			}); err != nil {
				writeErr <- fmt.Errorf("write stdin: %w", err)
				return
			}
		}
		writeErr <- codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
			Version: guestagent.ExecDuplexProtocolVersion,
			Type:    guestagent.ExecDuplexFrameStdinEOF,
		})
	}()

	var stdout strings.Builder
	var terminal guestagent.ExecDuplexResponseFrame
	for terminal.Terminal == nil {
		frame, err := codec.ReadResponse()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		switch frame.Type {
		case guestagent.ExecDuplexFrameStdout:
			stdout.Write(frame.Data)
		case guestagent.ExecDuplexFrameTerminal:
			terminal = frame
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write input: %v", err)
	}
	_ = client.Close()
	<-done
	wantDigest := sha256.Sum256([]byte(payload))
	want := fmt.Sprintf("%d\n%x\n", len(payload), wantDigest)
	if terminal.Terminal.ExitCode != 0 || terminal.Terminal.Category != execution.CategoryNone || stdout.String() != want {
		t.Fatalf("terminal = %#v stdout = %q, want stdout %q", terminal.Terminal, stdout.String(), want)
	}
}

func TestExecDuplexStreamsOutputBeforeCompletion(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExecDuplexConn(server)
	}()
	codec := guestagent.NewExecDuplexCodec(client, client)
	requireExecDuplexReady(t, codec)
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStart,
		Start: &guestagent.ExecDuplexStart{
			Command: "printf early; sleep 1; printf done",
		},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStdinEOF,
	}); err != nil {
		t.Fatalf("write EOF: %v", err)
	}
	started, err := codec.ReadResponse()
	if err != nil || started.Type != guestagent.ExecDuplexFrameStarted {
		t.Fatalf("started = %#v, %v", started, err)
	}
	output, err := codec.ReadResponse()
	if err != nil || output.Type != guestagent.ExecDuplexFrameStdout || !strings.HasPrefix(string(output.Data), "early") {
		t.Fatalf("early output = %#v, %v", output, err)
	}
	select {
	case <-done:
		t.Fatal("duplex execution completed before its one-second workload")
	default:
	}
	output, err = codec.ReadResponse()
	if err != nil || output.Type != guestagent.ExecDuplexFrameStdout || !strings.HasPrefix(string(output.Data), "done") {
		t.Fatalf("final output = %#v, %v", output, err)
	}
	terminal, err := codec.ReadResponse()
	if err != nil || terminal.Type != guestagent.ExecDuplexFrameTerminal || terminal.Terminal.ExitCode != 0 {
		t.Fatalf("terminal = %#v, %v", terminal, err)
	}
	_ = client.Close()
	<-done
}

func TestExecDuplexDisconnectTerminatesProcessGroupGracefully(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExecDuplexConn(server)
	}()
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	cleanupPath := filepath.Join(dir, "cleaned")
	codec := guestagent.NewExecDuplexCodec(client, client)
	requireExecDuplexReady(t, codec)
	command := fmt.Sprintf(
		"trap \"printf cleaned > %s; exit 0\" TERM; printf started > %s; while :; do sleep 1; done",
		testShellQuote(cleanupPath),
		testShellQuote(startedPath),
	)
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStart,
		Start:   &guestagent.ExecDuplexStart{Command: command},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStdinEOF,
	}); err != nil {
		t.Fatalf("write EOF: %v", err)
	}
	if frame, err := codec.ReadResponse(); err != nil || frame.Type != guestagent.ExecDuplexFrameStarted {
		t.Fatalf("started = %#v, %v", frame, err)
	}
	waitForAgentMarker(t, startedPath)
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("duplex handler did not terminate after disconnect")
	}
	if body, err := os.ReadFile(cleanupPath); err != nil || string(body) != "cleaned" {
		t.Fatalf("cleanup marker = %q, %v", body, err)
	}
}

func TestExecDuplexProtocolViolationAfterStartTerminatesAndReportsTerminal(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExecDuplexConn(server)
	}()
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	cleanupPath := filepath.Join(dir, "cleaned")
	codec := guestagent.NewExecDuplexCodec(client, client)
	requireExecDuplexReady(t, codec)
	command := fmt.Sprintf(
		"trap \"printf cleaned > %s; exit 0\" TERM; printf started > %s; while :; do sleep 1; done",
		testShellQuote(cleanupPath),
		testShellQuote(startedPath),
	)
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStart,
		Start:   &guestagent.ExecDuplexStart{Command: command},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStdinEOF,
	}); err != nil {
		t.Fatalf("write EOF: %v", err)
	}
	if frame, err := codec.ReadResponse(); err != nil || frame.Type != guestagent.ExecDuplexFrameStarted {
		t.Fatalf("started = %#v, %v", frame, err)
	}
	waitForAgentMarker(t, startedPath)
	if err := codec.WriteRequest(guestagent.ExecDuplexRequestFrame{
		Version: guestagent.ExecDuplexProtocolVersion,
		Type:    guestagent.ExecDuplexFrameStdinEOF,
	}); err != nil {
		t.Fatalf("write duplicate EOF: %v", err)
	}
	var terminal guestagent.ExecDuplexResponseFrame
	for {
		frame, err := codec.ReadResponse()
		if err != nil {
			t.Fatalf("read terminal: %v", err)
		}
		if frame.Type == guestagent.ExecDuplexFrameTerminal {
			terminal = frame
			break
		}
	}
	if terminal.Terminal.Category != execution.CategoryProtocolError {
		t.Fatalf("terminal = %#v", terminal)
	}
	if body, err := os.ReadFile(cleanupPath); err != nil || string(body) != "cleaned" {
		t.Fatalf("cleanup marker = %q, %v", body, err)
	}
	_ = client.Close()
	<-done
}

func requireExecDuplexReady(t *testing.T, codec *guestagent.ExecDuplexCodec) {
	t.Helper()
	frame, err := codec.ReadResponse()
	if err != nil || frame.Type != guestagent.ExecDuplexFrameReady {
		t.Fatalf("ready = %#v, %v", frame, err)
	}
}

func waitForAgentMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("guest command did not create started marker %s", path)
}

func TestShellSendsExitFrameAndCloses(t *testing.T) {
	client, agent := net.Pipe()
	done := make(chan struct{})
	go func() {
		handleShellConn(agent)
		close(done)
	}()
	t.Cleanup(func() {
		client.Close()
		<-done
	})
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(client)
	go func() {
		_ = enc.Encode(guestagent.ShellFrame{Type: "start", Version: guestagent.ProtocolVersion})
		_ = enc.Encode(guestagent.ShellFrame{Type: "stdin", Data: []byte("(sleep 0.3; echo $((6*7))) & exit 7\n")})
	}()
	dec := json.NewDecoder(client)
	var stdout []byte
	for {
		var frame guestagent.ShellFrame
		if err := dec.Decode(&frame); err != nil {
			t.Fatalf("no exit frame: %v", err)
		}
		if frame.Type == "error" {
			t.Fatalf("shell error: %s", frame.Error)
		}
		if frame.Type == "stdout" {
			stdout = append(stdout, frame.Data...)
		}
		if frame.Type == "exit" {
			if frame.ExitCode != 7 {
				t.Fatalf("exit code = %d, want 7", frame.ExitCode)
			}
			break
		}
	}
	if !bytes.Contains(stdout, []byte("42")) {
		t.Fatalf("exit frame came before the shell's last output; stdout = %q", stdout)
	}
	var extra guestagent.ShellFrame
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("after exit: frame %+v, err %v; want EOF", extra, err)
	}
}
