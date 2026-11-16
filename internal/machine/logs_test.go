package machine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConsoleLogWriterAppendsAcrossWriters(t *testing.T) {
	dataDir := t.TempDir()
	for _, line := range []string{"boot\n", "ready\n"} {
		w := consoleLogWriter(dataDir, "m-1")
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "machines", "m-1", "console.log"))
	if err != nil {
		t.Fatalf("read console log: %v", err)
	}
	if string(got) != "boot\nready\n" {
		t.Fatalf("console log = %q, want %q", got, "boot\nready\n")
	}
}
