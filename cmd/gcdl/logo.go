package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Logo colours of Genomics Core Leuven: cyan, navy and the
// shades the "G" icon uses between them.
const (
	logoCyan     = lipgloss.Color("#0098ce")
	logoPacific  = lipgloss.Color("#0081b8")
	logoAzure    = lipgloss.Color("#076fa3")
	logoDeepCyan = lipgloss.Color("#0c6092")
	logoDeepBlue = lipgloss.Color("#115282")
	logoNavy     = lipgloss.Color("#193c6a")
)

// logoPalette and logoPixels are the "G" icon of
// https://pm.genomicscore.be/images/LogoGC.png scaled down to 12×12 pixels
// ('.' is transparent).
var logoPalette = map[byte]lipgloss.Color{
	'a': logoCyan,
	'b': logoPacific,
	'e': logoAzure,
	'i': logoDeepCyan,
	'm': logoDeepBlue,
	'n': logoNavy,
}

// spinnerShades are the logo blues from light to navy; the spinner goes
// through them and back.
var spinnerShades = []lipgloss.Color{logoCyan, logoPacific, logoAzure, logoDeepCyan, logoDeepBlue, logoNavy}

func spinnerShade(tick int) lipgloss.Color {
	n := len(spinnerShades) - 1
	i := tick % (2 * n)
	if i > n {
		i = 2*n - i
	}
	return spinnerShades[i]
}

var logoPixels = []string{
	"....aaaaa...",
	"..aaaaaaaa..",
	".aaaaaaaa...",
	".aaaa..a....",
	"aaaa........",
	"aaa.........",
	"aaa......bbb",
	"aaaa....bbbe",
	".aaaa..bbbbm",
	"..aaaabbbbin",
	"...aaabbbmnn",
	".....abennnn",
}

// renderIcon draws logoPixels with half block characters (two pixels per
// cell). Transparent pixels keep the terminal background.
func renderIcon() []string {
	var lines []string
	for y := 0; y+1 < len(logoPixels); y += 2 {
		var out strings.Builder
		for x := 0; x < len(logoPixels[y]); x++ {
			top, bottom := logoPixels[y][x], logoPixels[y+1][x]
			switch {
			case top != '.' && bottom != '.':
				out.WriteString(lipgloss.NewStyle().Foreground(logoPalette[top]).Background(logoPalette[bottom]).Render("▀"))
			case top != '.':
				out.WriteString(lipgloss.NewStyle().Foreground(logoPalette[top]).Render("▀"))
			case bottom != '.':
				out.WriteString(lipgloss.NewStyle().Foreground(logoPalette[bottom]).Render("▄"))
			default:
				out.WriteByte(' ')
			}
		}
		lines = append(lines, out.String())
	}
	return lines
}

// spaced puts a space between letters, like the wide lettering of the logo.
func spaced(s string) string { return strings.Join(strings.Split(s, ""), " ") }

// renderLogo returns the Genomics Core Leuven logo: the "G" icon and the name
// as text next to it. Without colour support it returns a plain title.
func renderLogo() string {
	if lipgloss.ColorProfile() == termenv.Ascii {
		return "GENOMICS CORE LEUVEN"
	}
	icon := renderIcon()
	navy := lipgloss.NewStyle().Foreground(logoNavy).Bold(true)
	cyan := lipgloss.NewStyle().Foreground(logoCyan)
	text := map[int]string{
		1: navy.Render(spaced("GENOMICS")),
		4: navy.Render(spaced("CORE")) + " " + cyan.Render(spaced("LEUVEN")),
	}
	for i, t := range text {
		icon[i] += "  " + t
	}
	return strings.Join(icon, "\n")
}
