package isofs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWriteProducesAJolietTreeWithTheOriginalNames(t *testing.T) {
	dir := t.TempDir()
	want := map[string][]byte{
		"payload/manifest.json":     []byte("{\"openssh\":{}}\n"),
		"payload/OpenSSH-Win64.zip": bytes.Repeat([]byte("zip"), 3000),
		"payload/nested/empty.txt":  {},
		"readme.txt":                []byte("hello"),
	}
	var files []File
	for name, content := range want {
		source := filepath.Join(dir, strings.ReplaceAll(name, "/", "_"))
		if err := os.WriteFile(source, content, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, File{Path: name, Source: source})
	}
	target := filepath.Join(dir, "out.iso")
	if err := Write(target, "KILNVMDATA", files); err != nil {
		t.Fatalf("Write: %v", err)
	}
	image, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(image)%sectorSize != 0 {
		t.Fatalf("image is %d bytes, want a multiple of %d", len(image), sectorSize)
	}

	primary := sector(image, 16)
	if primary[0] != 1 || string(primary[1:6]) != "CD001" {
		t.Fatalf("sector 16 is not a primary volume descriptor")
	}
	if got := strings.TrimSpace(string(primary[40:72])); got != "KILNVMDATA" {
		t.Fatalf("volume id = %q, want KILNVMDATA", got)
	}
	if got := binary.LittleEndian.Uint32(primary[80:]); int(got)*sectorSize != len(image) {
		t.Fatalf("volume space = %d sectors, image has %d", got, len(image)/sectorSize)
	}
	joliet := sector(image, 17)
	if joliet[0] != 2 || string(joliet[88:91]) != "%/E" {
		t.Fatalf("sector 17 is not a Joliet supplementary volume descriptor")
	}
	if terminator := sector(image, 18); terminator[0] != 255 {
		t.Fatalf("sector 18 is not a volume descriptor terminator")
	}

	got := map[string][]byte{}
	walk(t, image, joliet[156:190], "", decodeJoliet, got)
	if len(got) != len(want) {
		t.Fatalf("joliet tree has %d files, want %d: %v", len(got), len(want), keys(got))
	}
	for name, content := range want {
		if !bytes.Equal(got[name], content) {
			t.Fatalf("joliet file %s = %d bytes, want %d", name, len(got[name]), len(content))
		}
	}

	primaryFiles := map[string][]byte{}
	walk(t, image, primary[156:190], "", func(b []byte) string { return string(b) }, primaryFiles)
	if content := primaryFiles["PAYLOAD/OPENSSH_.ZIP;1"]; !bytes.Equal(content, want["payload/OpenSSH-Win64.zip"]) {
		t.Fatalf("primary tree lacks PAYLOAD/OPENSSH_.ZIP;1: %v", keys(primaryFiles))
	}
}

func TestWriteRejectsCollidingPrimaryNames(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "a")
	if err := os.WriteFile(source, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Write(filepath.Join(dir, "out.iso"), "X", []File{
		{Path: "longname-one.txt", Source: source},
		{Path: "longname-two.txt", Source: source},
	})
	if err == nil || !strings.Contains(err.Error(), "collide") {
		t.Fatalf("Write error = %v, want a collision error", err)
	}
}

func TestWriteRejectsDuplicatePaths(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "a")
	if err := os.WriteFile(source, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Write(filepath.Join(dir, "out.iso"), "X", []File{
		{Path: "a/b", Source: source},
		{Path: "a/b", Source: source},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Write error = %v, want a duplicate error", err)
	}
}

func sector(image []byte, lba uint32) []byte {
	return image[int(lba)*sectorSize : int(lba+1)*sectorSize]
}

func walk(t *testing.T, image, record []byte, prefix string, decode func([]byte) string, out map[string][]byte) {
	t.Helper()
	extent := binary.LittleEndian.Uint32(record[2:])
	size := binary.LittleEndian.Uint32(record[10:])
	if binary.BigEndian.Uint32(record[6:]) != extent || binary.BigEndian.Uint32(record[14:]) != size {
		t.Fatalf("both-endian fields disagree in record for %q", prefix)
	}
	data := image[int(extent)*sectorSize : int(extent)*sectorSize+int(size)]
	for offset := 0; offset < len(data); {
		length := int(data[offset])
		if length == 0 {
			offset = (offset/sectorSize + 1) * sectorSize
			continue
		}
		entry := data[offset : offset+length]
		offset += length
		name := entry[33 : 33+int(entry[32])]
		if len(name) == 1 && name[0] <= 1 {
			continue
		}
		full := prefix + decode(name)
		if entry[25]&2 != 0 {
			walk(t, image, entry, full+"/", decode, out)
			continue
		}
		fileExtent := binary.LittleEndian.Uint32(entry[2:])
		fileSize := binary.LittleEndian.Uint32(entry[10:])
		out[full] = image[int(fileExtent)*sectorSize : int(fileExtent)*sectorSize+int(fileSize)]
	}
}

func decodeJoliet(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return strings.TrimSuffix(string(utf16.Decode(units)), ";1")
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
