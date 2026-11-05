package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	accent        = logoCyan
	dim           = lipgloss.Color("241")
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	boxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 3)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	helpStyle     = lipgloss.NewStyle().Foreground(dim)
	keyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
	keyDescStyle  = lipgloss.NewStyle() // like the names of datasets that are not expired
	labelStyle    = lipgloss.NewStyle().Foreground(dim).Width(10)
	filterLabel   = lipgloss.NewStyle().Foreground(dim).Width(13)
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	borderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	focusBorder   = lipgloss.NewStyle().Foreground(accent)
	panelTitle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	expiredStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorStyle   = lipgloss.NewStyle().Background(lipgloss.Color("236")).Bold(true)
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
)

func statusColor(st string) lipgloss.Color {
	switch st {
	case "available":
		return "78"
	case "done":
		return "42"
	case "downloading", "verifying", "starting":
		return "214"
	case "queued":
		return "111"
	case "failed", "md5 mismatch":
		return "196"
	case "cancelled", "exists":
		return "180"
	}
	return "240"
}

// panel draws a rounded box of exactly width × (height+2) cells with the
// title embedded in the top border. The focused panel gets a coloured border.
func panel(title, body string, width, height int, focused bool) string {
	bs := borderStyle
	if focused {
		bs = focusBorder
	}
	inner := max(4, width-2)
	tw := ansi.StringWidth(title)
	top := bs.Render("╭─ ") + title + bs.Render(" "+strings.Repeat("─", max(0, inner-3-tw))+"╮")
	lines := strings.Split(body, "\n")
	var b strings.Builder
	b.WriteString(top + "\n")
	side := bs.Render("│")
	for i := 0; i < height; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		b.WriteString(side + " " + cell(l, inner-2) + " " + side + "\n")
	}
	b.WriteString(bs.Render("╰" + strings.Repeat("─", inner) + "╯"))
	return b.String()
}

func miniBar(frac float64, w int) string {
	n := max(0, min(w, int(frac*float64(w)+0.5)))
	return strings.Repeat("▰", n) + strings.Repeat("▱", w-n)
}
