package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

func TestExecDuplexCodecUsesNestedStartAndBoundsData(t *testing.T) {
	var encoded bytes.Buffer
	codec := NewExecDuplexCodec(nil, &encoded)
	if err := codec.WriteRequest(ExecDuplexRequestFrame{
		Version: ExecDuplexProtocolVersion,
		Type:    ExecDuplexFrameStart,
		Start:   &ExecDuplexStart{Command: "printf ok"},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(encoded.Bytes()), &raw); err != nil {
		t.Fatalf("decode raw frame: %v", err)
	}
	if _, exists := raw["command"]; exists {
		t.Fatal("start command must not appear at the frame root")
	}
	start, ok := raw["start"].(map[string]any)
	if !ok || start["command"] != "printf ok" {
		t.Fatalf("nested start = %#v", raw["start"])
	}

	if err := codec.WriteRequest(ExecDuplexRequestFrame{
		Version: ExecDuplexProtocolVersion,
		Type:    ExecDuplexFrameStdin,
		Data:    make([]byte, execution.MaxDataBytes+1),
	}); err == nil {
		t.Fatal("oversized stdin frame succeeded")
	}
}

func TestExecDuplexCodecRejectsWrongDirectionAndMissingTerminal(t *testing.T) {
	requestCodec := NewExecDuplexCodec(strings.NewReader(`{"version":2,"type":"stdout","data":"b2s="}`+"\n"), nil)
	if _, err := requestCodec.ReadRequest(); err == nil {
		t.Fatal("response frame accepted in request direction")
	}
	responseCodec := NewExecDuplexCodec(strings.NewReader(`{"version":2,"type":"terminal"}`+"\n"), nil)
	if _, err := responseCodec.ReadResponse(); err == nil {
		t.Fatal("terminal frame without payload succeeded")
	}
}

func TestSendExecDuplexInputEmitsChunksAndEOF(t *testing.T) {
	var encoded bytes.Buffer
	codec := NewExecDuplexCodec(nil, &encoded)
	payload := bytes.Repeat([]byte("x"), execution.MaxDataBytes+17)
	if err := sendExecDuplexInput(codec, bytes.NewReader(payload)); err != nil {
		t.Fatalf("send input: %v", err)
	}
	decoder := NewExecDuplexCodec(&encoded, nil)
	var got bytes.Buffer
	for {
		frame, err := decoder.ReadRequest()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if frame.Type == ExecDuplexFrameStdinEOF {
			break
		}
		_, _ = got.Write(frame.Data)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("payload length = %d, want %d", got.Len(), len(payload))
	}
}

func TestExecuteFailClosedUsesOnlyDuplexPort(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vsock.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	connect := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := NewExecDuplexCodec(conn, nil).reader.ReadString('\n')
		connect <- line
		_, _ = io.WriteString(conn, "ERR unsupported\n")
	}()
	outcome := Execute(context.Background(), socketPath, execution.Request{Command: "true", Stdin: io.LimitReader(strings.NewReader(""), 0)})
	if outcome.Category != execution.CategoryRuntimeIncompatible {
		t.Fatalf("outcome = %#v", outcome)
	}
	if got := <-connect; got != "CONNECT 10706\n" {
		t.Fatalf("connect = %q, want only duplex port", got)
	}
}

func TestExecuteStreamsDuplexInputAndOutput(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vsock.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var connect [14]byte
		if _, err := io.ReadFull(conn, connect[:]); err != nil || string(connect[:]) != "CONNECT 10706\n" {
			return
		}
		_, _ = io.WriteString(conn, "OK 12345\n")
		codec := NewExecDuplexCodec(conn, conn)
		_ = codec.WriteResponse(ExecDuplexResponseFrame{Version: ExecDuplexProtocolVersion, Type: ExecDuplexFrameReady})
		start, err := codec.ReadRequest()
		if err != nil || start.Start == nil || start.Start.Command != "cat" {
			return
		}
		var body strings.Builder
		for {
			frame, err := codec.ReadRequest()
			if err != nil {
				return
			}
			if frame.Type == ExecDuplexFrameStdinEOF {
				break
			}
			body.Write(frame.Data)
		}
		received <- body.String()
		_ = codec.WriteResponse(ExecDuplexResponseFrame{Version: ExecDuplexProtocolVersion, Type: ExecDuplexFrameStarted})
		_ = codec.WriteResponse(ExecDuplexResponseFrame{Version: ExecDuplexProtocolVersion, Type: ExecDuplexFrameStdout, Data: []byte("ok")})
		_ = codec.WriteResponse(ExecDuplexResponseFrame{
			Version:  ExecDuplexProtocolVersion,
			Type:     ExecDuplexFrameTerminal,
			Terminal: &ExecDuplexTerminal{},
		})
	}()
	var stdout bytes.Buffer
	outcome := Execute(context.Background(), socketPath, execution.Request{
		Command: "cat",
		Stdin:   strings.NewReader("payload"),
		Stdout:  &stdout,
	})
	if !outcome.Succeeded() || stdout.String() != "ok" {
		t.Fatalf("outcome=%#v stdout=%q", outcome, stdout.String())
	}
	if got := <-received; got != "payload" {
		t.Fatalf("stdin = %q", got)
	}
}

func TestExecuteRequiresApplicationReadyAfterVsockAck(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vsock.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var connect [14]byte
		if _, err := io.ReadFull(conn, connect[:]); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "OK 12345\n")
	}()

	outcome := Execute(context.Background(), socketPath, execution.Request{Command: "true"})
	if outcome.Category != execution.CategoryRuntimeIncompatible {
		t.Fatalf("outcome = %#v", outcome)
	}
}
