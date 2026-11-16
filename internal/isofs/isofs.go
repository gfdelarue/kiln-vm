// Package isofs writes small read-only ISO 9660 images with Joliet names.
package isofs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const sectorSize = 2048

// File places the contents of Source at Path, a slash-separated path inside the image.
type File struct {
	Path   string
	Source string
}

type tree int

const (
	primaryTree tree = iota
	jolietTree
)

var trees = [...]tree{primaryTree, jolietTree}

type node struct {
	name     string
	parent   *node
	children []*node
	source   string
	size     int64
	extent   uint32
	number   uint16
	dirLBA   [2]uint32
	dirSize  [2]uint32
	isoNames [2][]byte
}

func (n *node) isDir() bool { return n.source == "" }

// Write creates target as an ISO 9660 image holding files under volumeID.
func Write(target, volumeID string, files []File) error {
	root, err := buildTree(files)
	if err != nil {
		return err
	}
	dirs := directoriesInPathTableOrder(root)
	if len(dirs) > 0xffff {
		return errors.New("too many directories")
	}
	for i, dir := range dirs {
		dir.number = uint16(i + 1)
		for _, t := range trees {
			sortChildren(dir, t)
		}
	}

	next := uint32(19)
	var pathTableSize [2]uint32
	var lTable, mTable [2]uint32
	for _, t := range trees {
		pathTableSize[t] = uint32(len(pathTable(dirs, t, binary.LittleEndian)))
		sectors := sectorsFor(int64(pathTableSize[t]))
		lTable[t] = next
		mTable[t] = next + sectors
		next += 2 * sectors
	}
	for _, t := range trees {
		for _, dir := range dirs {
			dir.dirSize[t] = uint32(len(directoryRecords(dir, t, time.Time{})))
			dir.dirLBA[t] = next
			next += dir.dirSize[t] / sectorSize
		}
	}
	for _, f := range filesInOrder(root) {
		f.extent = next
		next += sectorsFor(f.size)
	}

	out, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create iso image: %w", err)
	}
	w := bufio.NewWriter(out)
	now := time.Now().UTC()
	if err := writeImage(w, root, dirs, volumeID, next, pathTableSize, lTable, mTable, now); err != nil {
		_ = out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = out.Close()
		return fmt.Errorf("write iso image: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close iso image: %w", err)
	}
	return nil
}

func writeImage(w io.Writer, root *node, dirs []*node, volumeID string, totalSectors uint32, pathTableSize, lTable, mTable [2]uint32, now time.Time) error {
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("write iso image: %w", err)
		}
		return nil
	}
	if err := write(make([]byte, 16*sectorSize)); err != nil {
		return err
	}
	for _, t := range trees {
		if err := write(volumeDescriptor(t, volumeID, root, totalSectors, pathTableSize[t], lTable[t], mTable[t], now)); err != nil {
			return err
		}
	}
	terminator := make([]byte, sectorSize)
	terminator[0] = 255
	copy(terminator[1:], "CD001")
	terminator[6] = 1
	if err := write(terminator); err != nil {
		return err
	}
	for _, t := range trees {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			if err := write(padToSector(pathTable(dirs, t, order))); err != nil {
				return err
			}
		}
	}
	for _, t := range trees {
		for _, dir := range dirs {
			if err := write(directoryRecords(dir, t, now)); err != nil {
				return err
			}
		}
	}
	for _, f := range filesInOrder(root) {
		if err := copyFile(w, f); err != nil {
			return err
		}
	}
	return nil
}

