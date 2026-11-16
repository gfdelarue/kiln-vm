package cli

import "strings"

type textTable struct {
	Headers []string
	Rows    [][]string
}

type textTableRenderOptions struct {
	MaxWidth int
}

type textTableLayout struct {
	widths []int
	gap    string
	width  int
}

func renderTextTable(table textTable, options textTableRenderOptions) string {
	layout := layoutTextTable(table, options)
	var b strings.Builder
	writeRenderedTableLine(&b, formatTextTableLine(table.Headers, layout.widths, layout.gap), options.MaxWidth)
	writeRenderedTableLine(&b, strings.Repeat("-", boundedTextTableWidth(layout.width, options.MaxWidth)), 0)
	for _, row := range table.Rows {
		writeRenderedTableLine(&b, formatTextTableLine(row, layout.widths, layout.gap), options.MaxWidth)
	}
	return b.String()
}

func boundedTextTableWidth(width int, maxWidth int) int {
	if maxWidth > 0 && width > maxWidth {
		return maxWidth
	}
	return width
}

func writeRenderedTableLine(b *strings.Builder, line string, maxWidth int) {
	if maxWidth > 0 {
		line = truncateDisplayText(line, maxWidth)
	}
	b.WriteString(line)
	b.WriteByte('\n')
}

func layoutTextTable(table textTable, options textTableRenderOptions) textTableLayout {
	widths := naturalTextTableColumnWidths(table)
	if len(widths) == 0 {
		return textTableLayout{gap: "", width: 0}
	}
	gap := "  "
	width := textTableWidth(widths, gap)
	if options.MaxWidth > 0 && width > options.MaxWidth {
		widths = shrinkTextTableColumnWidths(widths, options.MaxWidth, gap, 3)
		width = textTableWidth(widths, gap)
	}
	if options.MaxWidth > 0 && width > options.MaxWidth {
		gap = " "
		widths = shrinkTextTableColumnWidths(widths, options.MaxWidth, gap, 1)
		width = textTableWidth(widths, gap)
	}
	return textTableLayout{widths: widths, gap: gap, width: width}
}

func naturalTextTableColumnWidths(table textTable) []int {
	widths := make([]int, len(table.Headers))
	for i, header := range table.Headers {
		widths[i] = displayWidth(cleanTableCell(header))
	}
	for _, row := range table.Rows {
		for i := range table.Headers {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			if width := displayWidth(cleanTableCell(value)); width > widths[i] {
				widths[i] = width
			}
		}
	}
	for i, width := range widths {
		if width < 1 {
			widths[i] = 1
		}
	}
	return widths
}

func shrinkTextTableColumnWidths(widths []int, maxWidth int, gap string, minWidth int) []int {
	result := append([]int(nil), widths...)
	if minWidth < 1 {
		minWidth = 1
	}
	for textTableWidth(result, gap) > maxWidth {
		index := widestShrinkableColumn(result, minWidth)
		if index < 0 {
			break
		}
		result[index]--
	}
	return result
}

func widestShrinkableColumn(widths []int, minWidth int) int {
	index := -1
	max := minWidth
	for i, width := range widths {
		if width > max {
			max = width
			index = i
		}
	}
	return index
}

func textTableWidth(widths []int, gap string) int {
	if len(widths) == 0 {
		return 0
	}
	width := 0
	for _, column := range widths {
		width += column
	}
	width += len(gap) * (len(widths) - 1)
	return width
}

func formatTextTableLine(values []string, widths []int, gap string) string {
	var b strings.Builder
	for i, width := range widths {
		if i > 0 {
			b.WriteString(gap)
		}
		value := ""
		if i < len(values) {
			value = values[i]
		}
		cell := truncateDisplayText(cleanTableCell(value), width)
		b.WriteString(cell)
		if padding := width - displayWidth(cell); padding > 0 {
			b.WriteString(strings.Repeat(" ", padding))
		}
	}
	return b.String()
}

func cleanTableCell(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateDisplayText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(value) <= width {
		return value
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	runes := []rune(value)
	return string(runes[:width-3]) + "..."
}

func displayWidth(value string) int {
	return len([]rune(value))
}
