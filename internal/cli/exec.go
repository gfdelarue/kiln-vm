package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/execwire"
)

type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.Code)
}

func executeRemoteCommand(c *Client, path, command string) error {
	return executeRemoteCommandTo(
		context.Background(),
		c,
		path,
		command,
		redirectedExecInput(os.Stdin),
		os.Stdout,
		os.Stderr,
	)
}

func executeRemoteCommandTo(
	ctx context.Context,
	c *Client,
	path string,
	command string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	outcome, err := c.ExecDuplex(ctx, path, execution.Request{
		Command: command,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err != nil {
		return err
	}
	if outcome.Category != execution.CategoryNone {
		return fmt.Errorf("remote exec failed: %w", execution.ErrorFromOutcome(outcome))
	}
	if outcome.ExitCode != 0 {
		return &ExitError{Code: outcome.ExitCode}
	}
	return nil
}

func (c *Client) ExecDuplex(ctx context.Context, path string, request execution.Request) (execution.Outcome, error) {
	if err := execution.ValidateCommand(request.Command); err != nil {
		return execution.Outcome{}, fmt.Errorf("validate exec command: %w", err)
	}

	bodyReader, bodyWriter := io.Pipe()
	codec := execwire.NewCodec(nil, bodyWriter)
	releaseBody := make(chan struct{})
	go func() {
		err := writeHTTPExecRequest(codec, request.Command, request.Stdin)
		if err != nil {
			_ = bodyWriter.CloseWithError(err)
			return
		}
		select {
		case <-ctx.Done():
		case <-releaseBody:
		}
		_ = bodyWriter.Close()
	}()
	defer close(releaseBody)

	httpRequest, err := c.newRequest(http.MethodPost, path, bodyReader)
	if err != nil {
		_ = bodyReader.Close()
		return execution.Outcome{}, err
	}
	httpRequest = httpRequest.WithContext(ctx)
	httpRequest.Header.Set("Content-Type", execwire.MediaType)
	httpRequest.Header.Set("Accept", execwire.MediaType)

	response, err := c.execHTTPClient().Do(httpRequest)
	if err != nil {
		_ = bodyReader.Close()
		return execution.Outcome{}, fmt.Errorf("exec request failed: %w", sanitizeHTTPClientError(err))
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return execution.Outcome{}, fmt.Errorf("exec redirect %d refused because commands cannot be replayed safely", response.StatusCode)
	}
	if response.StatusCode >= 400 {
		return execution.Outcome{}, decodeExecAPIError(response)
	}
	if strings.TrimSpace(response.Header.Get("Content-Type")) != execwire.MediaType {
		return execution.Outcome{}, fmt.Errorf("exec response content type = %q, want %q", response.Header.Get("Content-Type"), execwire.MediaType)
	}

	responseCodec := execwire.NewCodec(response.Body, nil)
	started := false
	for {
		frame, err := responseCodec.ReadResponse()
		if err != nil {
			return execution.Outcome{}, fmt.Errorf("read exec response: %w", err)
		}
		switch frame.Type {
		case execwire.FrameStarted:
			if started {
				return execution.Outcome{}, errors.New("duplicate exec started frame")
			}
			started = true
			if request.Started != nil {
				if err := request.Started(); err != nil {
					return execution.Outcome{}, fmt.Errorf("publish exec started: %w", err)
				}
			}
		case execwire.FrameStdout, execwire.FrameStderr:
			if !started {
				return execution.Outcome{}, errors.New("exec output arrived before started")
			}
			destination := request.Stdout
			if frame.Type == execwire.FrameStderr {
				destination = request.Stderr
			}
			if destination == nil {
				destination = io.Discard
			}
			if _, err := destination.Write(frame.Data); err != nil {
				return execution.Outcome{}, fmt.Errorf("write exec output: %w", err)
			}
		case execwire.FrameTerminal:
			if !started {
				return execution.Outcome{}, errors.New("exec terminal arrived before started")
			}
			return execution.Outcome{
				ExitCode:  frame.Terminal.ExitCode,
				Category:  frame.Terminal.Category,
				Message:   frame.Terminal.Message,
				Retryable: frame.Terminal.Retryable,
				Ambiguous: frame.Terminal.Ambiguous,
			}, nil
		}
	}
}

func writeHTTPExecRequest(codec *execwire.Codec, command string, stdin io.Reader) error {
	if err := codec.WriteRequest(execwire.RequestFrame{
		Version: execwire.Version,
		Type:    execwire.FrameStart,
		Start:   &execwire.Start{Command: command},
	}); err != nil {
		return fmt.Errorf("write exec start: %w", err)
	}
	if stdin != nil {
		buffer := make([]byte, execution.MaxDataBytes)
		for {
			n, err := stdin.Read(buffer)
			if n > 0 {
				if writeErr := codec.WriteRequest(execwire.RequestFrame{
					Version: execwire.Version,
					Type:    execwire.FrameStdin,
					Data:    append([]byte(nil), buffer[:n]...),
				}); writeErr != nil {
					return fmt.Errorf("write exec stdin: %w", writeErr)
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("read exec stdin: %w", err)
				}
				break
			}
		}
	}
	if err := codec.WriteRequest(execwire.RequestFrame{Version: execwire.Version, Type: execwire.FrameStdinEOF}); err != nil {
		return fmt.Errorf("write exec stdin EOF: %w", err)
	}
	return nil
}

func (c *Client) execHTTPClient() *http.Client {
	client := c.requestHTTPClient(true)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("exec redirects are refused because commands cannot be replayed safely")
	}
	return client
}

func decodeExecAPIError(response *http.Response) error {
	var apiErr apiErrorResponse
	body, _ := io.ReadAll(io.LimitReader(response.Body, execution.MaxDiagnosticBytes*2))
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return fmt.Errorf("API error (%d): %s", response.StatusCode, formatAPIError(apiErr))
	}
	return fmt.Errorf("API error: %s", response.Status)
}

func redirectedExecInput(stdin *os.File) io.Reader {
	if stdin == nil {
		return nil
	}
	info, err := stdin.Stat()
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return stdin
}