func buildTree(files []File) (*node, error) {
	root := &node{}
	for _, f := range files {
		clean := path.Clean(strings.TrimPrefix(f.Path, "/"))
		if f.Path == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
			return nil, fmt.Errorf("invalid iso path %q", f.Path)
		}
		info, err := os.Stat(f.Source)
		if err != nil {
			return nil, fmt.Errorf("stat iso source: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("iso source %s is not a regular file", f.Source)
		}
		if info.Size() > 0xffffffff {
			return nil, fmt.Errorf("iso source %s is larger than 4 GiB", f.Source)
		}
		parts := strings.Split(clean, "/")
		dir := root
		for _, part := range parts[:len(parts)-1] {
			child := findChild(dir, part)
			if child == nil {
				child = &node{name: part, parent: dir}
				dir.children = append(dir.children, child)
			} else if !child.isDir() {
				return nil, fmt.Errorf("iso path %q uses file %q as a directory", f.Path, part)
			}
			dir = child
		}
		name := parts[len(parts)-1]
		if findChild(dir, name) != nil {
			return nil, fmt.Errorf("duplicate iso path %q", f.Path)
		}
		dir.children = append(dir.children, &node{name: name, parent: dir, source: f.Source, size: info.Size()})
	}
	return root, assignNames(root)
}

