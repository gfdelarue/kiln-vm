package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	ForwardPort             = 10707
	MinForwardTargetPort    = 1
	MaxForwardTargetPort    = 65535
	forwardHandshakeTimeout = 15 * time.Second
)

type ForwardRequest struct {
	Version int `json:"version"`
	Port    int `json:"port"`
}

type ForwardResponse struct {
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`
}

type ForwardTargetError struct {
	Message string
}

func (e *ForwardTargetError) Error() string {
	return e.Message
}

func ValidForwardTargetPort(port int) bool {
	return port >= MinForwardTargetPort && port <= MaxForwardTargetPort
}

func DialForward(ctx context.Context, udsPath string, port int) (net.Conn, error) {
	if !ValidForwardTargetPort(port) {
		return nil, fmt.Errorf("forward port %d is outside %d-%d", port, MinForwardTargetPort, MaxForwardTargetPort)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial firecracker vsock bridge: %w", err)
	}

	deadline := time.Now().Add(forwardHandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set guest forward handshake deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	reader, err := forwardHandshake(conn, port)
	if !stop() {
		conn.Close()
		return nil, fmt.Errorf("guest forward handshake cancelled: %w", ctx.Err())
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear guest forward handshake deadline: %w", err)
	}
	return &forwardConn{Conn: conn, reader: reader}, nil
}

func forwardHandshake(conn net.Conn, port int) (*bufio.Reader, error) {
	reader, err := connectGuestPort(conn, ForwardPort)
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(ForwardRequest{Version: ProtocolVersion, Port: port})
	if err != nil {
		return nil, fmt.Errorf("encode guest forward request: %w", err)
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return nil, fmt.Errorf("write guest forward request: %w", err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read guest forward response: %w", err)
	}
	var response ForwardResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("decode guest forward response: %w", err)
	}
	if response.Error != "" {
		return nil, &ForwardTargetError{Message: response.Error}
	}
	if response.Version != ProtocolVersion {
		return nil, fmt.Errorf("unsupported guest forward protocol version %d", response.Version)
	}
	return reader, nil
}

type forwardConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *forwardConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read guest forward stream: %w", err)
	}
	return n, nil
}
