package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/guestagent"
)

const (
	forwardRequestTimeout    = 30 * time.Second
	forwardRequestMaxBytes   = 4096
	forwardTargetDialTimeout = 5 * time.Second
)

func serveForward(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("kiln-vm-agent forward accept: %v", err)
			continue
		}
		go handleForwardConn(conn)
	}
}

func handleForwardConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReaderSize(conn, forwardRequestMaxBytes)
	_ = conn.SetReadDeadline(time.Now().Add(forwardRequestTimeout))
	line, err := reader.ReadSlice('\n')
	if err != nil {
		writeForwardResponse(conn, fmt.Sprintf("read forward request: %v", err))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var req guestagent.ForwardRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeForwardResponse(conn, fmt.Sprintf("decode forward request: %v", err))
		return
	}
	if req.Version != guestagent.ProtocolVersion {
		writeForwardResponse(conn, fmt.Sprintf("protocol mismatch: got %d want %d", req.Version, guestagent.ProtocolVersion))
		return
	}
	if !guestagent.ValidForwardTargetPort(req.Port) {
		writeForwardResponse(conn, fmt.Sprintf("port must be between %d and %d", guestagent.MinForwardTargetPort, guestagent.MaxForwardTargetPort))
		return
	}

	target, err := dialForwardTarget(req.Port)
	if err != nil {
		writeForwardResponse(conn, err.Error())
		return
	}
	defer target.Close()
	if !writeForwardResponse(conn, "") {
		return
	}
	spliceForward(conn, reader, target)
}

func dialForwardTarget(port int) (net.Conn, error) {
	dialer := net.Dialer{Timeout: forwardTargetDialTimeout}
	portText := strconv.Itoa(port)
	conn, err := dialer.Dial("tcp4", net.JoinHostPort("127.0.0.1", portText))
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("connect to loopback: %w", err)
	}
	conn, err = dialer.Dial("tcp6", net.JoinHostPort("::1", portText))
	if err == nil {
		return conn, nil
	}
	for _, unavailable := range []error{syscall.ECONNREFUSED, syscall.EADDRNOTAVAIL, syscall.ENETUNREACH, syscall.EAFNOSUPPORT} {
		if errors.Is(err, unavailable) {
			return nil, errors.New("connection refused on 127.0.0.1 and [::1]")
		}
	}
	return nil, fmt.Errorf("connection refused on 127.0.0.1; connect to [::1]: %w", err)
}

func writeForwardResponse(w io.Writer, message string) bool {
	resp := guestagent.ForwardResponse{
		Version: guestagent.ProtocolVersion,
		Error:   message,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("kiln-vm-agent forward write response: %v", err)
		return false
	}
	return true
}

func spliceForward(client net.Conn, clientReader io.Reader, target net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, clientReader)
		closeWrite(target)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, target)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = target.Close()
	<-done
}

func closeWrite(conn net.Conn) {
	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
}
