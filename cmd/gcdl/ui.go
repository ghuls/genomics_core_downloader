package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/ghuls/genomics_core_downloader/internal/credstore"
	"github.com/ghuls/genomics_core_downloader/internal/download"
	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

type screen int

const (
	screenLogin screen = iota
	screenLoading
	screenList
)

// focus is the panel that receives navigation keys (switched with tab).
type focus int

const (
	focusDatasets focus = iota
	focusDownloads
	focusActivity
)

type loginDoneMsg struct{ err error }

type datasetsMsg struct {
	ds  []portal.Dataset
	err error
}

type keepAliveMsg struct{}

type model struct {
	client   *portal.Client
	outDir   string
	parallel int

	screen   screen
	user     textinput.Model
	pass     textinput.Model
	unlock   textinput.Model // passphrase of the saved password
	saved    *credstore.File // nil if no password is saved
	useSaved bool            // login screen asks for the passphrase
	spinner  spinner.Model
	spinTick int
	progress progress.Model
	err      string

	datasets    []portal.Dataset
	visible     []int // indices into datasets
	cursor      int   // index into visible
	offset      int
	selected    map[string]bool
	hideExpired bool
	filter      textinput.Model
	filtering   bool // filter input has focus
	filterField int  // index into filterFields

	jobs      []*job
	nextJobID int
	dlCursor  int
	dlOffset  int
	events    chan tea.Msg

	messages  []string
	actOffset int  // first visible activity line
	actFollow bool // keep showing the newest messages

	focus         focus
	width, height int
	confirmQuit   bool
}

func newModel(c *portal.Client, outDir, username string, saved *credstore.File, profile termenv.Profile, parallel int) model {
	u := textinput.New()
	u.Placeholder = "username"
	u.Prompt = loginPrompt("Username")
	u.SetValue(username)
	u.CharLimit = 1024
	p := textinput.New()
	p.Placeholder = "password"
	p.Prompt = loginPrompt("Password")
	p.EchoMode = textinput.EchoPassword
	p.EchoCharacter = '•'
	p.CharLimit = 1024
	if username == "" {
		u.Focus()
	} else {
		p.Focus()
	}
	k := textinput.New()
	k.Placeholder = "passphrase"
	k.Prompt = loginPrompt("Passphrase")
	k.EchoMode = textinput.EchoPassword
	k.EchoCharacter = '•'
	k.CharLimit = 1024
	useSaved := saved != nil && (username == "" || username == saved.User)
	if useSaved {
		u.SetValue(saved.User)
		u.Blur()
		p.Focus() // for "type password instead"
		k.Focus()
	} else {
		saved = nil
	}
	f := textinput.New()
	f.Prompt = "/ "
	f.Placeholder = "type to filter"
	f.CharLimit = 200
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent)
	return model{
		client:    c,
		outDir:    outDir,
		parallel:  max(1, parallel),
		user:      u,
		pass:      p,
		unlock:    k,
		saved:     saved,
		useSaved:  useSaved,
		spinner:   sp,
		filter:    f,
		progress:  progress.New(progress.WithGradient(string(logoCyan), string(logoNavy)), progress.WithColorProfile(profile), progress.WithoutPercentage()),
		selected:  map[string]bool{},
		events:    make(chan tea.Msg, 256),
		actFollow: true,
		width:     100,
		height:    30,
	}
}

func (m model) Init() tea.Cmd {
	// A single listener for download events runs for the whole program; every
	// handler of such an event starts it again.
	return tea.Batch(textinput.Blink, m.spinner.Tick, waitEvent(m.events))
}

func keepAlive() tea.Cmd {
	return tea.Tick(4*time.Minute, func(time.Time) tea.Msg { return keepAliveMsg{} })
}

