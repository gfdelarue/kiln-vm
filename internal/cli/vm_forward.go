package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/guestagent"
)

const (
	defaultVMForwardListen    = "127.0.0.1:0"
	vmForwardCopyBufferBytes  = 32 * 1024
	vmForwardCloseWriteGrace  = time.Second
	vmForwardAcceptBackoffMin = 5 * time.Millisecond
	vmForwardAcceptBackoffMax = time.Second
)

var vmForwardCmd = &cobra.Command{
	Use:   "forward <name-or-id> <port>",
	Short: "Forward a local TCP port to a port in a persistent VM, waking it when policy allows",
	Long: `Forward a local TCP port to a TCP port on the guest's loopback interface.

The command listens on --listen, which defaults to 127.0.0.1:0 (loopback, with a
free port chosen by the system). It prints the address it bound and serves until
interrupted. Each accepted connection opens its own tunnel through the kiln-vm API
and the guest agent, so the VM needs no ingress rule. Forwarding also works for
VMs with the isolated-web network policy.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := parseVMForwardPort(args[1])
		if err != nil {
			return err
		}
		listen, _ := cmd.Flags().GetString("listen")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runVMForward(ctx, vmForwardOptions{
			client: c,
			ref:    args[0],
			port:   port,
			listen: listen,
			dialer: managementWebSocketDialer(),
			stdout: os.Stdout,
			stderr: os.Stderr,
		})
	},
}

type vmForwardDialer interface {
	DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)
}

type vmForwardOptions struct {
	client *Client
	ref    string
	port   int
	listen string
	dialer vmForwardDialer
	stdout io.Writer
	stderr io.Writer
}

func parseVMForwardPort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || !guestagent.ValidForwardTargetPort(port) {
		return 0, fmt.Errorf("port must be an integer between %d and %d", guestagent.MinForwardTargetPort, guestagent.MaxForwardTargetPort)
	}
	return port, nil
}

func runVMForward(ctx context.Context, opts vmForwardOptions) error {
	if opts.client == nil {
		return errors.New("forward vm port: client is nil")
	}
	if !guestagent.ValidForwardTargetPort(opts.port) {
		return fmt.Errorf("port must be an integer between %d and %d", guestagent.MinForwardTargetPort, guestagent.MaxForwardTargetPort)
	}
	if err := validateAPIURL(opts.client.BaseURL, opts.client.AllowInsecureHTTP); err != nil {
		return err
	}
	wsURL, err := websocketURL(opts.client.BaseURL, "/vms/"+opts.ref+"/forward/"+strconv.Itoa(opts.port))
	if err != nil {
		return err
	}
	header := http.Header{}
	if opts.client.Token != "" {
		header.Set("Authorization", "Bearer "+opts.client.Token)
	}

	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.listen, err)
	}
	defer listener.Close()
	if _, err := fmt.Fprintf(opts.stdout, "forwarding %s to port %d in %s\n", listener.Addr(), opts.port, opts.ref); err != nil {
		return fmt.Errorf("report forward address: %w", err)
	}

	forwarder := &vmForwarder{
		dialer: opts.dialer,
		wsURL:  wsURL,
		header: header,
		logger: log.New(opts.stderr, "kiln-vm forward: ", 0),
	}
	stopListener := context.AfterFunc(ctx, func() { listener.Close() })
	defer stopListener()

	var sessions sync.WaitGroup
	defer sessions.Wait()
	backoff := time.Duration(0)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			backoff = min(max(2*backoff, vmForwardAcceptBackoffMin), vmForwardAcceptBackoffMax)
			forwarder.logger.Printf("accept: %v; retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			forwarder.serve(ctx, conn)
		}()
	}
}

type vmForwarder struct {
	dialer vmForwardDialer
	wsURL  string
	header http.Header
	logger *log.Logger
}

func (f *vmForwarder) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	peer := conn.RemoteAddr()
	wsConn, resp, err := f.dialer.DialContext(ctx, f.wsURL, f.header)
	if err != nil {
		if ctx.Err() == nil {
			f.logger.Printf("connection from %s: open VM forward: %s", peer, describeWebSocketDialError(err, resp))
		}
		return
	}
	defer wsConn.Close()
	stopSession := context.AfterFunc(ctx, func() {
		conn.Close()
		wsConn.Close()
	})
	defer stopSession()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, vmForwardCopyBufferBytes)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if writeErr := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				_ = wsConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(vmForwardCloseWriteGrace))
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) && closeErr.Code != websocket.CloseNormalClosure && ctx.Err() == nil {
					f.logger.Printf("connection from %s: VM forward closed: %s", peer, closeErr.Text)
				}
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
	<-done
	conn.Close()
	wsConn.Close()
	<-done
}

func describeWebSocketDialError(err error, resp *http.Response) string {
	if resp == nil {
		return err.Error()
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	var apiErr apiErrorResponse
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return fmt.Sprintf("API error (%d): %s", resp.StatusCode, formatAPIError(apiErr))
	}
	return fmt.Sprintf("%v: %s", err, resp.Status)
}
