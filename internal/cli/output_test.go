package cli

import (
	"strings"
	"testing"
)

func TestRenderTableAddsFullWidthSeparator(t *testing.T) {
	output := renderTextTable(textTable{
		Headers: []string{"NAME", "STATE"},
		Rows:    [][]string{{"vm-storefront-api", "running"}},
	}, textTableRenderOptions{MaxWidth: 80})
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header, separator, row; got %d lines: %q", len(lines), output)
	}
	if lines[0] != "NAME               STATE  " {
		t.Fatalf("unexpected header line: %q", lines[0])
	}
	if lines[1] != strings.Repeat("-", len(lines[0])) {
		t.Fatalf("separator length/content mismatch: header=%q separator=%q", lines[0], lines[1])
	}
	if lines[2] != "vm-storefront-api  running" {
		t.Fatalf("unexpected row line: %q", lines[2])
	}
}

func TestRenderTableTruncatesToTerminalWidth(t *testing.T) {
	output := renderTextTable(textTable{
		Headers: []string{"NAME", "STATE", "PROFILE", "IP"},
		Rows: [][]string{{
			"tk-node-vm-173629-1781181381811-p-web-runtime-1",
			"running",
			"ubuntu-docker",
			"172.16.11.138",
		}},
	}, textTableRenderOptions{MaxWidth: 52})
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header, separator, row; got %d lines: %q", len(lines), output)
	}
	for _, line := range lines {
		if len([]rune(line)) > 52 {
			t.Fatalf("line exceeded terminal width: len=%d line=%q output=%q", len([]rune(line)), line, output)
		}
	}
	if !strings.Contains(lines[2], "...") {
		t.Fatalf("expected long value to be truncated: %q", lines[2])
	}
	if strings.Contains(lines[2], "173629-1781181381811") {
		t.Fatalf("row still contains untruncated long machine name: %q", lines[2])
	}
}

func TestRenderTableKeepsPipeOutputUnbounded(t *testing.T) {
	longID := "00651899-13a4-47e0-8a40-46b26c753bfe"
	output := renderTextTable(textTable{
		Headers: []string{"ID", "NAME", "IMAGE"},
		Rows:    [][]string{{longID, "vm-storefront-api", longID}},
	}, textTableRenderOptions{})
	if !strings.Contains(output, longID) {
		t.Fatalf("unbounded output should preserve full values: %q", output)
	}
	if !strings.Contains(output, strings.Repeat("-", len(strings.Split(output, "\n")[0]))) {
		t.Fatalf("unbounded output missing separator matching table width: %q", output)
	}
}

func TestCleanTableCellPreventsAccidentalMultilineRows(t *testing.T) {
	output := renderTextTable(textTable{
		Headers: []string{"NAME", "STATE"},
		Rows:    [][]string{{"multi\nline\tname", "running"}},
	}, textTableRenderOptions{MaxWidth: 80})
	if strings.Contains(output, "multi\nline") || strings.Contains(output, "\t") {
		t.Fatalf("table cell was not normalized: %q", output)
	}
	if !strings.Contains(output, "multi line name") {
		t.Fatalf("normalized value missing: %q", output)
	}
}

func TestRenderTableClampsExtremelyNarrowOutput(t *testing.T) {
	output := renderTextTable(textTable{
		Headers: []string{"NAME", "STATE", "PROFILE", "IP", "PRIVATE_IP"},
		Rows: [][]string{{
			"tk-node-vm-173629-1781181381811-p-web-runtime-1",
			"running",
			"ubuntu-docker",
			"172.16.11.138",
			"10.42.2.2",
		}},
	}, textTableRenderOptions{MaxWidth: 12})
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	for _, line := range lines {
		if len([]rune(line)) > 12 {
			t.Fatalf("line exceeded hard terminal bound: len=%d line=%q output=%q", len([]rune(line)), line, output)
		}
	}
	if lines[1] != strings.Repeat("-", 12) {
		t.Fatalf("separator should stay a plain full-width rule: %q", lines[1])
	}
}
