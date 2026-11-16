package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeyLen) }

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := key(1)
	ct, nonce, err := Encrypt(k, "app-1", "API_KEY", []byte("s3cret value"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("s3cret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := Decrypt(k, "app-1", "API_KEY", ct, nonce)
	if err != nil || string(got) != "s3cret value" {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
	// Empty values work too.
	ct, nonce, _ = Encrypt(k, "app-1", "EMPTY", nil)
	if got, err := Decrypt(k, "app-1", "EMPTY", ct, nonce); err != nil || len(got) != 0 {
		t.Fatalf("empty value: %q, %v", got, err)
	}
}

func TestEncryptUsesAFreshNonceEachTime(t *testing.T) {
	k := key(1)
	seen := map[string]bool{}
	for range 100 {
		_, nonce, err := Encrypt(k, "a", "K", []byte("same"))
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(nonce)] {
			t.Fatal("a nonce was reused")
		}
		seen[string(nonce)] = true
	}
}

func TestDecryptRejectsTamperingAndWrongContext(t *testing.T) {
	k := key(1)
	ct, nonce, _ := Encrypt(k, "app-1", "NAME", []byte("value"))

	flipped := append([]byte(nil), ct...)
	flipped[0] ^= 0xff
	badNonce := append([]byte(nil), nonce...)
	badNonce[0] ^= 0xff

	tests := map[string]func() error{
		"tampered ciphertext": func() error { _, err := Decrypt(k, "app-1", "NAME", flipped, nonce); return err },
		"truncated":           func() error { _, err := Decrypt(k, "app-1", "NAME", ct[:len(ct)-1], nonce); return err },
		"tampered nonce":      func() error { _, err := Decrypt(k, "app-1", "NAME", ct, badNonce); return err },
		"short nonce":         func() error { _, err := Decrypt(k, "app-1", "NAME", ct, nonce[:4]); return err },
		"wrong key":           func() error { _, err := Decrypt(key(2), "app-1", "NAME", ct, nonce); return err },
		"another app":         func() error { _, err := Decrypt(k, "app-2", "NAME", ct, nonce); return err },
		"another name":        func() error { _, err := Decrypt(k, "app-1", "OTHER", ct, nonce); return err },
		"bad key length":      func() error { _, err := Decrypt([]byte("short"), "app-1", "NAME", ct, nonce); return err },
	}
	for name, f := range tests {
		if f() == nil {
			t.Errorf("%s: decryption succeeded", name)
		}
	}
	if _, _, err := Encrypt([]byte("short"), "a", "n", []byte("v")); err == nil {
		t.Error("Encrypt accepted a short key")
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadMasterKeyFromEnvironment(t *testing.T) {
	want := key(7)
	got, err := LoadMasterKey(env(map[string]string{"SHIPYARD_MASTER_KEY": base64.StdEncoding.EncodeToString(want)}), t.TempDir())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"not base64!!", base64.StdEncoding.EncodeToString([]byte("too short"))} {
		if _, err := LoadMasterKey(env(map[string]string{"SHIPYARD_MASTER_KEY": bad}), t.TempDir()); err == nil {
			t.Errorf("accepted bad key %q", bad)
		}
	}
}

func TestLoadMasterKeyCreatesAndReusesAFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := LoadMasterKey(env(nil), dir)
	if err != nil || len(first) != KeyLen {
		t.Fatalf("first load: %v, %v", first, err)
	}
	second, err := LoadMasterKey(env(nil), dir)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("second load differs: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "master.key")); err != nil || info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("master.key mode = %v, %v (want owner-only)", info.Mode(), err)
	}
	// A damaged file is an error, never silently replaced (that would orphan every secret).
	if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(env(nil), dir); err == nil || !strings.Contains(err.Error(), "master.key") {
		t.Fatalf("damaged key file: %v", err)
	}
}

// Several processes starting together must all end up with the same key.
func TestLoadMasterKeyConcurrentCreatorsAgree(t *testing.T) {
	dir := t.TempDir()
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadMasterKey(env(nil), dir)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	for i := range keys {
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatalf("process %d got a different key", i)
		}
	}
}

func TestDeriveIsDeterministicAndPurposeBound(t *testing.T) {
	m := key(9)
	a, b := Derive(m, "sy_app cookie"), Derive(m, "sy_app cookie")
	if !bytes.Equal(a, b) || len(a) != 32 {
		t.Fatal("Derive is not deterministic")
	}
	if bytes.Equal(a, Derive(m, "something else")) || bytes.Equal(a, m) || bytes.Equal(a, Derive(key(8), "sy_app cookie")) {
		t.Fatal("derived keys must differ by purpose, master and from the master itself")
	}
}