func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if !m.layout().showBottom && m.focus == focusActivity {
			m.focus = focusDatasets
		}
		m.clampAll()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.spinTick++
		m.spinner.Style = lipgloss.NewStyle().Foreground(spinnerShade(m.spinTick))
		return m, cmd

	case loginDoneMsg:
		if msg.err != nil {
			m.screen = screenLogin
			m.err = msg.err.Error()
			if m.useSaved {
				m.unlock.SetValue("")
			} else {
				m.pass.SetValue("")
				m.pass.Focus()
			}
			return m, textinput.Blink
		}
		m.err = ""
		return m, tea.Batch(m.listCmd(), keepAlive())

	case datasetsMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			if m.screen == screenLoading {
				m.screen = screenLogin
			} else {
				m.logf(errStyle.Render("✗ refresh failed: %v"), msg.err)
			}
			return m, nil
		}
		m.err = ""
		m.screen = screenList
		// Keep real file names learned earlier.
		names := map[string]string{}
		for _, d := range m.datasets {
			if d.FileName != "" {
				names[d.Key()] = d.FileName
			}
		}
		for i := range msg.ds {
			if fn, ok := names[msg.ds[i].Key()]; ok && msg.ds[i].FileName == "" {
				msg.ds[i].FileName = fn
			}
		}
		m.datasets = msg.ds
		m.refilter()
		return m, nil

	case keepAliveMsg:
		c := m.client
		return m, tea.Batch(keepAlive(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = c.KeepAlive(ctx)
			return nil
		})

	case linkMsg:
		j := m.job(msg.id)
		if j == nil || j.state != jobStarting {
			return m, nil
		}
		if msg.err != nil {
			m.finish(j, download.Result{}, msg.err)
			return m, m.schedule()
		}
		m.setFileName(msg.ds)
		j.ds = msg.ds
		j.state = jobRunning
		j.lastTime = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel = cancel
		dest := filepath.Join(m.outDir, msg.ds.FileName)
		id, md5, url, ch := j.id, msg.ds.MD5, msg.url, m.events
		go func() {
			res, err := download.File(ctx, url, dest, md5, func(p download.Progress) {
				select {
				case ch <- progressMsg{id: id, p: p}:
				default: // drop progress updates if the UI is behind
				}
			})
			ch <- dlDoneMsg{id: id, res: res, err: err}
		}()
		return m, nil

	case progressMsg:
		if j := m.job(msg.id); j != nil && j.state == jobRunning {
			now := time.Now()
			if msg.p.Phase == download.PhaseDownloading && j.prog.Phase == download.PhaseDownloading {
				if dt := now.Sub(j.lastTime).Seconds(); dt > 0 && msg.p.Done >= j.lastDone {
					inst := float64(msg.p.Done-j.lastDone) / dt
					if j.speed == 0 {
						j.speed = inst
					} else {
						j.speed = 0.8*j.speed + 0.2*inst
					}
				}
			}
			j.prog, j.lastDone, j.lastTime = msg.p, msg.p.Done, now
		}
		return m, waitEvent(m.events)

	case dlDoneMsg:
		if j := m.job(msg.id); j != nil && j.state == jobRunning {
			m.finish(j, msg.res, msg.err)
		}
		return m, tea.Batch(waitEvent(m.events), m.schedule())

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancelAll()
			return m, tea.Quit
		}
		switch m.screen {
		case screenLogin:
			return m.updateLogin(msg)
		case screenList:
			return m.updateList(msg)
		}
	}
	return m, nil
}

