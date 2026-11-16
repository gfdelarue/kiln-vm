package cli

import (
	"os"
	"testing"
)

func TestRedirectedExecInputSelectsFilesAndPipesButNotCharacterDevices(t *testing.T) {
	regular, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create regular stdin: %v", err)
	}
	defer regular.Close()
	if input := redirectedExecInput(regular); input != regular {
		t.Fatalf("regular input = %T, want file", input)
	}

	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	defer pipeReader.Close()
	defer pipeWriter.Close()
	if input := redirectedExecInput(pipeReader); input != pipeReader {
		t.Fatalf("pipe input = %T, want pipe", input)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	if input := redirectedExecInput(devNull); input != nil {
		t.Fatalf("character-device input = %T, want nil", input)
	}
	if input := redirectedExecInput(nil); input != nil {
		t.Fatalf("nil input = %T, want nil", input)
	}
}
