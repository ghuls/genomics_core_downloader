// Package portal implements a minimal client for the Genomics Core project
// management portal (https://pm.genomicscore.be/), which is a Wt (C++ web
// toolkit) application. There is no API: the client speaks Wt's AJAX protocol
// (request=jsupdate POSTs carrying signal names) and scrapes the HTML that the
// server pushes back inside JavaScript responses.
package portal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/net/html"
)

const (
	DefaultBaseURL = "https://pm.genomicscore.be"
	myDataPath     = "/my-data"
)

// UserAgent is sent with every request; main sets the version.
var UserAgent = "GenomicCoreDownloader"

var (
	ErrBadCredentials  = errors.New("login or password is wrong")
	ErrSessionExpired  = errors.New("portal session expired")
	ErrNotDownloadable = errors.New("dataset is expired and can no longer be downloaded")
)

// Client is a single Wt session. It is safe for concurrent use; requests are
// serialised because Wt requires strictly ordered ackIds.
type Client struct {
	BaseURL string
	// DebugDir, if set, receives a copy of every request and response
	// (passwords are blanked out).
	DebugDir string
	HTTP     *http.Client

	mu         sync.Mutex
	username   string
	password   *memguard.Enclave // encrypted in locked memory, see Login
	passField  string            // id of the password input while logging in
	sessionURL string            // e.g. /;jsessionid=...?wtd=...
	ackID      string
	pageID     string
	path       string // current Wt internal path ("_" parameter)
	formValues map[string]string
	fragments  []fragment // every HTML fragment received, for later lookups
	dialogOpen string     // id of a shown modal dialog we could not close ("" if none)
	loggedIn   bool
	datasets   []Dataset
	debugSeq   int
}

// Dataset is one row of the "My Data" table.
type Dataset struct {
	Project     string
	Name        string // display name; truncated by the portal for downloadable sets
	FileName    string // real file name, known once the download link has been fetched
	MD5         string
	Outline     string
	Expires     string
	FileCount   string
	TotalSize   string
	RequestedBy string
	Expired     bool

	buttonID string
	signal   string
}

// Downloadable reports whether the portal still offers a download link.
func (d Dataset) Downloadable() bool { return !d.Expired && d.signal != "" }

// Key identifies a dataset across sessions (widget ids and signals change).
func (d Dataset) Key() string { return d.MD5 + "|" + d.Name }

// DisplayName is the real file name when known, else the (possibly
// truncated) name shown by the portal.
func (d Dataset) DisplayName() string {
	if d.FileName != "" {
		return d.FileName
	}
	return d.Name
}

func New() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Jar: jar, Timeout: 60 * time.Second},
	}
}

func (c *Client) reset() {
	jar, _ := cookiejar.New(nil)
	c.HTTP.Jar = jar
	c.sessionURL, c.ackID, c.pageID, c.path = "", "", "", ""
	c.formValues = map[string]string{}
	c.fragments = nil
	c.datasets = nil
	c.dialogOpen = ""
	c.loggedIn = false
}

// Login starts a new Wt session and logs in. The password is moved into a
// memguard enclave (encrypted with a random key, in locked memory that is not
// swapped; memguard also disables core dumps) and the password slice is wiped.
// It is kept that way so that an expired session can be re-established
// transparently, and only decrypted (into locked memory) while logging in.
func (c *Client) Login(ctx context.Context, username string, password []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(password) == 0 {
		return ErrBadCredentials
	}
	c.username, c.password = username, memguard.NewEnclave(password)
	return c.login(ctx)
}

var (
	reSessionURL = regexp.MustCompile(`var w='([^']*)'`)
	reSID        = regexp.MustCompile(`"&sid="\+(-?\d+)`)
	reResponse   = regexp.MustCompile(`Wt\._p_\.response\((-?\d+)\)`)
	reSetPage    = regexp.MustCompile(`Wt\._p_\.setPage\((\d+)\)`)
)

