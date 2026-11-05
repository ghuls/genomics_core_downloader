package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

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

func helpBar(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, keyStyle.Render(pairs[i])+" "+keyDescStyle.Render(pairs[i+1]))
	}
	return strings.Join(parts, helpStyle.Render(" · "))
}
