package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	if !Supported {
		t.Skip("saved passwords are not supported on this platform")
	}
	path := filepath.Join(t.TempDir(), "gcdl", "credentials.json")
	f, err := Seal("someone", []byte("portal-secret"), []byte("my passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	g, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := g.Open([]byte("my passphrase")); err != nil || string(pw) != "portal-secret" || g.User != "someone" {
		t.Fatalf("open: %q %v", pw, err)
	}
	if _, err := g.Open([]byte("wrong")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	g.User = "someone-else" // the username is authenticated
	if _, err := g.Open([]byte("my passphrase")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("tampered user: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPermissions(t *testing.T) {
	if !Supported {
		t.Skip("saved passwords are not supported on this platform")
	}
	dir := filepath.Join(t.TempDir(), "gcdl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	f, _ := Seal("someone", []byte("pw"), []byte("passphrase"))
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %04o, want 0700", st.Mode().Perm())
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); !errors.Is(err, ErrInsecure) {
			t.Errorf("mode %04o: got %v, want ErrInsecure", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		os.Chmod(path, mode)
		if _, err := Load(path); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
}

func TestShortPassphrase(t *testing.T) {
	// Characters are counted, not bytes: 7 × "é" is 14 bytes but too short.
	for _, p := range []string{"", "1234567", "ééééééé"} {
		if _, err := Seal("someone", []byte("pw"), []byte(p)); !errors.Is(err, ErrShortPassphrase) {
			t.Errorf("Seal with %q: err = %v", p, err)
		}
	}
	if _, err := Seal("someone", []byte("pw"), []byte("12345678")); err != nil {
		t.Errorf("Seal with 8 characters: %v", err)
	}
}
