package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/ghuls/genomics_core_downloader/internal/credstore"
	"github.com/ghuls/genomics_core_downloader/internal/download"
	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestViews(t *testing.T) {
	var tm tea.Model = newModel(portal.New(), "/data/out", "someone", nil, termenv.ANSI256, 2)
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 130, Height: 34})
	if v := ansi.Strip(tm.View()); !strings.Contains(v, "Username") || !strings.Contains(strings.ReplaceAll(v, " ", ""), "GENOMICSCORE") {
		t.Fatalf("login view:\n%s", v)
	}
	ds := []portal.Dataset{
		{Name: "1700000001_gc-run-250301-nextseq2000-fca_gc-run-250301-...", MD5: "e8fd", Expires: "2027-01-28 17:42", FileCount: "2923", TotalSize: "96GiB"},
		{Project: "250101_DEMO_PROJECT_A", Name: "1700000002_x.zip", MD5: "e2d6", Expired: true, TotalSize: "15GiB", RequestedBy: "a@b.c", Expires: "2026-10-05 08:44"},
		{Name: "1700000005_gc-run-250115-nextseq2000-fca", MD5: "f958", Expired: true, TotalSize: "66GiB", Expires: "2023-04-12 17:01"},
	}
	tm, _ = tm.Update(datasetsMsg{ds: ds})
	tm, _ = tm.Update(key("down"))
	tm, _ = tm.Update(key("d"))
	v := ansi.Strip(tm.View())
	for _, want := range []string{"3 datasets", "Datasets", "Details", "Download", "Activity", "96GiB", "cannot be downloaded", "/ to filter"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	// Status is the last column of the header.
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "Expires (BE)") && !strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(strings.TrimSpace(l), "│"), " "), "Status") {
			t.Errorf("status not last: %q", l)
		}
	}
	for i, l := range strings.Split(v, "\n") {
		if w := ansi.StringWidth(l); w > 130 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}

	// Files and Size are right aligned.
	if !strings.Contains(v, "  2923 ") || !strings.Contains(v, "   96GiB ") {
		t.Errorf("files/size not right aligned:\n%s", v)
	}

	// The filter only looks at the name: an expiry date does not match.
	tm, _ = tm.Update(key("/"))
	for _, r := range "2023-04" {
		tm, _ = tm.Update(key(string(r)))
	}
	if m := tm.(model); len(m.visible) != 0 {
		t.Fatalf("filter matched outside the name: visible=%v", m.visible)
	}
	tm, _ = tm.Update(key("esc"))

	// Filter: two terms, both must match.
	tm, _ = tm.Update(key("/"))
	for _, r := range "nextseq 2501" {
		tm, _ = tm.Update(key(string(r)))
	}
	tm, _ = tm.Update(key("enter"))
	m := tm.(model)
	if len(m.visible) != 1 || m.datasets[m.visible[0]].MD5 != "f958" {
		t.Fatalf("filter: visible=%v", m.visible)
	}
	tm, _ = tm.Update(key("esc"))
	if m := tm.(model); len(m.visible) != 3 {
		t.Fatalf("esc should clear filter, visible=%v", m.visible)
	}

	// "/" while typing switches the filter to the project field.
	tm, _ = tm.Update(key("/"))
	for _, r := range "demo" {
		tm, _ = tm.Update(key(string(r)))
	}
	if m := tm.(model); len(m.visible) != 0 {
		t.Fatalf("name filter matched a project: visible=%v", m.visible)
	}
	tm, _ = tm.Update(key("/"))
	if m := tm.(model); len(m.visible) != 1 || m.datasets[m.visible[0]].MD5 != "e2d6" {
		t.Fatalf("project filter: visible=%v", m.visible)
	}
	if v := ansi.Strip(tm.View()); !strings.Contains(v, "project") || !strings.Contains(v, "press / again to filter on requested by") {
		t.Fatalf("project filter line:\n%s", v)
	}
	tm, _ = tm.Update(key("esc"))

	// Tab cycles the focus; arrows then scroll the activity panel.
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m := tm.(model); m.focus != focusDownloads {
		t.Fatalf("focus after tab = %v", m.focus)
	}
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m := tm.(model); m.focus != focusActivity {
		t.Fatalf("focus after 2 tabs = %v", m.focus)
	}
	m = tm.(model)
	for i := 0; i < 20; i++ {
		m.logf("message %d", i)
	}
	tm = m
	tm, _ = tm.Update(key("home"))
	if v := ansi.Strip(tm.View()); !strings.Contains(v, "message 0") || strings.Contains(v, "message 19") {
		t.Errorf("activity not scrolled to top:\n%s", v)
	}
	tm, _ = tm.Update(key("end"))
	if v := ansi.Strip(tm.View()); !strings.Contains(v, "message 19") {
		t.Errorf("activity not scrolled to end:\n%s", v)
	}
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m := tm.(model); m.focus != focusDownloads {
		t.Fatalf("focus after shift+tab = %v", m.focus)
	}

	// Several download jobs, one running with progress shown in the table.
	m = tm.(model)
	m.jobs = []*job{
		{id: 1, key: ds[0].Key(), ds: ds[0], state: jobRunning, prog: download.Progress{Phase: download.PhaseDownloading, Done: 42, Total: 100}, speed: 1 << 20},
		{id: 2, key: "q", ds: portal.Dataset{Name: "queued.zip"}, state: jobQueued},
		{id: 3, key: "f", ds: portal.Dataset{Name: "failed.zip"}, state: jobFailed, err: errors.New("boom")},
	}
	v = ansi.Strip(m.View())
	for _, want := range []string{"42%", "▰▰▰▱▱▱", "1 running · 1 queued · 1 finished", "queued.zip", "✗ boom", "1.0 MiB/s"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	for i, l := range strings.Split(v, "\n") {
		if w := ansi.StringWidth(l); w > 130 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}
	t.Log("\n" + m.View())
}

func TestSavedPasswordLogin(t *testing.T) {
	saved, err := credstore.Seal("someone", []byte("secret"), []byte("phrase"))
	if err != nil {
		t.Fatal(err)
	}
	var tm tea.Model = newModel(portal.New(), ".", "", saved, termenv.ANSI256, 2)
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := ansi.Strip(tm.View())
	if !strings.Contains(v, "Passphrase") || !strings.Contains(v, "someone") || strings.Contains(v, "Password:") {
		t.Fatalf("unlock view:\n%s", v)
	}
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyTab}) // type the password instead
	if v := ansi.Strip(tm.View()); !strings.Contains(v, "Password:") || !strings.Contains(v, "esc use saved password") {
		t.Fatalf("manual view:\n%s", v)
	}
	tm, _ = tm.Update(key("esc"))
	if m := tm.(model); !m.useSaved {
		t.Fatal("esc should go back to the saved password")
	}
	// A different -u ignores the saved password.
	if m := newModel(portal.New(), ".", "other", saved, termenv.ANSI256, 2); m.useSaved || m.saved != nil {
		t.Fatal("saved password used for another user")
	}
}

func TestEscDoesNotQuit(t *testing.T) {
	var tm tea.Model = newModel(portal.New(), ".", "someone", nil, termenv.ANSI256, 2)
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	tm, _ = tm.Update(datasetsMsg{ds: []portal.Dataset{{Name: "a.zip", MD5: "1"}}})
	tm, cmd := tm.Update(key("esc"))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("esc quit the main window")
		}
	}
	if _, cmd = tm.Update(key("q")); cmd == nil {
		t.Fatal("q did not quit")
	} else if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatal("q did not quit")
	}
}
