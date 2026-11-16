package execwire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

const (
	MediaType = "application/vnd.kiln-vm.exec-duplex; version=1"
	Version   = 1

	FrameStart    = "start"
	FrameStdin    = "stdin"
	FrameStdinEOF = "stdin_eof"
	FrameStarted  = "started"
	FrameStdout   = "stdout"
	FrameStderr   = "stderr"
	FrameTerminal = "terminal"
)

type Start struct {
	Command string `json:"command"`
}

type Terminal struct {
	ExitCode  int                `json:"exit_code"`
	Category  execution.Category `json:"category,omitempty"`
	Message   string             `json:"message,omitempty"`
	Retryable bool               `json:"retryable,omitempty"`
	Ambiguous bool               `json:"ambiguous,omitempty"`
}

type RequestFrame struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
	Start   *Start `json:"start,omitempty"`
	Data    []byte `json:"data,omitempty"`
}

type ResponseFrame struct {
	Version  int       `json:"version"`
	Type     string    `json:"type"`
	Data     []byte    `json:"data,omitempty"`
	Terminal *Terminal `json:"terminal,omitempty"`
}

type Codec struct {
	reader *bufio.Reader
	writer io.Writer
}

func NewCodec(reader io.Reader, writer io.Writer) *Codec {
	var buffered *bufio.Reader
	if reader != nil {
		buffered = bufio.NewReaderSize(reader, execution.MaxEncodedFrameBytes+1)
	}
	return &Codec{reader: buffered, writer: writer}
}

func (c *Codec) ReadRequest() (RequestFrame, error) {
	var frame RequestFrame
	if err := c.read(&frame); err != nil {
		return frame, err
	}
	return frame, validateRequest(frame)
}

func (c *Codec) ReadResponse() (ResponseFrame, error) {
	var frame ResponseFrame
	if err := c.read(&frame); err != nil {
		return frame, err
	}
	return frame, validateResponse(frame)
}

func (c *Codec) WriteRequest(frame RequestFrame) error {
	if err := validateRequest(frame); err != nil {
		return err
	}
	return c.write(frame)
}

func (c *Codec) WriteResponse(frame ResponseFrame) error {
	if err := validateResponse(frame); err != nil {
		return err
	}
	return c.write(frame)
}

func (c *Codec) read(target any) error {
	if c.reader == nil {
		return errors.New("exec codec has no reader")
	}
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("exec frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	if err != nil {
		return fmt.Errorf("read exec frame: %w", err)
	}
	if len(line) > execution.MaxEncodedFrameBytes {
		return fmt.Errorf("exec frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	if err := json.Unmarshal(line, target); err != nil {
		return fmt.Errorf("decode exec frame: %w", err)
	}
	return nil
}

func (c *Codec) write(frame any) error {
	if c.writer == nil {
		return errors.New("exec codec has no writer")
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode exec frame: %w", err)
	}
	if len(data)+1 > execution.MaxEncodedFrameBytes {
		return fmt.Errorf("exec frame exceeds %d encoded bytes", execution.MaxEncodedFrameBytes)
	}
	data = append(data, '\n')
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("write exec frame: %w", err)
	}
	return nil
}

func validateRequest(frame RequestFrame) error {
	if frame.Version != Version {
		return fmt.Errorf("exec protocol mismatch: got %d want %d", frame.Version, Version)
	}
	switch frame.Type {
	case FrameStart:
		if frame.Start == nil {
			return errors.New("exec start payload is required")
		}
		if len(frame.Data) != 0 {
			return errors.New("exec start frame cannot contain data")
		}
		if err := execution.ValidateCommand(frame.Start.Command); err != nil {
			return fmt.Errorf("validate exec command: %w", err)
		}
		return nil
	case FrameStdin:
		if frame.Start != nil {
			return errors.New("exec stdin frame cannot contain start")
		}
		if len(frame.Data) > execution.MaxDataBytes {
			return fmt.Errorf("exec stdin data exceeds %d bytes", execution.MaxDataBytes)
		}
	case FrameStdinEOF:
		if frame.Start != nil || len(frame.Data) != 0 {
			return errors.New("exec stdin_eof frame cannot contain a payload")
		}
	default:
		return fmt.Errorf("unknown exec request frame %q", frame.Type)
	}
	return nil
}

func validateResponse(frame ResponseFrame) error {
	if frame.Version != Version {
		return fmt.Errorf("exec protocol mismatch: got %d want %d", frame.Version, Version)
	}
	switch frame.Type {
	case FrameStarted:
		if len(frame.Data) != 0 || frame.Terminal != nil {
			return errors.New("exec started frame cannot contain a payload")
		}
	case FrameStdout, FrameStderr:
		if frame.Terminal != nil {
			return errors.New("exec output frame cannot contain terminal")
		}
		if len(frame.Data) > execution.MaxDataBytes {
			return fmt.Errorf("exec output data exceeds %d bytes", execution.MaxDataBytes)
		}
	case FrameTerminal:
		if frame.Terminal == nil {
			return errors.New("exec terminal payload is required")
		}
		if len(frame.Data) != 0 {
			return errors.New("exec terminal frame cannot contain data")
		}
		if len(frame.Terminal.Message) > execution.MaxDiagnosticBytes {
			return fmt.Errorf("exec terminal diagnostic exceeds %d bytes", execution.MaxDiagnosticBytes)
		}
	default:
		return fmt.Errorf("unknown exec response frame %q", frame.Type)
	}
	return nil
}
