package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// KeyLen is the length of the master key in bytes (AES-256).
const KeyLen = 32

// LoadMasterKey returns the master key: SHIPYARD_MASTER_KEY (base64 of 32
// bytes) if set, otherwise the key stored in <dataDir>/master.key, which is
// created with a random key on first use. The data dir is shared by every
// Shipyard process, so local development needs no setup; set the environment
// variable for anything that other people use.
func LoadMasterKey(getenv func(string) string, dataDir string) ([]byte, error) {
	if v := strings.TrimSpace(getenv("SHIPYARD_MASTER_KEY")); v != "" {
		return decodeKey(v, "SHIPYARD_MASTER_KEY")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "master.key")
	for range 3 { // another process may create the file between our attempts
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // fixed file name inside the configured data dir
			return decodeKey(strings.TrimSpace(string(data)), path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		key := make([]byte, KeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate master key: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // see above
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path) // never leave a half-written key behind
			return nil, fmt.Errorf("write %s: %w", path, werr)
		}
		return key, nil
	}
	return nil, fmt.Errorf("could not create %s", path)
}

func decodeKey(s, where string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != KeyLen {
		return nil, fmt.Errorf("%s must be base64 of exactly %d bytes (generate one with: openssl rand -base64 32)", where, KeyLen)
	}
	return key, nil
}

// Derive returns a 32-byte subkey for one purpose (HKDF-SHA256), so the
// master key itself is only used to encrypt data and every other secret
// (such as the _sy_app cookie signing key) is independent of it.
func Derive(master []byte, purpose string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte("shipyard/"+purpose)), out); err != nil {
		return nil // HKDF-SHA256 can produce 32 bytes; unreachable
	}
	return out
}
