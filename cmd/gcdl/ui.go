package main

import (
	"context"
	"errors"
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

// --- download jobs ----------------------------------------------------------

type jobState int

const (
	jobQueued jobState = iota
	jobStarting
	jobRunning
	jobDone
	jobSkipped
	jobMismatch
	jobFailed
	jobCancelled
)

func (s jobState) active() bool  { return s == jobStarting || s == jobRunning }
func (s jobState) pending() bool { return s == jobQueued || s.active() }

type job struct {
	id       int
	key      string
	ds       portal.Dataset
	state    jobState
	cancel   context.CancelFunc
	prog     download.Progress
	speed    float64 // bytes/s, smoothed
	lastDone int64
	lastTime time.Time
	res      download.Result
	err      error
}

// --- messages ---------------------------------------------------------------

type loginDoneMsg struct{ err error }
type datasetsMsg struct {
	ds  []portal.Dataset
	err error
}
type linkMsg struct {
	id  int
	ds  portal.Dataset
	url string
	err error
}
type progressMsg struct {
	id int
	p  download.Progress
}
type dlDoneMsg struct {
	id  int
	res download.Result
	err error
}
type keepAliveMsg struct{}

// --- model ------------------------------------------------------------------

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

func (m model) updateSaved(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, tea.Quit
	case "tab", "shift+tab":
		m.useSaved, m.err = false, ""
		return m, textinput.Blink
	case "enter":
		if m.unlock.Value() == "" {
			m.err = "please enter the passphrase"
			return m, nil
		}
		m.err = ""
		m.screen = screenLoading
		// textinput only hands out a string copy; take it as bytes (wiped
		// below) and drop the input's own copy right away.
		c, saved, phrase := m.client, m.saved, []byte(m.unlock.Value())
		m.unlock.Reset()
		return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
			pass, err := saved.Open(phrase)
			clear(phrase)
			if err != nil {
				return loginDoneMsg{err: err}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return loginDoneMsg{err: c.Login(ctx, saved.User, pass)}
		})
	}
	var cmd tea.Cmd
	m.unlock, cmd = m.unlock.Update(msg)
	return m, cmd
}

func (m model) updateLogin(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.useSaved {
		return m.updateSaved(msg)
	}
	switch msg.String() {
	case "esc":
		if m.saved != nil { // back to the saved password
			m.useSaved, m.err = true, ""
			return m, textinput.Blink
		}
		return m, tea.Quit
	case "tab", "shift+tab", "up", "down":
		if m.user.Focused() {
			m.user.Blur()
			m.pass.Focus()
		} else {
			m.pass.Blur()
			m.user.Focus()
		}
		return m, textinput.Blink
	case "enter":
		if m.user.Focused() {
			m.user.Blur()
			m.pass.Focus()
			return m, textinput.Blink
		}
		if strings.TrimSpace(m.user.Value()) == "" || m.pass.Value() == "" {
			m.err = "please enter username and password"
			return m, nil
		}
		m.err = ""
		m.screen = screenLoading
		// Login wipes pass; the input's copy is dropped right away.
		c, user, pass := m.client, strings.TrimSpace(m.user.Value()), []byte(m.pass.Value())
		m.pass.Reset()
		return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return loginDoneMsg{err: c.Login(ctx, user, pass)}
		})
	}
	var cmd tea.Cmd
	if m.user.Focused() {
		m.user, cmd = m.user.Update(msg)
	} else {
		m.pass, cmd = m.pass.Update(msg)
	}
	return m, cmd
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

