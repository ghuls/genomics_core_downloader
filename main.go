// Command genomics_core_downloader is a terminal UI to log in to the Genomics
// Core portal (https://pm.genomicscore.be/), list the datasets on the
// "My Data" page and download the ones that have not expired yet.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/awnumar/memguard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	flag "github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/ghuls/genomics_core_downloader/internal/credstore"
	"github.com/ghuls/genomics_core_downloader/internal/portal"
)

func main() {
	// Wipe memguard's locked memory (the encrypted password and its key) on
	// a normal exit.
	defer memguard.Purge()
	outDir := flag.StringP("output", "o", ".", "directory to download datasets to")
	username := flag.StringP("user", "u", os.Getenv("GENOMICSCORE_USER"), "portal username (default $GENOMICSCORE_USER)")
	parallel := flag.IntP("jobs", "j", 2, "number of downloads to run in parallel")
	list := flag.BoolP("list", "l", false, "do not start the TUI: log in, print the dataset table and exit")
	savePassword := flag.Bool("save-password", false, "save the portal password encrypted with a passphrase of your choice\n(in $XDG_CONFIG_HOME/gcdl/credentials.json) and exit")
	forgetPassword := flag.Bool("forget-password", false, "delete the saved password and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	colorMode := flag.String("color", "auto", "colours: auto, never, 16, 256 or truecolor\n(auto treats tmux/screen as 256 colours; NO_COLOR is honoured)")
	debugDir := flag.String("debug", "", "save the portal traffic to this directory (contains session data and download links)")
	flag.CommandLine.SortFlags = false
	_ = flag.CommandLine.MarkHidden("debug")
	if !credstore.Supported {
		_ = flag.CommandLine.MarkHidden("save-password")
		_ = flag.CommandLine.MarkHidden("forget-password")
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("gcdl", version())
		return
	}
	portal.UserAgent = "GenomicCoreDownloader/" + version().String()

	profile, err := colorProfile(*colorMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	setColorProfile(profile)

	credPath, err := credstore.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if (*savePassword || *forgetPassword) && !credstore.Supported {
		fmt.Fprintln(os.Stderr, "error: saving the password is not supported on this platform (the file cannot be kept private)")
		os.Exit(2)
	}
	if *forgetPassword {
		switch err := os.Remove(credPath); {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintln(os.Stderr, "no saved password")
		case err != nil:
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		default:
			fmt.Fprintln(os.Stderr, "saved password removed:", credPath)
		}
		return
	}
	var saved *credstore.File
	if credstore.Supported {
		saved, err = credstore.Load(credPath)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) && !*savePassword {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "(remove it with --forget-password or replace it with --save-password)")
		os.Exit(1)
	}

	if st, err := os.Stat(*outDir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "output directory %q does not exist\n", *outDir)
		os.Exit(1)
	}

	client := portal.New()
	if *debugDir != "" {
		if err := os.MkdirAll(*debugDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		client.DebugDir = *debugDir
	}
	if *savePassword {
		if err := runSavePassword(client, credPath, *username, saved); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if *list {
		if err := runList(client, *username, saved); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	p := tea.NewProgram(newModel(client, *outDir, *username, saved, profile, *parallel), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var stdin = bufio.NewReader(os.Stdin)

func prompt(label string) string {
	fmt.Fprint(os.Stderr, label)
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

// promptSecret reads a secret without echo. It is returned as a byte slice
// (never a string) so that it can be wiped after use.
func promptSecret(label string) ([]byte, error) {
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return b, err
}

// credentials returns the username and password for a non-interactive run:
// from the saved password (asking for its passphrase) or by prompting.
func credentials(user string, saved *credstore.File) (string, []byte, error) {
	if saved != nil && (user == "" || user == saved.User) {
		phrase, err := promptSecret(fmt.Sprintf("Passphrase for the saved password of %s: ", saved.User))
		defer clear(phrase)
		if err != nil {
			return "", nil, err
		}
		pass, err := saved.Open(phrase)
		return saved.User, pass, err
	}
	if user == "" {
		user = prompt("Username: ")
	}
	pass, err := promptSecret("Password: ")
	return user, pass, err
}

func runList(c *portal.Client, user string, saved *credstore.File) error {
	user, pass, err := credentials(user, saved)
	defer clear(pass) // Login already wipes it; this covers the error paths
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.Login(ctx, user, pass); err != nil {
		return err
	}
	ds, err := c.ListDatasets(ctx)
	if err != nil {
		return err
	}
	printTable(os.Stdout, ds)
	return nil
}

// runSavePassword asks for the portal credentials, checks them by logging in
// and saves the password encrypted with a new passphrase.
func runSavePassword(c *portal.Client, path, user string, saved *credstore.File) error {
	if user == "" && saved != nil {
		user = saved.User
	}
	if user == "" {
		user = prompt("Username: ")
	}
	if user == "" {
		return errors.New("no username given")
	}
	pass, err := promptSecret(fmt.Sprintf("Portal password for %s: ", user))
	defer clear(pass)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "checking the password with https://pm.genomicscore.be…")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.Login(ctx, user, bytes.Clone(pass)); err != nil { // Login wipes its copy
		return err
	}
	fmt.Fprintln(os.Stderr, "Choose a passphrase to encrypt the saved password (asked each time gcdl starts).")
	phrase, err := promptSecret("Passphrase: ")
	defer clear(phrase)
	if err != nil {
		return err
	}
	if len(phrase) == 0 {
		return errors.New("the passphrase cannot be empty")
	}
	again, err := promptSecret("Repeat passphrase: ")
	defer clear(again)
	if err != nil {
		return err
	}
	if !bytes.Equal(again, phrase) {
		return errors.New("the passphrases do not match")
	}
	f, err := credstore.Seal(user, pass, phrase)
	if err != nil {
		return err
	}
	if err := credstore.Save(path, f); err != nil {
		return err
	}
	if saved != nil && saved.User != user {
		fmt.Fprintf(os.Stderr, "replaced the saved password of %s\n", saved.User)
	}
	fmt.Fprintln(os.Stderr, "password saved encrypted in", path)
	return nil
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
