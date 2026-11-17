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
		created, err := createKeyFile(path, key)
		if err != nil {
			return nil, err
		}
		if created {
			return key, nil
		}
		// Another process created it first: loop and read its key.
	}
	return nil, fmt.Errorf("could not create %s", path)
}

// createKeyFile publishes key at path only if path does not exist yet, and
// never lets another process see a partly written file: the key is written to
// a temporary file first, then hard-linked into place (atomic, and it fails
// with "exists" when another process got there first).
func createKeyFile(path string, key []byte) (created bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "master.key.tmp-*") // mode 0600
	if err != nil {
		return false, fmt.Errorf("create temp key file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, fmt.Errorf("write %s: %w", tmp.Name(), werr)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("create %s: %w", path, err)
	}
	return true, nil
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