func (m model) listCmd() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ds, err := c.ListDatasets(ctx)
		return datasetsMsg{ds: ds, err: err}
	}
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key != "q" {
		m.confirmQuit = false
	}
	if m.filtering {
		switch key {
		case "enter", "tab":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		case "esc":
			m.filtering = false
			m.filter.Blur()
			m.filter.SetValue("")
			m.refilter()
			return m, nil
		case "/": // filter on the next field, keeping the text
			m.filterField = (m.filterField + 1) % len(filterFields)
			m.refilter()
			return m, nil
		case "up", "down", "pgup", "pgdown", "home", "end": // move in the list, not the text
			return m.updateDatasets(key)
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.refilter()
			return m, cmd
		}
	}

	// Global keys.
	switch key {
	case "tab", "shift+tab":
		panels := []focus{focusDatasets, focusDownloads}
		if m.layout().showBottom {
			panels = append(panels, focusActivity)
		}
		i := 0
		for k, p := range panels {
			if p == m.focus {
				i = k
			}
		}
		if key == "tab" {
			i = (i + 1) % len(panels)
		} else {
			i = (i + len(panels) - 1) % len(panels)
		}
		m.focus = panels[i]
		return m, nil
	case "/":
		m.focus = focusDatasets
		m.filtering = true
		m.filter.Focus()
		return m, textinput.Blink
	case "esc":
		if m.filter.Value() != "" {
			m.filter.SetValue("")
			m.refilter()
			return m, nil
		}
		m.focus = focusDatasets
		return m, nil
	case "q":
		if m.running() > 0 && !m.confirmQuit {
			m.confirmQuit = true
			return m, nil
		}
		m.cancelAll()
		return m, tea.Quit
	case "e":
		m.hideExpired = !m.hideExpired
		m.refilter()
		return m, nil
	case "r":
		m.logf("%s", helpStyle.Render("• refreshing dataset list…"))
		return m, m.listCmd()
	}

	switch m.focus {
	case focusDownloads:
		return m.updateDownloads(key)
	case focusActivity:
		return m.updateActivity(key)
	default:
		return m.updateDatasets(key)
	}
}

func (m model) updateActivity(key string) (tea.Model, tea.Cmd) {
	l := m.layout()
	page := max(1, l.bottomLines-1)
	switch key {
	case "up", "k":
		m.actOffset--
	case "down", "j":
		m.actOffset++
	case "pgup", "ctrl+b":
		m.actOffset -= page
	case "pgdown", "ctrl+f":
		m.actOffset += page
	case "home", "g":
		m.actOffset = 0
	case "end", "G":
		m.actOffset = len(m.messages)
	}
	maxOff := max(0, len(m.messages)-l.bottomLines)
	m.actOffset = max(0, min(m.actOffset, maxOff))
	m.actFollow = m.actOffset == maxOff
	return m, nil
}

func (m *model) logf(format string, args ...any) {
	m.messages = append(m.messages, fmt.Sprintf(format, args...))
	if len(m.messages) > 1000 {
		m.messages = m.messages[len(m.messages)-1000:]
		m.actOffset = max(0, m.actOffset-1)
	}
	if m.actFollow {
		m.actOffset = max(0, len(m.messages)-m.layout().bottomLines)
	}
}

func scrollInto(cursor, offset, n, h int) (int, int) {
	cursor = max(0, min(cursor, n-1))
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+h {
		offset = cursor - h + 1
	}
	return cursor, max(0, min(offset, max(0, n-h)))
}

func (m *model) clampAll() {
	l := m.layout()
	m.cursor, m.offset = scrollInto(m.cursor, m.offset, len(m.visible), l.rows)
	m.dlCursor, m.dlOffset = scrollInto(m.dlCursor, m.dlOffset, len(m.jobs), l.dlLines)
	maxOff := max(0, len(m.messages)-l.bottomLines)
	if m.actFollow {
		m.actOffset = maxOff
	}
	m.actOffset = max(0, min(m.actOffset, maxOff))
}

// layout describes the vertical split of the list screen:
//
//	header                       1
//	Datasets panel               rows + 4 (borders, filter line, column header)
//	Downloads panel              dlLines + 2
//	Details | Activity panels    bottomLines + 2 (only if there is room)
//	key help                     3 (more when groups wrap)
type layout struct {
	rows        int
	dlLines     int
	bottomLines int
	showBottom  bool
}

