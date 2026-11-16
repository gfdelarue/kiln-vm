package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"github.com/gfdelarue/kiln-vm/internal/guestagent"
)

func openShell(c *Client, path string) error {
	return openShellWithDialer(c, path, managementWebSocketDialer())
}

func managementWebSocketDialer() *websocket.Dialer {
	dialer := *websocket.DefaultDialer
	dialer.Proxy = proxyWithManagementLoopbackBypass(dialer.Proxy)
	return &dialer
}

type shellDialer interface {
	Dial(urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)
}

func openShellWithDialer(c *Client, path string, dialer shellDialer) error {
	if c == nil {
		return fmt.Errorf("connect shell: client is nil")
	}
	if err := validateAPIURL(c.BaseURL, c.AllowInsecureHTTP); err != nil {
		return err
	}
	wsURL, err := websocketURL(c.BaseURL, path)
	if err != nil {
		return err
	}

	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	wsConn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		return fmt.Errorf("connect shell: %w", err)
	}
	defer wsConn.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set raw terminal: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	if err := wsConn.WriteJSON(guestagent.ShellFrame{
		Type:    "start",
		Version: guestagent.ProtocolVersion,
		Cols:    uint16(cols),
		Rows:    uint16(rows),
	}); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	go func() {
		for range sigCh {
			cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil {
				continue
			}
			_ = wsConn.WriteJSON(guestagent.ShellFrame{
				Type: "resize",
				Cols: uint16(cols),
				Rows: uint16(rows),
			})
		}
	}()

	errCh := make(chan error, 2)
	exitCh := make(chan int, 1)

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if writeErr := wsConn.WriteJSON(guestagent.ShellFrame{
					Type: "stdin",
					Data: append([]byte(nil), buf[:n]...),
				}); writeErr != nil {
					errCh <- writeErr
					return
				}
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}

			var frame guestagent.ShellFrame
			if err := json.Unmarshal(msg, &frame); err != nil {
				errCh <- err
				return
			}

			switch frame.Type {
			case "stdout", "stderr":
				if _, err := os.Stdout.Write(frame.Data); err != nil {
					errCh <- err
					return
				}
			case "error":
				errCh <- errors.New(frame.Error)
				return
			case "exit":
				exitCh <- frame.ExitCode
				return
			}
		}
	}()

	select {
	case code := <-exitCh:
		if code != 0 {
			os.Exit(code)
		}
		return nil
	case err := <-errCh:
		if err == nil ||
			err.Error() == "EOF" ||
			strings.Contains(err.Error(), "close 1000") ||
			strings.Contains(err.Error(), "unexpected EOF") {
			return nil
		}
		return err
	}
}

func websocketURL(baseURL, path string) (string, error) {
	u, err := url.Parse(strings.TrimRight(canonicalizeLocalhostURLString(baseURL), "/"))
	if err != nil {
		return "", fmt.Errorf("parse API URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported API scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}