func (c *Client) login(ctx context.Context) error {
	c.reset()

	boot, err := c.get(ctx, c.BaseURL+"/")
	if err != nil {
		return fmt.Errorf("loading portal: %w", err)
	}
	m := reSessionURL.FindStringSubmatch(boot)
	sid := reSID.FindStringSubmatch(boot)
	if m == nil || sid == nil {
		return errors.New("unexpected bootstrap page (Cloudflare challenge or portal change?)")
	}
	c.sessionURL = m[1]
	if !strings.HasPrefix(c.sessionURL, "/") {
		return fmt.Errorf("unexpected session url %q", c.sessionURL)
	}

	q := url.Values{}
	q.Set("sid", sid[1])
	q.Set("scrW", "1920")
	q.Set("scrH", "1080")
	q.Set("tz", "60")
	q.Set("tzS", "Europe/Brussels")
	q.Set("htmlHistory", "true")
	q.Set("deployPath", "/")
	q.Set("request", "script")
	q.Set("rand", fmt.Sprint(time.Now().UnixNano()%1000000))
	script, err := c.get(ctx, c.BaseURL+c.sessionURL+"&"+q.Encode())
	if err != nil {
		return fmt.Errorf("loading portal script: %w", err)
	}
	if err := c.absorb(script, true); err != nil {
		return err
	}
	if c.ackID == "" || c.pageID == "" {
		return errors.New("could not find Wt session state in portal script")
	}

	doc := parseFragments(c.htmlSeen())
	userIn := doc.find(func(n *node) bool { return n.Data == "input" && n.attr("placeholder") == "login" })
	passIn := doc.find(func(n *node) bool { return n.Data == "input" && n.attr("type") == "password" })
	if userIn == nil || passIn == nil {
		return errors.New("login form not found on portal")
	}
	// A key press in the username field enables the (server-side disabled)
	// Login button, as in a browser. Send it while both fields are still
	// empty, so the password only goes out with the final Enter.
	c.formValues[userIn.id()] = ""
	c.formValues[passIn.id()] = ""
	if sig := userIn.signal("onkeyup"); sig != "" {
		if _, err := c.post(ctx, sig, userIn.id(), keyEvent("keyup", 65)); err != nil {
			return err
		}
	}
	delete(c.formValues, passIn.id())
	c.formValues[userIn.id()] = c.username
	// post adds the password to the requests while passField is set.
	c.passField = passIn.id()
	defer func() { c.passField = "" }()

	var resp string
	if sig := passIn.signal("onkeypress"); sig != "" {
		resp, err = c.post(ctx, sig, passIn.id(), keyEvent("keypress", 13))
	} else {
		btn := doc.find(func(n *node) bool { return n.Data == "button" && strings.TrimSpace(n.text()) == "Login" })
		if btn == nil || btn.signal("onclick") == "" {
			return errors.New("login button not found on portal")
		}
		resp, err = c.post(ctx, btn.signal("onclick"), btn.id(), clickEvent())
	}
	if err != nil {
		return err
	}
	if strings.Contains(resp, "Login or password is wrong") {
		return ErrBadCredentials
	}
	c.loggedIn = true
	return nil
}

// ListDatasets navigates to "My Data" > "My Storage Service" and returns all
// datasets. Calling it again starts a fresh session, so the list is current.
func (c *Client) ListDatasets(ctx context.Context) ([]Dataset, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.datasets != nil || !c.loggedIn {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}
	ds, err := c.listDatasets(ctx)
	if errors.Is(err, ErrSessionExpired) {
		if err = c.login(ctx); err == nil {
			ds, err = c.listDatasets(ctx)
		}
	}
	return ds, err
}

// listDatasets loads the table and fills in the full file names, which the
// portal truncates in the table but shows in each dataset's Download dialog.
func (c *Client) listDatasets(ctx context.Context) ([]Dataset, error) {
	if _, err := c.loadTable(ctx); err != nil {
		return nil, err
	}
	fillFileNames(c.datasets, dialogFileNames(c.fragments))
	for i := range c.datasets {
		d := c.datasets[i]
		if !d.Downloadable() || d.FileName != "" {
			continue
		}
		// Dialog not rendered yet: open (and close) it to read the name.
		if _, _, err := c.downloadURL(ctx, d); err != nil {
			if errors.Is(err, ErrSessionExpired) {
				return nil, err
			}
			if c.dialogOpen != "" {
				break // could not close the dialog; further clicks would be ignored
			}
		}
	}
	return append([]Dataset(nil), c.datasets...), nil
}

