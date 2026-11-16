package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/gfdelarue/kiln-vm/internal/refid"
)

func printTable(headers []string, rows [][]string) {
	table := textTable{Headers: headers, Rows: rows}
	viewport := measureOutputViewport(os.Stdout, os.LookupEnv)
	writeRenderedOutput(os.Stdout, renderTextTable(table, textTableRenderOptions{
		MaxWidth: viewport.Width,
	}))
}

func writeRenderedOutput(w io.Writer, text string) {
	_, _ = io.WriteString(w, text)
}

func displayIDMap(ids []string, wide bool) map[string]string {
	result := make(map[string]string, len(ids))
	if wide {
		for _, id := range ids {
			result[id] = id
		}
		return result
	}
	return refid.AbbreviateAll(ids, refid.DefaultDisplayLength)
}

func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
