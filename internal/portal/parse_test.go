package portal

import (
	"os"
	"testing"
)

func TestParseDatasets(t *testing.T) {
	js, err := os.ReadFile("testdata/mydata.js")
	if err != nil {
		t.Fatal(err)
	}
	ds, ok := parseDatasets(fragmentsOf(string(js)))
	if !ok || len(ds) != 2 {
		t.Fatalf("got ok=%v, %d datasets", ok, len(ds))
	}
	a, e := ds[0], ds[1]
	if !a.Downloadable() || a.signal != "sd944" || a.buttonID != "o3179460c" || a.MD5 != "e8fd00f1966896a78b48992444d4e937" ||
		a.TotalSize != "96GiB" || a.Expires != "2027-01-28 17:42" || a.FileCount != "2923" {
		t.Errorf("available row: %+v", a)
	}
	if e.Downloadable() || !e.Expired || e.Project != "250101_DEMO_PROJECT_A" ||
		e.Name != "1700000002_250101_DEMO_PROJECT_A_250201.NovaSeqX1.FCB_fastq_0001.zip" || e.RequestedBy != "someone@example.org" {
		t.Errorf("expired row: %+v", e)
	}
}

func TestSignedURLAndDialog(t *testing.T) {
	js := `Wt._p_.response(1);{var j=Wt3_7_1.$('o317945f1');j.style.display='';` +
		`Wt3_7_1.setHtml(j,'<input id="o31794569" value="https://storage.googleapis.com/b/sent/files/x_1_of_1.zip?Expires=1&amp;GoogleAccessId=a%40b&amp;Signature=dl4%2Fq%3D%3D" />');` +
		`var o = Wt3_7_1.$('o317945f1');if (o && o.wtPopup) o.wtPopup.shown();}`
	u := findSignedURL(js)
	want := "https://storage.googleapis.com/b/sent/files/x_1_of_1.zip?Expires=1&GoogleAccessId=a%40b&Signature=dl4%2Fq%3D%3D"
	if u != want {
		t.Errorf("url = %q", u)
	}
	if fileNameFromURL(u) != "x_1_of_1.zip" {
		t.Errorf("file name = %q", fileNameFromURL(u))
	}
	if shownDialog(js) != "o317945f1" {
		t.Errorf("dialog = %q", shownDialog(js))
	}
}

func TestFillFileNames(t *testing.T) {
	ds := []Dataset{
		{Name: "1700000003_gc-run-250401-novaseqx1-fcb_gc-run-250401-no...", signal: "s1"},
		{Name: "1700000004_gc-run-250401-novaseqx1-fcb_gc-run-250401-no...", signal: "s2"},
		{Name: "1700000001_gc-run-250301-nextseq2000-fca_gc-run-250301-...", signal: "s3"},
		{Name: "1700000002_x.zip", Expired: true},
	}
	frags := []fragment{
		{target: "o1", html: `<a href="https://storage.googleapis.com/b/sent/files/1700000004_gc-run-250401-novaseqx1-fcb_gc-run-250401-novaseqx1-fcb_part_2_of_2.zip?Expires=1&amp;Signature=x" download>x</a>`},
		{target: "o2", html: `<a href="https://storage.googleapis.com/b/sent/files/1700000003_gc-run-250401-novaseqx1-fcb_gc-run-250401-novaseqx1-fcb_part_1_of_2.zip?Expires=1">x</a>`},
	}
	fillFileNames(ds, dialogFileNames(frags))
	want := []string{
		"1700000003_gc-run-250401-novaseqx1-fcb_gc-run-250401-novaseqx1-fcb_part_1_of_2.zip",
		"1700000004_gc-run-250401-novaseqx1-fcb_gc-run-250401-novaseqx1-fcb_part_2_of_2.zip",
		"", "",
	}
	for i, w := range want {
		if ds[i].FileName != w {
			t.Errorf("row %d: FileName = %q, want %q", i, ds[i].FileName, w)
		}
	}
	if ds[3].DisplayName() != "1700000002_x.zip" {
		t.Errorf("expired display name = %q", ds[3].DisplayName())
	}
}
