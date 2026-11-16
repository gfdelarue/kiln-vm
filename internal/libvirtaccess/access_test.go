package libvirtaccess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareReadOnlyFileAddsAncestorExecuteWithoutRead(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locked, "media.iso")
	if err := os.WriteFile(path, []byte("iso"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := PrepareReadOnlyFile(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(locked)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0711 {
		t.Fatalf("ancestor mode = %o, want 0711", got)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Fatalf("read-only file mode = %o, want 0644", got)
	}
}

func TestPrepareWritableFileKeepsWritableFallbackForNonRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "disk.qcow2")
	if err := os.WriteFile(path, []byte("disk"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := PrepareWritableFile(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupRuntimeIdentity(); os.Geteuid() == 0 && ok {
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("root-owned writable mode = %o, want 0600", got)
		}
		return
	}
	if got := info.Mode().Perm(); got != 0666 {
		t.Fatalf("non-root writable mode = %o, want 0666", got)
	}
}
