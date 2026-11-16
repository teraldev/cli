package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Table prints rows with aligned columns to stdout. Cells may contain
// styling; widths are measured without escape codes. On a non-terminal the
// header is plain so the output stays easy to process with awk or cut.
func (o *IO) Table(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && lipgloss.Width(cell) > widths[i] {
				widths[i] = lipgloss.Width(cell)
			}
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("   ")
			}
			if style != nil {
				cell = style(cell)
			}
			b.WriteString(cell)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(cell)))
			}
		}
		fmt.Fprintln(o.Out, strings.TrimRight(b.String(), " "))
	}
	upper := make([]string, len(headers))
	for i, h := range headers {
		upper[i] = strings.ToUpper(h)
	}
	line(upper, func(s string) string { return o.Theme.Header.Render(s) })
	for _, row := range rows {
		line(row, nil)
	}
}

// KV prints aligned "key  value" pairs to stdout.
func (o *IO) KV(pairs [][2]string) {
	width := 0
	for _, p := range pairs {
		if lipgloss.Width(p[0]) > width {
			width = lipgloss.Width(p[0])
		}
	}
	for _, p := range pairs {
		if p[1] == "" {
			continue
		}
		fmt.Fprintf(o.Out, "%s%s  %s\n", o.Theme.Muted.Render(p[0]), strings.Repeat(" ", width-lipgloss.Width(p[0])), p[1])
	}
}
