package portal

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// reHTMLStart matches the places where Wt passes an HTML string literal to the
// browser; the literal itself starts right after the match.
var reHTMLStart = regexp.MustCompile(`(?:setHtml\(\s*([\w$]+)\s*,|([\w$]+)\.innerHTML\s*=|insertAdjacentHTML\(\s*'\w+'\s*,)\s*'`)

// Wt refers to DOM elements through short-lived variables:
//
//	var j12=Wt3_7_1.$('o3179f593');   or   j12.setAttribute('id', 'o3179f936');
var (
	reVarLookup = regexp.MustCompile(`var ([\w$]+)=Wt3_7_1\.\$\('(o\w+)'\)`)
	reVarSetID  = regexp.MustCompile(`([\w$]+)\.setAttribute\('id',\s*'(o\w+)'\)`)
)

// fragment is an HTML string together with the id of the element whose
// contents it becomes ("" if unknown).
type fragment struct {
	target string
	html   string
}

// targetedFragments is fragmentsOf, but also resolves the target element.
func targetedFragments(js string) []fragment {
	vars := map[string]string{}
	for _, re := range []*regexp.Regexp{reVarLookup, reVarSetID} {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			vars[m[1]] = m[2]
		}
	}
	var out []fragment
	for _, loc := range reHTMLStart.FindAllStringSubmatchIndex(js, -1) {
		s, ok := jsSingleQuoted(js[loc[1]:])
		if !ok || !strings.Contains(s, "<") {
			continue
		}
		var v string
		for g := 1; g <= 2; g++ {
			if loc[2*g] >= 0 {
				v = js[loc[2*g]:loc[2*g+1]]
			}
		}
		out = append(out, fragment{target: vars[v], html: s})
	}
	return out
}

// fragmentsOf extracts and unescapes all HTML string literals from a Wt
// JavaScript response.
func fragmentsOf(js string) []string {
	var out []string
	for _, f := range targetedFragments(js) {
		out = append(out, f.html)
	}
	return out
}

// jsSingleQuoted decodes a JS string literal whose opening quote has already
// been consumed.
func jsSingleQuoted(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			return b.String(), true
		case ch == '\\' && i+1 < len(s):
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'x':
				if i+2 < len(s) {
					if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
						b.WriteByte(byte(v))
						i += 2
						continue
					}
				}
				b.WriteByte(e)
			case 'u':
				if i+4 < len(s) {
					if v, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
						b.WriteRune(rune(v))
						i += 4
						continue
					}
				}
				b.WriteByte(e)
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(ch)
		}
	}
	return "", false
}

type node struct{ *html.Node }

type document struct{ roots []*html.Node }

func parseFragments(frags []string) document {
	var d document
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	for _, f := range frags {
		// Table fragments (e.g. a bare <tr>) need a matching context.
		nodes, err := html.ParseFragment(strings.NewReader(f), ctx)
		if err != nil {
			continue
		}
		d.roots = append(d.roots, nodes...)
	}
	return d
}

func (d document) walk(fn func(*node)) {
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode {
			fn(&node{n})
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	for _, r := range d.roots {
		rec(r)
	}
}

func (d document) find(pred func(*node) bool) (found *node) {
	d.walk(func(n *node) {
		if found == nil && pred(n) {
			found = n
		}
	})
	return found
}

func (n *node) attr(key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (n *node) id() string { return n.attr("id") }

func (n *node) hasClass(c string) bool {
	for _, f := range strings.Fields(n.attr("class")) {
		if f == c {
			return true
		}
	}
	return false
}

var reSignal = regexp.MustCompile(`Wt\._p_\.update\(o,\s*'(\w+)'`)

// signal returns the Wt signal emitted by the given event handler attribute.
func (n *node) signal(attr string) string {
	if m := reSignal.FindStringSubmatch(n.attr(attr)); m != nil {
		return m[1]
	}
	return ""
}

func (n *node) text() string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n.Node)
	return strings.Join(strings.Fields(b.String()), " ")
}

func (n *node) children(tag string) []*node {
	var out []*node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			out = append(out, &node{c})
		}
	}
	return out
}

// widgetNum converts a Wt widget id like "o317945f1" to a number; Wt
// allocates them sequentially.
func widgetNum(id string) (uint64, bool) {
	if len(id) < 2 || id[0] != 'o' {
		return 0, false
	}
	v, err := strconv.ParseUint(id[1:], 16, 64)
	return v, err == nil
}

// parseDatasets finds the "My Data" table (the one with a "Dataset" column).
func parseDatasets(frags []string) ([]Dataset, bool) {
	doc := parseFragments(frags)
	var table *node
	doc.walk(func(n *node) {
		if n.Data != "table" {
			return
		}
		sub := document{roots: []*html.Node{n.Node}}
		if sub.find(func(x *node) bool { return x.Data == "th" && x.text() == "Dataset" }) != nil {
			table = n // keep the last one: newest render wins
		}
	})
	if table == nil {
		return nil, false
	}

	sub := document{roots: []*html.Node{table.Node}}
	var cols []string
	sub.walk(func(n *node) {
		if n.Data == "th" {
			cols = append(cols, n.text())
		}
	})
	col := func(name string) int {
		for i, c := range cols {
			if strings.EqualFold(strings.TrimSpace(c), name) || strings.HasPrefix(strings.ToLower(c), strings.ToLower(name)) {
				return i
			}
		}
		return -1
	}
	iProject, iDataset, iMD5, iOutline := col("Project"), col("Dataset"), col("MD5"), col("Outline")
	iExp, iCount, iSize, iReq := col("Expiration"), col("File Count"), col("Total Size"), col("Requested by")

	var out []Dataset
	tbody := sub.find(func(n *node) bool { return n.Data == "tbody" })
	if tbody == nil {
		return out, true
	}
	for _, tr := range tbody.children("tr") {
		tds := tr.children("td")
		get := func(i int) string {
			if i < 0 || i >= len(tds) {
				return ""
			}
			return tds[i].text()
		}
		d := Dataset{
			Project: get(iProject), MD5: get(iMD5), Outline: get(iOutline), Expires: get(iExp),
			FileCount: get(iCount), TotalSize: get(iSize), RequestedBy: get(iReq),
		}
		d.Name = get(iDataset)
		if iDataset >= 0 && iDataset < len(tds) {
			cell := document{roots: []*html.Node{tds[iDataset].Node}}
			if btn := cell.find(func(x *node) bool { return x.Data == "button" && x.signal("onclick") != "" }); btn != nil {
				d.buttonID, d.signal = btn.id(), btn.signal("onclick")
			}
		}
		if strings.HasPrefix(d.Name, "Expired:") || d.signal == "" {
			d.Expired = true
			d.Name = strings.TrimSpace(strings.TrimPrefix(d.Name, "Expired:"))
		}
		out = append(out, d)
	}
	return out, true
}
