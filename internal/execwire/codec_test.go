package execwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

func TestStartNestsCommand(t *testing.T) {
	var output bytes.Buffer
	if err := NewCodec(nil, &output).WriteRequest(RequestFrame{
		Version: Version,
		Type:    FrameStart,
		Start:   &Start{Command: "true"},
	}); err != nil {
		t.Fatalf("write start: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &raw); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if _, exists := raw["command"]; exists {
		t.Fatal("root command field must remain absent")
	}
}

func TestCodecRejectsDirectionAndBounds(t *testing.T) {
	if _, err := NewCodec(strings.NewReader(`{"version":1,"type":"stdout"}`+"\n"), nil).ReadRequest(); err == nil {
		t.Fatal("response frame accepted as request")
	}
	if err := NewCodec(nil, &bytes.Buffer{}).WriteResponse(ResponseFrame{
		Version: Version,
		Type:    FrameStdout,
		Data:    make([]byte, execution.MaxDataBytes+1),
	}); err == nil {
		t.Fatal("oversized response data succeeded")
	}
}
