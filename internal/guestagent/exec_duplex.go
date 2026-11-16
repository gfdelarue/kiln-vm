package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

const (
	ExecDuplexPort             = 10706
	ExecDuplexProtocolVersion  = 2
	ExecDuplexHandshakeTimeout = 5 * time.Second

	ExecDuplexFrameStart    = "start"
	ExecDuplexFrameStdin    = "stdin"
	ExecDuplexFrameStdinEOF = "stdin_eof"
	ExecDuplexFrameReady    = "ready"
	ExecDuplexFrameStarted  = "started"
	ExecDuplexFrameStdout   = "stdout"
	ExecDuplexFrameStderr   = "stderr"
	ExecDuplexFrameTerminal = "terminal"
)

type ExecDuplexStart struct {
	Command string `json:"command"`
}

type ExecDuplexTerminal struct {
	ExitCode  int                `json:"exit_code"`
	Category  execution.Category `json:"category,omitempty"`
	Message   string             `json:"message,omitempty"`
	Retryable bool               `json:"retryable,omitempty"`
	Ambiguous bool               `json:"ambiguous,omitempty"`
}

type ExecDuplexRequestFrame struct {
	Version int              `json:"version"`
	Type    string           `json:"type"`
	Start   *ExecDuplexStart `json:"start,omitempty"`
	Data    []byte           `json:"data,omitempty"`
}

type ExecDuplexResponseFrame struct {
	Version  int                 `json:"version"`
	Type     string              `json:"type"`
	Data     []byte              `json:"data,omitempty"`
	Terminal *ExecDuplexTerminal `json:"terminal,omitempty"`
}

type ExecDuplexCodec struct {
	reader *bufio.Reader
	writer io.Writer
}

func NewExecDuplexCodec(reader io.Reader, writer io.Writer) *ExecDuplexCodec {
	var buffered *bufio.Reader
	if reader != nil {
		if existing, ok := reader.(*bufio.Reader); ok {
			buffered = existing
		} else {
			buffered = bufio.NewReaderSize(reader, execution.MaxEncodedFrameBytes+1)
		}
	}
	return &ExecDuplexCodec{reader: buffered, writer: writer}
}

func (c *ExecDuplexCodec) ReadRequest() (ExecDuplexRequestFrame, error) {
	var frame ExecDuplexRequestFrame
	if err := c.read(&frame); err != nil {
		return frame, err
	}
	if err := validateRequestFrame(frame); err != nil {
		return frame, err
	}
	return frame, nil
}

func (c *ExecDuplexCodec) ReadResponse() (ExecDuplexResponseFrame, error) {
	var frame ExecDuplexResponseFrame
	if err := c.read(&frame); err != nil {
		return frame, err
	}
	if err := validateResponseFrame(frame); err != nil {
		return frame, err
	}
	return frame, nil
}

func (c *ExecDuplexCodec) WriteRequest(frame ExecDuplexRequestFrame) error {
	if err := validateRequestFrame(frame); err != nil {
		return err
	}
	return c.write(frame)
}

func (c *ExecDuplexCodec) WriteResponse(frame ExecDuplexResponseFrame) error {
	if err := validateResponseFrame(frame); err != nil {
		return err
	}
	return c.write(frame)
}