// dialogFileNames returns the file names of all download links seen so far.
func dialogFileNames(frags []fragment) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range frags {
		s := stdhtml.UnescapeString(f.html)
		for _, u := range reSignedURL.FindAllString(s, -1) {
			if n := fileNameFromURL(u); n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// fillFileNames matches truncated table names ("1700000001_gc-run-…-...")
// against full file names; only unambiguous prefix matches are used.
func fillFileNames(ds []Dataset, names []string) {
	for i := range ds {
		d := &ds[i]
		if d.FileName != "" || !d.Downloadable() {
			continue
		}
		prefix := strings.TrimSpace(d.Name)
		prefix = strings.TrimSuffix(strings.TrimSuffix(prefix, "..."), "…")
		if prefix == "" {
			continue
		}
		var match string
		n := 0
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				match = name
				n++
			}
		}
		if n == 1 {
			d.FileName = match
		}
	}
}

// clickables returns elements with a click signal that match pred, or whose
// parent/child matches pred (the handler can sit on an <a>, its <li> or a
// <span> inside), in document order.
func (c *Client) clickables(pred func(*node) bool) []*node {
	doc := parseFragments(c.htmlSeen())
	var out []*node
	seen := map[string]bool{}
	add := func(n *node) {
		if n != nil && n.id() != "" && n.signal("onclick") != "" && !seen[n.id()] {
			seen[n.id()] = true
			out = append(out, n)
		}
	}
	doc.walk(func(n *node) {
		if !pred(n) {
			return
		}
		add(n)
		sub := document{roots: []*html.Node{n.Node}}
		sub.walk(add)
		for p := n.Parent; p != nil; p = p.Parent {
			if p.Type == html.ElementNode {
				add(&node{p})
			}
		}
	})
	return out
}

func isMyDataMenu(n *node) bool {
	return strings.Contains(n.attr("href"), "my-data") || strings.EqualFold(n.text(), "My Data")
}

// The dataset table lives in the "My Storage Service" entry of the My Data
// page's submenu (the page opens on "Service Description").
func isStorageMenu(n *node) bool {
	return n.Data == "a" && strings.Contains(strings.ToLower(n.text()), "storage")
}

func (c *Client) loadTable(ctx context.Context) ([]Dataset, error) {
	if !c.loggedIn {
		return nil, errors.New("not logged in")
	}
	try := func(resp string) bool {
		ds, ok := parseDatasets(fragmentsOf(resp))
		if ok {
			c.datasets = ds
		}
		return ok
	}

	// Changing the internal path is what clicking the "My Data" menu item does.
	c.path = myDataPath
	resp, err := c.post(ctx, "hash", "", nil)
	if err != nil {
		return nil, err
	}
	if try(resp) {
		return c.datasets, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		if items := c.clickables(isStorageMenu); len(items) > 0 {
			item := items[len(items)-1] // newest render
			if resp, err = c.post(ctx, item.signal("onclick"), item.id(), clickEvent()); err != nil {
				return nil, err
			}
			if try(resp) {
				return c.datasets, nil
			}
		}
		if attempt == 0 {
			// The My Data page may not be shown: click its menu entry.
			items := c.clickables(isMyDataMenu)
			if len(items) == 0 {
				break
			}
			if resp, err = c.post(ctx, items[0].signal("onclick"), items[0].id(), clickEvent()); err != nil {
				return nil, err
			}
			if try(resp) {
				return c.datasets, nil
			}
		}
	}
	msg := "could not find the dataset table on the My Data > My Storage Service page"
	if c.DebugDir == "" {
		msg += " (rerun with --debug=<dir> to save the portal traffic)"
	}
	return nil, errors.New(msg)
}

var reSignedURL = regexp.MustCompile(`https://storage\.googleapis\.com/[^'"\s<>\\]+`)

// DownloadURL asks the portal for the signed download link of a dataset. The
// returned dataset has FileName filled in.
func (c *Client) DownloadURL(ctx context.Context, d Dataset) (string, Dataset, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !d.Downloadable() {
		return "", d, ErrNotDownloadable
	}
	link, d2, err := c.downloadURL(ctx, d)
	if err != nil && !errors.Is(err, ErrNotDownloadable) {
		// Fresh session and retry once: covers expired sessions and a modal
		// dialog that we did not manage to close.
		if lerr := c.login(ctx); lerr != nil {
			return "", d, lerr
		}
		if _, lerr := c.loadTable(ctx); lerr != nil {
			return "", d, lerr
		}
		link, d2, err = c.downloadURL(ctx, d)
	}
	return link, d2, err
}

