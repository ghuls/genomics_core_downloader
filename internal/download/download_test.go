package download

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResume(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 100000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.zip", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	sum := md5.Sum(data)
	want := hex.EncodeToString(sum[:])

	dest := filepath.Join(t.TempDir(), "f.zip")
	if err := os.WriteFile(dest+".part", data[:12345], 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := File(context.Background(), srv.URL, dest, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) || !res.Resumed || !res.MD5Match || res.Size != int64(len(data)) {
		t.Fatalf("res=%+v equal=%v", res, bytes.Equal(got, data))
	}
	res, err = File(context.Background(), srv.URL, dest, want, nil)
	if err != nil || !res.Skipped {
		t.Fatalf("second run: %+v %v", res, err)
	}
}
