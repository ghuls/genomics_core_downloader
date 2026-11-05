package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ghuls/genomics_core_downloader/internal/download"
	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

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
