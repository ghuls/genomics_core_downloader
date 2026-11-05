package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// colorProfile picks the terminal colour profile.
//
// termenv only enables colours when $TERM contains "256color", "color" or
// "ansi" (or $COLORTERM is set). Inside tmux/screen $TERM is often plain
// "screen" or "tmux", and $COLORTERM is usually not forwarded over ssh, so
// detection falls back to no colours even though tmux supports 256 colours.
func colorProfile(mode string) (termenv.Profile, error) {
	switch strings.ToLower(mode) {
	case "never", "none", "off":
		return termenv.Ascii, nil
	case "16", "ansi":
		return termenv.ANSI, nil
	case "256", "always", "on":
		return termenv.ANSI256, nil
	case "truecolor", "24bit":
		return termenv.TrueColor, nil
	case "auto", "":
	default:
		return termenv.Ascii, fmt.Errorf("invalid --color %q (use auto, never, 16, 256 or truecolor)", mode)
	}
	if termenv.EnvNoColor() {
		return termenv.Ascii, nil
	}
	p := termenv.EnvColorProfile() // also honours CLICOLOR_FORCE
	if p == termenv.Ascii && inMultiplexer() && term.IsTerminal(int(os.Stdout.Fd())) {
		return termenv.ANSI256, nil
	}
	return p, nil
}

func inMultiplexer() bool {
	term := os.Getenv("TERM")
	return os.Getenv("TMUX") != "" || os.Getenv("STY") != "" ||
		strings.HasPrefix(term, "screen") || strings.HasPrefix(term, "tmux")
}

func setColorProfile(p termenv.Profile) {
	lipgloss.SetColorProfile(p)
}