func (m model) updateDownloads(key string) (tea.Model, tea.Cmd) {
	page := max(1, m.layout().dlLines-1)
	switch key {
	case "up", "k":
		m.dlCursor--
	case "down", "j":
		m.dlCursor++
	case "pgup", "ctrl+b":
		m.dlCursor -= page
	case "pgdown", "ctrl+f":
		m.dlCursor += page
	case "home", "g":
		m.dlCursor = 0
	case "end", "G":
		m.dlCursor = len(m.jobs) - 1
	case "x":
		if j := m.selectedJob(); j != nil && j.state.pending() {
			m.cancelJob(j)
			return m, m.schedule()
		}
	case "d", "enter": // retry
		if j := m.selectedJob(); j != nil && !j.state.pending() && j.state != jobDone && j.state != jobSkipped {
			j.state, j.err, j.speed = jobQueued, nil, 0
			j.prog = download.Progress{Total: -1}
			m.logf(helpStyle.Render("• %s: queued again"), displayName(j.ds))
			return m, m.schedule()
		}
	case "c": // clear finished downloads
		var keep []*job
		for _, j := range m.jobs {
			if j.state.pending() {
				keep = append(keep, j)
			}
		}
		m.jobs = keep
	}
	m.clampAll()
	return m, nil
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

// --- job management ---------------------------------------------------------

func (m *model) job(id int) *job {
	for _, j := range m.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

func (m *model) latestJob(key string) *job {
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if m.jobs[i].key == key {
			return m.jobs[i]
		}
	}
	return nil
}

func (m *model) selectedJob() *job {
	if m.dlCursor < 0 || m.dlCursor >= len(m.jobs) {
		return nil
	}
	return m.jobs[m.dlCursor]
}

func (m *model) dataset(key string) *portal.Dataset {
	for i := range m.datasets {
		if m.datasets[i].Key() == key {
			return &m.datasets[i]
		}
	}
	return nil
}

func (m *model) running() int {
	n := 0
	for _, j := range m.jobs {
		if j.state.active() {
			n++
		}
	}
	return n
}

// schedule starts queued jobs (in order) until the parallel limit is reached.
func (m *model) schedule() tea.Cmd {
	var cmds []tea.Cmd
	for _, j := range m.jobs {
		if m.running() >= m.parallel {
			break
		}
		if j.state != jobQueued {
			continue
		}
		j.state = jobStarting
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		j.cancel = cancel
		c, d, id := m.client, j.ds, j.id
		cmds = append(cmds, func() tea.Msg {
			defer cancel()
			url, d2, err := c.DownloadURL(ctx, d)
			return linkMsg{id: id, ds: d2, url: url, err: err}
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) cancelJob(j *job) {
	switch j.state {
	case jobQueued:
		j.state = jobCancelled
		m.logf(warnStyle.Render("■ %s: removed from queue"), displayName(j.ds))
	case jobStarting:
		if j.cancel != nil {
			j.cancel()
		}
		j.state = jobCancelled
		m.logf(warnStyle.Render("■ %s: cancelled"), displayName(j.ds))
	case jobRunning:
		if j.cancel != nil {
			j.cancel() // dlDoneMsg reports the result
		}
	}
}

func (m *model) cancelAll() {
	for _, j := range m.jobs {
		if j.state == jobQueued {
			j.state = jobCancelled
		}
		if j.state.active() && j.cancel != nil {
			j.cancel()
		}
	}
}

// finish records the outcome of a job.
func (m *model) finish(j *job, res download.Result, err error) {
	j.res, j.err = res, err
	name := displayName(j.ds)
	switch {
	case errors.Is(err, context.Canceled):
		j.state = jobCancelled
		m.logf(warnStyle.Render("■ %s: cancelled (partial file kept, will resume)"), name)
	case err != nil:
		j.state = jobFailed
		m.logf(errStyle.Render("✗ %s: %v"), name, err)
	case res.Skipped:
		j.state = jobSkipped
		m.logf(warnStyle.Render("• %s: already exists, skipped"), res.Path)
	case res.MD5Match:
		j.state = jobDone
		m.logf(okStyle.Render("✓ %s: done, MD5 OK (%s)"), res.Path, humanBytes(res.Size))
	default:
		j.state = jobMismatch
		m.logf(errStyle.Render("! %s: downloaded but MD5 %s ≠ expected %s"), res.Path, res.MD5, j.ds.MD5)
	}
}

func (m *model) setFileName(d portal.Dataset) {
	if ds := m.dataset(d.Key()); ds != nil && d.FileName != "" {
		ds.FileName = d.FileName
	}
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

// --- dataset list -----------------------------------------------------------

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

// --- layout -----------------------------------------------------------------

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

func (m model) statusOf(d portal.Dataset) string {
	if j := m.latestJob(d.Key()); j != nil {
		return jobLabel(j)
	}
	if d.Downloadable() {
		return "available"
	}
	return "expired"
}

func jobLabel(j *job) string {
	switch j.state {
	case jobQueued:
		return "queued"
	case jobStarting:
		return "starting"
	case jobRunning:
		if j.prog.Phase == download.PhaseHashingPartial {
			return "verifying"
		}
		return "downloading"
	case jobDone:
		return "done"
	case jobSkipped:
		return "exists"
	case jobMismatch:
		return "md5 mismatch"
	case jobFailed:
		return "failed"
	default:
		return "cancelled"
	}
}

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

// --- views ------------------------------------------------------------------

func (m model) View() string {
	switch m.screen {
	case screenLogin, screenLoading:
		return m.viewLogin()
	default:
		return m.viewList()
	}
}

// loginPrompt pads a login form label so that all fields line up.
func loginPrompt(label string) string { return fmt.Sprintf("%-12s", label+":") }

func (m model) viewLogin() string {
	var b strings.Builder
	b.WriteString(renderLogo() + "\n\n")
	b.WriteString(helpStyle.Render("Sign in to https://pm.genomicscore.be") + "\n\n")
	var (
		helpLogin  = helpBar("enter", "login", "tab", "switch field", "esc", "quit")
		helpSaved  = helpBar("enter", "unlock and login", "tab", "type password instead", "esc", "quit")
		helpManual = helpBar("enter", "login", "tab", "switch field", "esc", "use saved password")
	)
	help := helpLogin
	if m.useSaved {
		b.WriteString(loginPrompt("Username") + m.saved.User + helpStyle.Render("  (saved password)") + "\n")
		b.WriteString(m.unlock.View() + "\n\n")
		help = helpSaved
	} else {
		b.WriteString(m.user.View() + "\n")
		b.WriteString(m.pass.View() + "\n\n")
		if m.saved != nil {
			help = helpManual
		}
	}
	switch {
	case m.screen == screenLoading:
		b.WriteString(m.spinner.View() + " logging in and loading datasets…")
	case m.err != "":
		b.WriteString(errStyle.Render(m.err))
	default:
		b.WriteString(help)
	}
	// Keep the box the same size in both modes (it only grows for a long
	// error message).
	content := b.String()
	width := lipgloss.Width(content)
	if m.saved != nil {
		width = max(width, lipgloss.Width(helpSaved), lipgloss.Width(helpManual))
	}
	box := boxStyle.Width(width + boxStyle.GetHorizontalPadding()).Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
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

func miniBar(frac float64, w int) string {
	n := max(0, min(w, int(frac*float64(w)+0.5)))
	return strings.Repeat("▰", n) + strings.Repeat("▱", w-n)
}

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

func helpBar(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, keyStyle.Render(pairs[i])+" "+keyDescStyle.Render(pairs[i+1]))
	}
	return strings.Join(parts, helpStyle.Render(" · "))
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

func (m model) downloadsPanel(width int, l layout) string {
	inner := width - 4
	var running, queued, finished int
	var speed float64
	for _, j := range m.jobs {
		switch {
		case j.state.active():
			running++
			speed += j.speed
		case j.state == jobQueued:
			queued++
		default:
			finished++
		}
	}
	title := panelTitle.Render("Downloads")
	if len(m.jobs) > 0 {
		s := fmt.Sprintf("  %d running · %d queued · %d finished", running, queued, finished)
		if speed > 0 {
			s += " · " + humanBytes(int64(speed)) + "/s"
		}
		title += helpStyle.Render(s)
	}

	var body []string
	if len(m.jobs) == 0 {
		body = append(body, helpStyle.Render(fmt.Sprintf(
			"No downloads yet — select datasets with space and press d (up to %d run in parallel, --jobs).", m.parallel)))
	}
	// Columns: mark, name, then either a progress block or a status text.
	barW := max(10, min(30, inner/5))
	const pctW, bytesW, speedW, etaW = 4, 21, 12, 12
	progW := barW + 1 + pctW + 1 + bytesW + 1 + speedW + 1 + etaW
	nameW := max(20, inner-2-1-progW)
	pb := m.progress
	pb.Width = barW
	for i := m.dlOffset; i < min(len(m.jobs), m.dlOffset+l.dlLines); i++ {
		j := m.jobs[i]
		mark := "  "
		if i == m.dlCursor && m.focus == focusDownloads {
			mark = "▸ "
		}
		st := jobLabel(j)
		color := lipgloss.NewStyle().Foreground(statusColor(st))
		var right string
		switch {
		case j.state == jobRunning:
			frac := ratio(j.prog.Done, j.prog.Total)
			sp, eta := "", ""
			if j.prog.Phase == download.PhaseHashingPartial {
				sp = "verifying"
			} else if j.speed > 0 {
				sp = humanBytes(int64(j.speed)) + "/s"
				if j.prog.Total > 0 {
					eta = "ETA " + (time.Duration(float64(j.prog.Total-j.prog.Done)/j.speed) * time.Second).Round(time.Second).String()
				}
			}
			right = pb.ViewAs(frac) + " " + cellRight(fmt.Sprintf("%.0f%%", 100*frac), pctW) + " " +
				cellRight(humanBytes(j.prog.Done)+" / "+humanBytes(j.prog.Total), bytesW) + " " +
				cellRight(sp, speedW) + " " + cellRight(eta, etaW)
		case j.state == jobStarting:
			right = m.spinner.View() + color.Render(" requesting download link…")
		case j.state == jobQueued:
			right = color.Render("queued")
		case j.state == jobDone:
			right = color.Render("✓ done · MD5 OK · " + humanBytes(j.res.Size))
		case j.state == jobSkipped:
			right = color.Render("• already exists, skipped")
		case j.state == jobMismatch:
			right = color.Render("! MD5 mismatch: got " + j.res.MD5)
		case j.state == jobFailed:
			right = color.Render("✗ " + fmt.Sprint(j.err))
		default:
			right = color.Render("■ cancelled (partial file kept)")
		}
		name := cell(displayName(j.ds), nameW)
		if i == m.dlCursor && m.focus == focusDownloads {
			name = cursorStyle.Render(name)
		}
		body = append(body, mark+name+" "+right)
	}
	if n := len(m.jobs); n > l.dlLines {
		title += helpStyle.Render(fmt.Sprintf(" · %d–%d of %d", m.dlOffset+1, min(n, m.dlOffset+l.dlLines), n))
	}
	return panel(title, strings.Join(body, "\n"), width, l.dlLines, m.focus == focusDownloads)
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

func matches(n int) string {
	if n == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", n)
}

func displayName(d portal.Dataset) string { return d.DisplayName() }

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
