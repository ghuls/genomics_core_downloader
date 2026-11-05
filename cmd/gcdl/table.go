package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

type column struct {
	title string
	width int
	right bool
}

// columns lays out the dataset table for the given content width; Status is
// last, Files and Size are right aligned.
func (m model) columns(width int) []column { return tableColumns(m.datasets, width) }

// tableColumns lays out the dataset table (also used by --list) for the given
// content width.
func tableColumns(ds []portal.Dataset, width int) []column {
	fixed := []column{{"", 2, false}, {"Expires (BE)", 16, false}, {"Files", 6, true}, {"Size", 8, true}, {"Status", 12, false}}
	used := 0
	for _, c := range fixed {
		used += c.width + 1
	}
	flex := max(30, width-used)
	// Project and Requested by are mostly empty: size them to their content
	// so the Dataset column gets the room for full file names.
	project, requested := len("Project"), len("Requested by")
	for _, d := range ds {
		project = max(project, ansi.StringWidth(d.Project))
		requested = max(requested, ansi.StringWidth(d.RequestedBy))
	}
	project, requested = min(project, flex/4), min(requested, flex/5)
	name := flex - project - requested - 2
	return []column{fixed[0], {"Project", project, false}, {"Dataset", name, false}, fixed[1], fixed[2], fixed[3],
		{"Requested by", requested, false}, fixed[4]}
}

func cell(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func cellRight(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) + s
}

// renderRow styles every cell (so a row background is not interrupted) and
// uses a separate style for the last (status) cell.
func renderRow(cols []column, vals []string, base, last lipgloss.Style) string {
	var b strings.Builder
	for i, c := range cols {
		st := base
		if i == len(cols)-1 {
			st = last
		}
		if i > 0 {
			b.WriteString(base.Render(" "))
		}
		if c.right {
			b.WriteString(st.Render(cellRight(vals[i], c.width)))
		} else {
			b.WriteString(st.Render(cell(vals[i], c.width)))
		}
	}
	return b.String()
}

// printTable prints the datasets like the Datasets table of the TUI (plus an
// MD5 column): same columns, alignment and colours (plain text when not
// writing to a terminal). On a terminal the table is drawn in a panel no wider than the window.
func printTable(w io.Writer, ds []portal.Dataset) {
	fit := tableColumns(ds, 1<<16) // measure the widest names
	need := 0
	for _, c := range fit[1:] {
		need += c.width + 1
	}
	name := 0
	for _, d := range ds {
		name = max(name, ansi.StringWidth(d.DisplayName()))
	}
	need += name - fit[2].width
	md5 := len("MD5")
	for _, d := range ds {
		md5 = max(md5, len(d.MD5))
	}
	need += md5 + 1
	termWidth, _, err := term.GetSize(int(os.Stdout.Fd()))
	isTerm := err == nil && termWidth > 0
	if isTerm {
		need = min(need, termWidth-4)
	}
	// No cursor/selection mark; the MD5 column follows Dataset.
	cols := tableColumns(ds, need-md5-1+fit[0].width+1)[1:]
	cols = append(cols[:2], append([]column{{"MD5", md5, false}}, cols[2:]...)...)

	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	lines := []string{renderRow(cols, titles, headerStyle, headerStyle)}
	avail := 0
	for _, d := range ds {
		st, base := "available", lipgloss.NewStyle()
		if d.Downloadable() {
			avail++
		} else {
			st, base = "expired", expiredStyle
		}
		lines = append(lines, renderRow(cols,
			[]string{d.Project, d.DisplayName(), d.MD5, d.Expires, d.FileCount, d.TotalSize, d.RequestedBy, st},
			base, base.Foreground(statusColor(st))))
	}
	if !isTerm {
		for _, l := range lines {
			fmt.Fprintln(w, strings.TrimRight(l, " "))
		}
		return
	}
	title := panelTitle.Render("Datasets") + helpStyle.Render(fmt.Sprintf("  %d datasets · %d downloadable", len(ds), avail))
	width := 0
	for _, c := range cols {
		width += c.width + 1
	}
	fmt.Fprintln(w, panel(title, strings.Join(lines, "\n"), width+3, len(lines), false))
}