func (c *Client) downloadURL(ctx context.Context, d Dataset) (string, Dataset, error) {
	if c.dialogOpen != "" {
		return "", d, errors.New("a portal dialog is still open")
	}
	// Widget ids and signals are per session: look the dataset up again.
	var cur *Dataset
	for i := range c.datasets {
		if c.datasets[i].Key() == d.Key() {
			cur = &c.datasets[i]
			break
		}
	}
	if cur == nil {
		return "", d, errors.New("dataset not found in current session")
	}
	if !cur.Downloadable() {
		return "", d, ErrNotDownloadable
	}
	resp, err := c.post(ctx, cur.signal, cur.buttonID, clickEvent())
	if err != nil {
		return "", d, err
	}

	// Every dataset has its own "Download …" dialog. The portal often renders
	// it ahead of time and the click only shows it, so read the link from the
	// dialog's own HTML rather than from the click response.
	dialog := shownDialog(resp)
	var dialogHTML string
	if dialog != "" {
		c.dialogOpen = dialog
		for _, f := range c.fragments {
			if f.target == dialog {
				dialogHTML = f.html // newest render wins
			}
		}
	}
	link := findSignedURL(dialogHTML)
	if link == "" {
		link = findSignedURL(resp)
	}
	if dialog != "" {
		c.closeDialog(ctx, dialog, dialogHTML)
	}
	if link == "" {
		return "", d, errors.New("portal did not return a download link")
	}
	d.FileName = fileNameFromURL(link)
	cur.FileName = d.FileName
	return link, d, nil
}

func findSignedURL(js string) string {
	s := stdhtml.UnescapeString(strings.NewReplacer(`\/`, `/`, `\x26`, `&`, `\u0026`, `&`).Replace(js))
	return reSignedURL.FindString(s)
}

var reShownDialog = regexp.MustCompile(`Wt3_7_1\.\$\('(o\w+)'\);\s*if \(o && o\.wtPopup\) o\.wtPopup\.shown\(\)`)

func shownDialog(js string) string {
	if m := reShownDialog.FindStringSubmatch(js); m != nil {
		return m[1]
	}
	return ""
}

// closeDialog clicks the "×" of a download dialog. Wt drops events for widgets
// outside a modal dialog, so it must be closed before the next dataset can be
// clicked.
func (c *Client) closeDialog(ctx context.Context, dialogID, dialogHTML string) {
	isClose := func(n *node) bool { return n.hasClass("close") && n.signal("onclick") != "" }
	closer := parseFragments([]string{dialogHTML}).find(isClose)
	if closer == nil {
		// Dialog HTML unknown: take the first close button rendered after the
		// dialog element (Wt allocates widget ids sequentially).
		dialogNum, ok := widgetNum(dialogID)
		if !ok {
			return
		}
		var bestNum uint64
		parseFragments(c.htmlSeen()).walk(func(n *node) {
			if num, ok := widgetNum(n.id()); ok && isClose(n) && num > dialogNum && (closer == nil || num < bestNum) {
				closer, bestNum = n, num
			}
		})
	}
	if closer == nil {
		return
	}
	resp, err := c.post(ctx, closer.signal("onclick"), closer.id(), clickEvent())
	if err == nil && strings.Contains(resp, "wtPopup.hidden()") {
		c.dialogOpen = ""
	}
}

// KeepAlive pings the session so it does not time out while idle.
func (c *Client) KeepAlive(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loggedIn {
		return nil
	}
	_, err := c.post(ctx, "keepAlive", "", nil)
	return err
}

func (c *Client) htmlSeen() []string {
	out := make([]string, len(c.fragments))
	for i, f := range c.fragments {
		out[i] = f.html
	}
	return out
}

// --- low level protocol -----------------------------------------------------

func (c *Client) get(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", c.BaseURL+"/")
	return c.do(req, nil)
}

func (c *Client) do(req *http.Request, form url.Values) (string, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.dump(req, form, 0, []byte(err.Error()))
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	c.dump(req, form, resp.StatusCode, body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return string(body), nil
}

// dump writes a request/response pair to DebugDir.
func (c *Client) dump(req *http.Request, form url.Values, status int, body []byte) {
	if c.DebugDir == "" {
		return
	}
	c.debugSeq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\nHTTP %d\n\n", req.Method, req.URL.String(), status)
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, form.Get(k))
	}
	base := filepath.Join(c.DebugDir, fmt.Sprintf("%03d", c.debugSeq))
	_ = os.WriteFile(base+"-request.txt", []byte(b.String()), 0o600)
	_ = os.WriteFile(base+"-response.js", body, 0o600)
}

