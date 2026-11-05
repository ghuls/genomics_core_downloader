// Package credstore keeps the portal password in a file encrypted with a
// passphrase chosen by the user.
//
// The key is derived from the passphrase with Argon2id and the password is
// sealed with XChaCha20-Poly1305 (the username is authenticated as
// additional data). The file is written with mode 0600 in a 0700 directory.
package credstore

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// MinPassphraseLen is the minimum number of characters of a new passphrase.
const MinPassphraseLen = 8

// ErrShortPassphrase is returned by Seal for a passphrase shorter than
// MinPassphraseLen.
var ErrShortPassphrase = fmt.Errorf("the passphrase must be at least %d characters", MinPassphraseLen)

// ErrWrongPassphrase is returned by Open when the passphrase does not decrypt
// the file (or the file was modified).
var ErrWrongPassphrase = errors.New("wrong passphrase for the saved password")

const version = 1

// File is the on-disk format (JSON, []byte fields are base64).
type File struct {
	Version    int    `json:"version"`
	User       string `json:"user"`
	KDF        string `json:"kdf"`
	Time       uint32 `json:"time"`
	MemoryKiB  uint32 `json:"memory_kib"`
	Threads    uint8  `json:"threads"`
	Salt       []byte `json:"salt"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// Path returns $XDG_CONFIG_HOME/gcdl/credentials.json (~/.config/gcdl/… if
// XDG_CONFIG_HOME is not set).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gcdl", "credentials.json"), nil
}

// Seal encrypts password for user with a key derived from passphrase. The
// caller remains responsible for wiping password and passphrase.
func Seal(user string, password, passphrase []byte) (*File, error) {
	if utf8.RuneCount(passphrase) < MinPassphraseLen {
		return nil, ErrShortPassphrase
	}
	f := &File{Version: version, User: user, KDF: "argon2id", Time: 3, MemoryKiB: 64 * 1024, Threads: 4,
		Salt: make([]byte, 16), Nonce: make([]byte, chacha20poly1305.NonceSizeX)}
	if _, err := rand.Read(f.Salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(f.Nonce); err != nil {
		return nil, err
	}
	aead, err := f.aead(passphrase)
	if err != nil {
		return nil, err
	}
	f.Ciphertext = aead.Seal(nil, f.Nonce, password, f.additionalData())
	return f, nil
}

// Open decrypts the password into a new slice, which the caller should wipe
// (portal.Client.Login does).
func (f *File) Open(passphrase []byte) ([]byte, error) {
	aead, err := f.aead(passphrase)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, f.Nonce, f.Ciphertext, f.additionalData())
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	return pt, nil
}

func (f *File) aead(passphrase []byte) (interface {
	Seal(dst, nonce, plaintext, ad []byte) []byte
	Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
}, error) {
	if f.Version != version || f.KDF != "argon2id" {
		return nil, fmt.Errorf("unsupported credentials file (version %d, kdf %q)", f.Version, f.KDF)
	}
	if f.Time == 0 || f.MemoryKiB == 0 || f.Threads == 0 || len(f.Nonce) != chacha20poly1305.NonceSizeX {
		return nil, errors.New("corrupt credentials file")
	}
	key := argon2.IDKey(passphrase, f.Salt, f.Time, f.MemoryKiB, f.Threads, chacha20poly1305.KeySize)
	defer clear(key) // NewX keeps its own copy
	return chacha20poly1305.NewX(key)
}

func (f *File) additionalData() []byte {
	return []byte(fmt.Sprintf("gcdl v%d\x00%s", f.Version, f.User))
}

// ErrUnsupported is returned by Load and Save on platforms where the file
// cannot be kept private (see Supported).
var ErrUnsupported = errors.New("saved passwords are not supported on this platform")

// ErrInsecure is returned by Load when other users can access the file.
var ErrInsecure = errors.New("credentials file is accessible by other users")

// Load reads the file at path. A missing file gives an error matching
// os.ErrNotExist. Like ssh with private keys, a file that is not owned by the
// current user or that group or others can access is refused (ErrInsecure).
func Load(path string) (*File, error) {
	if !Supported {
		return nil, ErrUnsupported
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 && checkPerms {
		return nil, fmt.Errorf("%w: %s has mode %04o, fix it with: chmod 600 %s", ErrInsecure, path, perm, path)
	}
	if err := checkOwner(st); err != nil {
		return nil, fmt.Errorf("%w: %s %v", ErrInsecure, path, err)
	}
	b, err := io.ReadAll(fh)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Save writes f to path (atomically, mode 0600). The directory containing
// it is created or tightened to mode 0700.
func Save(path string, f *File) error {
	if !Supported {
		return ErrUnsupported
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
