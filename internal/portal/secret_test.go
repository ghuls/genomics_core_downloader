package portal

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/awnumar/memguard"
)

func TestFormBodyPassword(t *testing.T) {
	pw := []byte("p@ss wörd&=+%/~")
	want := url.QueryEscape(string(pw))
	c := &Client{password: memguard.NewEnclave(pw), passField: "o1a2"}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Fatal("NewEnclave did not wipe the password slice")
	}
	v := url.Values{"request": {"jsupdate"}, "o1a1": {"someone"}}
	body, err := c.formBody(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("o1a2") != "p@ss wörd&=+%/~" || got.Get("o1a1") != "someone" || !strings.Contains(string(body), "o1a2="+want) {
		t.Fatalf("body %q", body)
	}
	if v.Get("o1a2") != "<redacted>" {
		t.Fatalf("dump form has %q", v.Get("o1a2"))
	}
	// Outside a login the password is not sent.
	c.passField = ""
	v = url.Values{"request": {"jsupdate"}}
	if body, _ := c.formBody(v); strings.Contains(string(body), "o1a2") {
		t.Fatalf("password sent outside login: %q", body)
	}
}