// post emits a Wt signal and returns the JavaScript the server sends back.
func (c *Client) post(ctx context.Context, signal, targetID string, event url.Values) (string, error) {
	v := url.Values{}
	v.Set("request", "jsupdate")
	v.Set("signal", signal)
	for k, val := range c.formValues {
		v.Set(k, val)
	}
	if c.path != "" {
		v.Set("_", c.path)
	}
	if targetID != "" {
		v.Set("tid", targetID)
	}
	for k, vals := range event {
		v[k] = vals
	}
	v.Set("ackId", c.ackID)
	v.Set("pageId", c.pageID)

	body, err := c.formBody(v)
	if err != nil {
		return "", err
	}
	defer clear(body) // may contain the password
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+c.sessionURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.BaseURL)
	req.Header.Set("Referer", c.BaseURL+myDataPath)
	resp, err := c.do(req, v)
	if err != nil {
		return "", err
	}
	if err := c.absorb(resp, false); err != nil {
		return "", err
	}
	return resp, nil
}

// formBody encodes v and, while logging in, the password. The password is
// only decrypted into locked memory and written straight into the returned
// slice (never into a string), which the caller must wipe. In v (used for the
// debug dump) it is replaced by "<redacted>".
func (c *Client) formBody(v url.Values) ([]byte, error) {
	enc := v.Encode()
	if c.passField == "" || c.password == nil {
		return []byte(enc), nil
	}
	pw, err := c.password.Open()
	if err != nil {
		return nil, err
	}
	defer pw.Destroy()
	key := url.QueryEscape(c.passField)
	// Allocate the final size up front so append never copies the password.
	body := make([]byte, 0, len(enc)+1+len(key)+1+3*pw.Size())
	body = append(body, enc...)
	body = append(body, '&')
	body = append(body, key...)
	body = append(body, '=')
	body = appendQueryEscape(body, pw.Bytes())
	v.Set(c.passField, "<redacted>")
	return body, nil
}

// appendQueryEscape appends src escaped like url.QueryEscape.
func appendQueryEscape(dst, src []byte) []byte {
	const hex = "0123456789ABCDEF"
	for _, b := range src {
		switch {
		case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9', b == '-', b == '_', b == '.', b == '~':
			dst = append(dst, b)
		case b == ' ':
			dst = append(dst, '+')
		default:
			dst = append(dst, '%', hex[b>>4], hex[b&15])
		}
	}
	return dst
}

// absorb updates session state from a server response.
// The bootstrap script contains the whole Wt library, so the expiry checks
// only apply to jsupdate responses.
func (c *Client) absorb(js string, bootstrap bool) error {
	if strings.Contains(js, "Script id mismatch") || (!bootstrap && (strings.HasPrefix(js, "Wt._p_.quit(") ||
		strings.Contains(js, "window.location.reload") || strings.Contains(js, "location.reload("))) {
		c.loggedIn = false
		return ErrSessionExpired
	}
	if m := reResponse.FindAllStringSubmatch(js, -1); m != nil {
		c.ackID = m[len(m)-1][1]
	} else if !strings.HasPrefix(strings.TrimSpace(js), "{") {
		return fmt.Errorf("unexpected portal response: %.200q", js)
	} else {
		// "{}" without a response id: Wt ignored our request (stale state).
		c.loggedIn = false
		return ErrSessionExpired
	}
	if m := reSetPage.FindAllStringSubmatch(js, -1); m != nil {
		c.pageID = m[len(m)-1][1]
	}
	c.fragments = append(c.fragments, targetedFragments(js)...)
	return nil
}

func baseEvent(typ string) url.Values {
	v := url.Values{}
	v.Set("type", typ)
	for _, k := range []string{"clientX", "clientY", "documentX", "documentY", "dragdX", "dragdY",
		"screenX", "screenY", "scrollX", "scrollY", "width", "height", "widgetX", "widgetY"} {
		v.Set(k, "0")
	}
	v.Set("wheel", "-1")
	return v
}

func clickEvent() url.Values {
	v := baseEvent("click")
	v.Set("button", "1")
	v.Set("charCode", "0")
	return v
}

func keyEvent(typ string, key int) url.Values {
	v := baseEvent(typ)
	v.Set("button", "0")
	v.Set("keyCode", fmt.Sprint(key))
	if typ == "keypress" {
		v.Set("charCode", fmt.Sprint(key))
	} else {
		v.Set("charCode", "0")
	}
	return v
}

func fileNameFromURL(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	p := u.Path
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}
