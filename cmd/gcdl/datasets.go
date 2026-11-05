package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ghuls/genomics_core_downloader/internal/download"
	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

func (m model) updateDatasets(key string) (tea.Model, tea.Cmd) {
	page := max(1, m.layout().rows-1)
	switch key {
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup", "ctrl+b":
		m.cursor -= page
	case "pgdown", "ctrl+f":
		m.cursor += page
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.visible) - 1
	case " ":
		if d := m.current(); d != nil {
			if d.Downloadable() {
				if m.selected[d.Key()] {
					delete(m.selected, d.Key())
				} else {
					m.selected[d.Key()] = true
				}
			} else {
				m.logf(warnStyle.Render("• %s is expired and cannot be downloaded"), displayName(*d))
			}
			m.cursor++
		}
	case "a": // (de)select all visible downloadable datasets
		all := true
		for _, i := range m.visible {
			if d := m.datasets[i]; d.Downloadable() && !m.selected[d.Key()] {
				all = false
			}
		}
		for _, i := range m.visible {
			if d := m.datasets[i]; d.Downloadable() {
				if all {
					delete(m.selected, d.Key())
				} else {
					m.selected[d.Key()] = true
				}
			}
		}
	case "x":
		if d := m.current(); d != nil {
			if j := m.latestJob(d.Key()); j != nil && j.state.pending() {
				m.cancelJob(j)
			}
		}
	case "d", "enter":
		var keys []string
		for _, d := range m.datasets { // keep table order
			if m.selected[d.Key()] {
				keys = append(keys, d.Key())
			}
		}
		if len(keys) == 0 {
			if d := m.current(); d != nil {
				if !d.Downloadable() {
					m.logf(warnStyle.Render("• %s is expired and cannot be downloaded"), displayName(*d))
					return m, nil
				}
				keys = []string{d.Key()}
			}
		}
		for _, k := range keys {
			if j := m.latestJob(k); j != nil && j.state.pending() {
				continue
			}
			if d := m.dataset(k); d != nil {
				m.nextJobID++
				m.jobs = append(m.jobs, &job{id: m.nextJobID, key: k, ds: *d, prog: download.Progress{Total: -1}})
			}
		}
		m.selected = map[string]bool{}
		m.clampAll()
		return m, m.schedule()
	}
	m.clampAll()
	return m, nil
}

// filterFields are the dataset fields the filter can match, in the order "/"
// cycles through them while typing.
var filterFields = []struct {
	name  string
	value func(portal.Dataset) string
}{
	{"dataset name", func(d portal.Dataset) string { return displayName(d) + " " + d.Name }},
	{"project", func(d portal.Dataset) string { return d.Project }},
	{"requested by", func(d portal.Dataset) string { return d.RequestedBy }},
}

// refilter applies the expired toggle and the filter. All space separated
// filter terms must occur in the selected field.
func (m *model) refilter() {
	var cur string
	if d := m.current(); d != nil {
		cur = d.Key()
	}
	terms := strings.Fields(strings.ToLower(m.filter.Value()))
	m.visible = m.visible[:0]
	for i, d := range m.datasets {
		if m.hideExpired && !d.Downloadable() {
			continue
		}
		text := strings.ToLower(filterFields[m.filterField].value(d))
		ok := true
		for _, t := range terms {
			if !strings.Contains(text, t) {
				ok = false
				break
			}
		}
		if ok {
			m.visible = append(m.visible, i)
		}
	}
	m.cursor = 0
	for vi, i := range m.visible {
		if m.datasets[i].Key() == cur {
			m.cursor = vi
		}
	}
	m.clampAll()
}

func (m *model) current() *portal.Dataset {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return nil
	}
	return &m.datasets[m.visible[m.cursor]]
}

func (m model) statusOf(d portal.Dataset) string {
	if j := m.latestJob(d.Key()); j != nil {
		return jobLabel(j)
	}
	if d.Downloadable() {
		return "available"
	}
	return "expired"
}

func (m model) datasetsPanel(width int, l layout) string {
	cols := m.columns(width - 4)
	var body []string
	switch {
	case m.filtering:
		next := filterFields[(m.filterField+1)%len(filterFields)].name
		body = append(body, filterLabel.Render(filterFields[m.filterField].name)+m.filter.View()+
			helpStyle.Render(fmt.Sprintf("   %s · press / again to filter on %s", matches(len(m.visible)), next)))
	case m.filter.Value() != "":
		body = append(body, filterLabel.Render(filterFields[m.filterField].name)+keyStyle.Render("/ ")+m.filter.Value()+
			helpStyle.Render(fmt.Sprintf("   %s · / edit · esc clear", matches(len(m.visible)))))
	default:
		body = append(body, helpStyle.Render("/ to filter"))
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	body = append(body, renderRow(cols, titles, headerStyle, headerStyle))
	for vi := m.offset; vi < min(len(m.visible), m.offset+l.rows); vi++ {
		d := m.datasets[m.visible[vi]]
		mark := []rune("  ")
		if vi == m.cursor {
			mark[0] = '▸'
		}
		if m.selected[d.Key()] {
			mark[1] = '●'
		}
		st := m.statusOf(d)
		stText := st
		if j := m.latestJob(d.Key()); j != nil && j.state == jobRunning && j.prog.Total > 0 {
			frac := ratio(j.prog.Done, j.prog.Total)
			if j.prog.Phase == download.PhaseHashingPartial {
				stText = fmt.Sprintf("verify %3.0f%%", 100*frac)
			} else {
				stText = fmt.Sprintf("%s %3.0f%%", miniBar(frac, 6), 100*frac)
			}
		}
		base := lipgloss.NewStyle()
		switch {
		case vi == m.cursor && m.focus == focusDatasets:
			base = cursorStyle
		case m.selected[d.Key()]:
			base = selectedStyle
		case !d.Downloadable():
			base = expiredStyle
		}
		body = append(body, renderRow(cols,
			[]string{string(mark), d.Project, displayName(d), d.Expires, d.FileCount, d.TotalSize, d.RequestedBy, stText},
			base, base.Foreground(statusColor(st))))
	}
	if len(m.visible) == 0 {
		body = append(body, helpStyle.Render("no datasets match"))
	}
	title := panelTitle.Render("Datasets")
	if len(m.visible) > 0 {
		title += helpStyle.Render(fmt.Sprintf("  %d–%d of %d", m.offset+1, min(len(m.visible), m.offset+l.rows), len(m.visible)))
	}
	if m.hideExpired {
		title += helpStyle.Render(" · expired hidden")
	}
	return panel(title, strings.Join(body, "\n"), width, l.rows+2, m.focus == focusDatasets)
}

func matches(n int) string {
	if n == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", n)
}

func displayName(d portal.Dataset) string { return d.DisplayName() }
