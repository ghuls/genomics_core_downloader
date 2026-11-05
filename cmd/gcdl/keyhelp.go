package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// keyGroup is a titled group of key help columns; each column holds up to
// two key/description pairs, shown below each other.
type keyGroup struct {
	title string
	cols  [][][2]string
}

// navGroup is the navigation group shown first for every panel.
func navGroup(move, firstLast string) keyGroup {
	return keyGroup{"Navigate", [][][2]string{
		{{"tab", "next panel"}, {"↑/↓", move}},
		{{"pgup/pgdn", "page"}, {"home/end", firstLast}},
	}}
}

// keyHelp lays out the key help of the focused panel as a grid: a title row
// and two rows of keys per group, indented like panel contents. Groups that
// do not fit the width wrap to a new block of rows.
func (m model) keyHelp(width int) []string {
	const indent, colGap, groupGap = 2, 3, 4
	var groups []keyGroup
	switch {
	case m.confirmQuit:
		return []string{
			strings.Repeat(" ", indent) + warnStyle.Render("Downloads are running. Press q again to quit (partial files are kept and resumed next time)."),
			strings.Repeat(" ", indent) + keyStyle.Render("q") + "  " + keyDescStyle.Render("quit"),
			strings.Repeat(" ", indent) + keyStyle.Render("any other key") + "  " + keyDescStyle.Render("keep running"),
		}
	case m.filtering:
		groups = []keyGroup{
			{"Navigate", [][][2]string{
				{{"↑/↓", "move"}},
				{{"pgup/pgdn", "page"}, {"home/end", "first/last"}},
			}},
			{"Filter", [][][2]string{
				{{"type", "filter"}, {"/", "on " + filterFields[(m.filterField+1)%len(filterFields)].name}},
				{{"enter", "apply"}, {"esc", "clear"}},
			}},
		}
	case m.focus == focusDownloads:
		groups = []keyGroup{
			navGroup("move", "first/last"),
			{"Download", [][][2]string{
				{{"x", "cancel"}, {"d", "retry"}},
				{{"c", "clear finished"}},
			}},
			{"", [][][2]string{{{"q", "quit"}}}},
		}
	case m.focus == focusActivity:
		groups = []keyGroup{
			navGroup("scroll", "oldest/newest"),
			{"", [][][2]string{{{"esc", "back to datasets"}}}},
			{"", [][][2]string{{{"q", "quit"}}}},
		}
	default:
		expired := "hide expired"
		if m.hideExpired {
			expired = "show expired"
		}
		groups = []keyGroup{
			navGroup("move", "first/last"),
			{"Select", [][][2]string{{{"space", "select"}, {"a", "all"}}}},
			{"Download", [][][2]string{{{"d", "download"}, {"x", "cancel"}}}},
			{"View", [][][2]string{
				{{"/", "filter"}, {"e", expired}},
				{{"r", "refresh list"}, {"q", "quit"}},
			}},
		}
	}

	// Render every group as three lines of equal width.
	type block struct {
		lines [3]string
		width int
	}
	var blocks []block
	for _, g := range groups {
		var b block
		for ci, col := range g.cols {
			kw, dw := 0, 0
			for _, kv := range col {
				kw = max(kw, ansi.StringWidth(kv[0]))
				dw = max(dw, ansi.StringWidth(kv[1]))
			}
			gap := 0
			if ci > 0 {
				gap = colGap
			}
			for r := 0; r < 2; r++ {
				cellText := strings.Repeat(" ", kw+2+dw)
				if r < len(col) {
					cellText = keyStyle.Render(cell(col[r][0], kw)) + "  " + keyDescStyle.Render(cell(col[r][1], dw))
				}
				b.lines[r+1] += strings.Repeat(" ", gap) + cellText
			}
			b.width += gap + kw + 2 + dw
		}
		b.lines[0] = panelTitle.Render(cell(g.title, b.width))
		blocks = append(blocks, b)
	}

	// Place the blocks side by side, wrapping when the width is exceeded.
	var out []string
	var row [3]string
	used := 0
	flush := func() {
		for _, l := range row {
			out = append(out, strings.TrimRight(strings.Repeat(" ", indent)+l, " "))
		}
		row, used = [3]string{}, 0
	}
	for _, b := range blocks {
		gap := 0
		if used > 0 {
			gap = groupGap
		}
		if used > 0 && indent+used+gap+b.width > width {
			flush()
			gap = 0
		}
		for i := range row {
			row[i] += strings.Repeat(" ", gap) + b.lines[i]
		}
		used += gap + b.width
	}
	flush()
	for i := range out {
		out[i] = ansi.Truncate(out[i], width, "…")
	}
	return out
}