func (c *ExecDuplexCodec) read(target any) error {
	if c.reader == nil {
		return errors.New("exec duplex codec has no reader")
	}
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("exec duplex frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	if err != nil {
		return fmt.Errorf("read exec duplex frame: %w", err)
	}
	if len(line) > execution.MaxEncodedFrameBytes {
		return fmt.Errorf("exec duplex frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	if err := json.Unmarshal(line, target); err != nil {
		return fmt.Errorf("decode exec duplex frame: %w", err)
	}
	return nil
}

func (c *ExecDuplexCodec) write(frame any) error {
	if c.writer == nil {
		return errors.New("exec duplex codec has no writer")
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode exec duplex frame: %w", err)
	}
	if len(data)+1 > execution.MaxEncodedFrameBytes {
		return fmt.Errorf("exec duplex frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	data = append(data, '\n')
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("write exec duplex frame: %w", err)
	}
	return nil
}

func validateRequestFrame(frame ExecDuplexRequestFrame) error {
	if frame.Version != ExecDuplexProtocolVersion {
		return fmt.Errorf("exec duplex protocol mismatch: got %d want %d", frame.Version, ExecDuplexProtocolVersion)
	}
	switch frame.Type {
	case ExecDuplexFrameStart:
		if frame.Start == nil {
			return errors.New("exec duplex start payload is required")
		}
		if len(frame.Data) != 0 {
			return errors.New("exec duplex start frame cannot contain data")
		}
		if err := execution.ValidateCommand(frame.Start.Command); err != nil {
			return fmt.Errorf("validate exec duplex command: %w", err)
		}
		return nil
	case ExecDuplexFrameStdin:
		if frame.Start != nil {
			return errors.New("exec duplex stdin frame cannot contain start")
		}
		if len(frame.Data) > execution.MaxDataBytes {
			return fmt.Errorf("exec duplex stdin data exceeds %d bytes", execution.MaxDataBytes)
		}
		return nil
	case ExecDuplexFrameStdinEOF:
		if frame.Start != nil || len(frame.Data) != 0 {
			return errors.New("exec duplex stdin_eof frame cannot contain a payload")
		}
		return nil
	default:
		return fmt.Errorf("unknown exec duplex request frame %q", frame.Type)
	}
}

func validateResponseFrame(frame ExecDuplexResponseFrame) error {
	if frame.Version != ExecDuplexProtocolVersion {
		return fmt.Errorf("exec duplex protocol mismatch: got %d want %d", frame.Version, ExecDuplexProtocolVersion)
	}
	switch frame.Type {
	case ExecDuplexFrameReady:
		if len(frame.Data) != 0 || frame.Terminal != nil {
			return errors.New("exec duplex ready frame cannot contain a payload")
		}
	case ExecDuplexFrameStarted:
		if len(frame.Data) != 0 || frame.Terminal != nil {
			return errors.New("exec duplex started frame cannot contain a payload")
		}
	case ExecDuplexFrameStdout, ExecDuplexFrameStderr:
		if frame.Terminal != nil {
			return errors.New("exec duplex output frame cannot contain terminal")
		}
		if len(frame.Data) > execution.MaxDataBytes {
			return fmt.Errorf("exec duplex output data exceeds %d bytes", execution.MaxDataBytes)
		}
	case ExecDuplexFrameTerminal:
		if frame.Terminal == nil {
			return errors.New("exec duplex terminal payload is required")
		}
		if len(frame.Data) != 0 {
			return errors.New("exec duplex terminal frame cannot contain data")
		}
		if len(frame.Terminal.Message) > execution.MaxDiagnosticBytes {
			return fmt.Errorf("exec duplex terminal diagnostic exceeds %d bytes", execution.MaxDiagnosticBytes)
		}
	default:
		return fmt.Errorf("unknown exec duplex response frame %q", frame.Type)
	}
	return nil
}

func Execute(ctx context.Context, udsPath string, request execution.Request) execution.Outcome {
	if err := execution.ValidateCommand(request.Command); err != nil {
		return execution.OutcomeFromError(&execution.Error{Category: execution.CategoryProtocolError, Message: err.Error(), Cause: err})
	}
	conn, err := dialGuest(ctx, udsPath, ExecDuplexPort)
	if err != nil {
		if !strings.Contains(err.Error(), "unexpected guest vsock ack") {
			return execution.OutcomeFromError(&execution.Error{
				Category:  execution.CategoryTransportUnavailable,
				Message:   execution.Diagnostic(fmt.Sprintf("guest exec transport unavailable: %v", err)),
				Retryable: true,
				Cause:     err,
			})
		}
		return execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryRuntimeIncompatible,
			Message:  "guest exec runtime is incompatible; recreate the VM or rebuild its profile with the current kiln-vm release",
			Cause:    err,
		})
	}
	defer conn.Close()
	stopClose := closeOnContextCancel(ctx, conn)
	defer stopClose()
	if err := conn.SetDeadline(time.Now().Add(ExecDuplexHandshakeTimeout)); err != nil {
		return execution.OutcomeFromError(fmt.Errorf("set guest exec handshake deadline: %w", err))
	}

	codec := NewExecDuplexCodec(conn, conn)
	ready, err := codec.ReadResponse()
	if err != nil {
		if ctx.Err() != nil {
			return execution.OutcomeFromError(context.Cause(ctx))
		}
		return execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryRuntimeIncompatible,
			Message:  "guest exec runtime is incompatible; recreate the VM or rebuild its profile with the current kiln-vm release",
			Cause:    fmt.Errorf("read guest exec ready frame: %w", err),
		})
	}
	if ready.Type != ExecDuplexFrameReady {
		return execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryRuntimeIncompatible,
			Message:  "guest exec runtime is incompatible; recreate the VM or rebuild its profile with the current kiln-vm release",
			Cause:    fmt.Errorf("expected guest exec ready frame, got %q", ready.Type),
		})
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Time{})
	}

	if err := codec.WriteRequest(ExecDuplexRequestFrame{
		Version: ExecDuplexProtocolVersion,
		Type:    ExecDuplexFrameStart,
		Start:   &ExecDuplexStart{Command: request.Command},
	}); err != nil {
		return duplexTransportOutcome(ctx, false, "send start frame", err)
	}

	inputErr := make(chan error, 1)
	go func() {
		sendErr := sendExecDuplexInput(codec, request.Stdin)
		inputErr <- sendErr
		if sendErr != nil {
			_ = conn.Close()
		}
	}()

	started := false
	for {
		frame, err := codec.ReadResponse()
		if err != nil {
			select {
			case sendErr := <-inputErr:
				if sendErr != nil {
					return duplexTransportOutcome(ctx, started, "send request frame", sendErr)
				}
			default:
			}
			return duplexTransportOutcome(ctx, started, "read response frame", err)
		}
		switch frame.Type {
		case ExecDuplexFrameStarted:
			if started {
				return execution.OutcomeFromError(&execution.Error{Category: execution.CategoryProtocolError, Message: "duplicate exec started frame"})
			}
			started = true
			if request.Started != nil {
				if err := request.Started(); err != nil {
					return execution.OutcomeFromError(err)
				}
			}
		case ExecDuplexFrameStdout, ExecDuplexFrameStderr:
			if !started {
				return execution.OutcomeFromError(&execution.Error{Category: execution.CategoryProtocolError, Message: "exec output arrived before started"})
			}
			destination := request.Stdout
			if frame.Type == ExecDuplexFrameStderr {
				destination = request.Stderr
			}
			if destination == nil {
				destination = io.Discard
			}
			if _, err := destination.Write(frame.Data); err != nil {
				return execution.OutcomeFromError(fmt.Errorf("write exec %s: %w", strings.TrimPrefix(frame.Type, "std"), err))
			}
		case ExecDuplexFrameTerminal:
			if !started && frame.Terminal.Category == execution.CategoryNone {
				return execution.OutcomeFromError(&execution.Error{Category: execution.CategoryProtocolError, Message: "exec terminal arrived before started"})
			}
			terminal := frame.Terminal
			return execution.Outcome{
				ExitCode:  terminal.ExitCode,
				Category:  terminal.Category,
				Message:   terminal.Message,
				Retryable: terminal.Retryable,
				Ambiguous: terminal.Ambiguous,
			}
		}
	}
}