func (m model) layout() layout {
	l := layout{bottomLines: 5}
	switch {
	case m.height >= 44:
		l.dlLines = 8
	case m.height >= 34:
		l.dlLines = 5
	default:
		l.dlLines = 3
	}
	rest := m.height - 1 - len(m.keyHelp(max(60, m.width))) - (l.dlLines + 2) - 4
	if rest-(l.bottomLines+2) >= 5 {
		l.showBottom = true
		rest -= l.bottomLines + 2
	}
	l.rows = max(3, rest)
	return l
}

func (m model) View() string {
	switch m.screen {
	case screenLogin, screenLoading:
		return m.viewLogin()
	default:
		return m.viewList()
	}
}

func (m model) viewList() string {
	width := max(60, m.width)
	l := m.layout()
	var out []string

	// Header.
	nAvail := 0
	for _, d := range m.datasets {
		if d.Downloadable() {
			nAvail++
		}
	}
	left := titleStyle.Render("Genomics Core · My Data")
	stats := fmt.Sprintf("%d datasets · %d downloadable", len(m.datasets), nAvail)
	if n := len(m.selected); n > 0 {
		stats += fmt.Sprintf(" · %d selected", n)
	}
	right := helpStyle.Render(stats + " · → " + m.outDir)
	out = append(out, left+strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-ansi.StringWidth(right)))+right)

	out = append(out, m.datasetsPanel(width, l))
	out = append(out, m.downloadsPanel(width, l))
	if l.showBottom {
		lw := width * 2 / 5
		out = append(out, lipgloss.JoinHorizontal(lipgloss.Top,
			panel(panelTitle.Render("Details"), m.detailsBody(), lw, l.bottomLines, false),
			m.activityPanel(width-lw, l)))
	}

	out = append(out, m.keyHelp(width)...)
	return strings.Join(out, "\n")
}

func (m model) activityPanel(width int, l layout) string {
	var lines []string
	if m.err != "" {
		lines = append(lines, errStyle.Render(m.err))
	}
	start := m.actOffset
	if m.focus != focusActivity || m.actFollow {
		start = max(0, len(m.messages)-l.bottomLines)
	}
	for i := start; i < min(len(m.messages), start+l.bottomLines); i++ {
		lines = append(lines, m.messages[i])
	}
	if len(m.messages) == 0 && m.err == "" {
		lines = append(lines, helpStyle.Render("nothing yet"))
	}
	title := panelTitle.Render("Activity")
	if n := len(m.messages); n > l.bottomLines {
		title += helpStyle.Render(fmt.Sprintf("  %d–%d of %d", start+1, min(n, start+l.bottomLines), n))
	}
	return panel(title, strings.Join(lines, "\n"), width, l.bottomLines, m.focus == focusActivity)
}

func (m model) detailsBody() string {
	d := m.current()
	if d == nil {
		return helpStyle.Render("no dataset highlighted")
	}
	st := m.statusOf(*d)
	line := func(label, val string) string { return labelStyle.Render(label) + val }
	var parts []string
	for _, p := range []string{d.Expires, d.FileCount, d.TotalSize, d.Outline} {
		if p == "" || p == "N/A" {
			continue
		}
		if p == d.FileCount {
			p += " files"
		}
		parts = append(parts, p)
	}
	who := d.Project
	if d.RequestedBy != "" {
		who = strings.TrimSpace(who + "  (requested by " + d.RequestedBy + ")")
	}
	if who == "" {
		who = helpStyle.Render("–")
	}
	return strings.Join([]string{
		line("Name", displayName(*d)),
		line("Project", who),
		line("MD5", d.MD5),
		line("Expires", strings.Join(parts, " · ")),
		line("Status", lipgloss.NewStyle().Foreground(statusColor(st)).Render(st)),
	}, "\n")
}

func ratio(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(1, float64(done)/float64(total))
}

func humanBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