func findChild(dir *node, name string) *node {
	for _, child := range dir.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func assignNames(dir *node) error {
	seen := map[string]bool{}
	for _, child := range dir.children {
		primary := primaryName(child.name, child.isDir())
		if seen[primary] {
			return fmt.Errorf("iso names in %q collide as %s", dir.name, primary)
		}
		seen[primary] = true
		child.isoNames[primaryTree] = []byte(primary)
		joliet, err := jolietName(child.name, child.isDir())
		if err != nil {
			return err
		}
		child.isoNames[jolietTree] = joliet
		if child.isDir() {
			if err := assignNames(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func primaryName(name string, isDir bool) string {
	base, ext := name, ""
	if !isDir {
		if i := strings.LastIndex(name, "."); i > 0 {
			base, ext = name[:i], name[i+1:]
		}
	}
	base, ext = dChars(base, 8), dChars(ext, 3)
	if base == "" {
		base = "_"
	}
	if isDir {
		return base
	}
	return base + "." + ext + ";1"
}

func dChars(value string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(value) {
		if b.Len() == limit {
			break
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func jolietName(name string, isDir bool) ([]byte, error) {
	if !isDir {
		name += ";1"
	}
	units := utf16.Encode([]rune(name))
	if len(units) > 64 {
		return nil, fmt.Errorf("iso name %q is longer than 64 characters", name)
	}
	out := make([]byte, 2*len(units))
	for i, u := range units {
		binary.BigEndian.PutUint16(out[2*i:], u)
	}
	return out, nil
}

func sortChildren(dir *node, t tree) {
	sort.SliceStable(dir.children, func(i, j int) bool {
		return string(dir.children[i].isoNames[t]) < string(dir.children[j].isoNames[t])
	})
}

func directoriesInPathTableOrder(root *node) []*node {
	dirs := []*node{root}
	for i := 0; i < len(dirs); i++ {
		children := make([]*node, 0, len(dirs[i].children))
		for _, child := range dirs[i].children {
			if child.isDir() {
				children = append(children, child)
			}
		}
		sort.Slice(children, func(a, b int) bool { return children[a].name < children[b].name })
		dirs = append(dirs, children...)
	}
	return dirs
}

func filesInOrder(dir *node) []*node {
	var files []*node
	for _, child := range dir.children {
		if child.isDir() {
			files = append(files, filesInOrder(child)...)
		} else {
			files = append(files, child)
		}
	}
	return files
}

func pathTable(dirs []*node, t tree, order binary.ByteOrder) []byte {
	var out []byte
	for _, dir := range dirs {
		name := []byte{0}
		parent := uint16(1)
		if dir.parent != nil {
			name = dir.isoNames[t]
			parent = dir.parent.number
		}
		entry := make([]byte, 8+len(name)+len(name)%2)
		entry[0] = byte(len(name))
		order.PutUint32(entry[2:], dir.dirLBA[t])
		order.PutUint16(entry[6:], parent)
		copy(entry[8:], name)
		out = append(out, entry...)
	}
	return out
}

func directoryRecords(dir *node, t tree, now time.Time) []byte {
	parent := dir
	if dir.parent != nil {
		parent = dir.parent
	}
	records := [][]byte{
		directoryRecord([]byte{0}, dir.dirLBA[t], dir.dirSize[t], true, now),
		directoryRecord([]byte{1}, parent.dirLBA[t], parent.dirSize[t], true, now),
	}
	for _, child := range dir.children {
		if child.isDir() {
			records = append(records, directoryRecord(child.isoNames[t], child.dirLBA[t], child.dirSize[t], true, now))
		} else {
			records = append(records, directoryRecord(child.isoNames[t], child.extent, uint32(child.size), false, now))
		}
	}
	var out []byte
	for _, record := range records {
		if used := len(out) % sectorSize; used+len(record) > sectorSize {
			out = append(out, make([]byte, sectorSize-used)...)
		}
		out = append(out, record...)
	}
	return padToSector(out)
}

func directoryRecord(name []byte, extent, size uint32, isDir bool, now time.Time) []byte {
	record := make([]byte, 33+len(name)+(len(name)+1)%2)
	record[0] = byte(len(record))
	putBoth32(record[2:], extent)
	putBoth32(record[10:], size)
	if !now.IsZero() {
		record[18] = byte(now.Year() - 1900)
		record[19] = byte(now.Month())
		record[20] = byte(now.Day())
		record[21] = byte(now.Hour())
		record[22] = byte(now.Minute())
		record[23] = byte(now.Second())
	}
	if isDir {
		record[25] = 2
	}
	putBoth16(record[28:], 1)
	record[32] = byte(len(name))
	copy(record[33:], name)
	return record
}

func volumeDescriptor(t tree, volumeID string, root *node, totalSectors, pathTableSize, lTable, mTable uint32, now time.Time) []byte {
	vd := make([]byte, sectorSize)
	vd[0] = 1
	text := func(field []byte, value string) {
		for i := range field {
			field[i] = ' '
		}
		copy(field, dChars(value, len(field)))
	}
	if t == jolietTree {
		vd[0] = 2
		copy(vd[88:], "%/E")
		text = func(field []byte, value string) {
			units := utf16.Encode([]rune(value))
			for i := 0; i+1 < len(field); i += 2 {
				u := uint16(' ')
				if i/2 < len(units) {
					u = units[i/2]
				}
				binary.BigEndian.PutUint16(field[i:], u)
			}
		}
	}
	copy(vd[1:], "CD001")
	vd[6] = 1
	text(vd[8:40], "")
	text(vd[40:72], volumeID)
	putBoth32(vd[80:], totalSectors)
	putBoth16(vd[120:], 1)
	putBoth16(vd[124:], 1)
	putBoth16(vd[128:], sectorSize)
	putBoth32(vd[132:], pathTableSize)
	binary.LittleEndian.PutUint32(vd[140:], lTable)
	binary.BigEndian.PutUint32(vd[148:], mTable)
	copy(vd[156:190], directoryRecord([]byte{0}, root.dirLBA[t], root.dirSize[t], true, now))
	for _, field := range [][2]int{{190, 318}, {318, 446}, {446, 574}, {574, 702}, {702, 739}, {739, 776}, {776, 813}} {
		text(vd[field[0]:field[1]], "")
	}
	stamp := []byte(now.Format("20060102150405") + "00\x00")
	unset := []byte("0000000000000000\x00")
	copy(vd[813:], stamp)
	copy(vd[830:], stamp)
	copy(vd[847:], unset)
	copy(vd[864:], unset)
	vd[881] = 1
	return vd
}

func copyFile(w io.Writer, f *node) error {
	in, err := os.Open(f.source)
	if err != nil {
		return fmt.Errorf("open iso source: %w", err)
	}
	defer in.Close()
	copied, err := io.CopyN(w, in, f.size)
	if err != nil {
		return fmt.Errorf("copy %s into iso image: %w", f.source, err)
	}
	if pad := int64(sectorsFor(copied))*sectorSize - copied; pad > 0 {
		if _, err := w.Write(make([]byte, pad)); err != nil {
			return fmt.Errorf("write iso image: %w", err)
		}
	}
	return nil
}

func sectorsFor(size int64) uint32 {
	return uint32((size + sectorSize - 1) / sectorSize)
}

func padToSector(b []byte) []byte {
	if rem := len(b) % sectorSize; rem != 0 || len(b) == 0 {
		b = append(b, make([]byte, sectorSize-rem)...)
	}
	return b
}

func putBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func putBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}