func sendExecDuplexInput(codec *ExecDuplexCodec, input io.Reader) error {
	if input != nil {
		buffer := make([]byte, execution.MaxDataBytes)
		for {
			n, err := input.Read(buffer)
			if n > 0 {
				if writeErr := codec.WriteRequest(ExecDuplexRequestFrame{
					Version: ExecDuplexProtocolVersion,
					Type:    ExecDuplexFrameStdin,
					Data:    append([]byte(nil), buffer[:n]...),
				}); writeErr != nil {
					return writeErr
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
	return codec.WriteRequest(ExecDuplexRequestFrame{
		Version: ExecDuplexProtocolVersion,
		Type:    ExecDuplexFrameStdinEOF,
	})
}

func duplexTransportOutcome(ctx context.Context, started bool, operation string, err error) execution.Outcome {
	if ctx.Err() != nil {
		return execution.OutcomeFromError(context.Cause(ctx))
	}
	category := execution.CategoryTransportUnavailable
	ambiguous := false
	retryable := true
	if started {
		category = execution.CategoryTransportLost
		ambiguous = true
		retryable = false
	}
	return execution.OutcomeFromError(&execution.Error{
		Category:  category,
		Message:   execution.Diagnostic(fmt.Sprintf("guest exec %s: %v", operation, err)),
		Retryable: retryable,
		Ambiguous: ambiguous,
		Cause:     err,
	})
}
