package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// aad binds a ciphertext to one app and variable name, so a value copied to
// another row (another app, another name) fails to decrypt.
func aad(appID, name string) []byte { return []byte(appID + "|" + name) }

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Encrypt seals plaintext with AES-256-GCM and a fresh random nonce.
func Encrypt(key []byte, appID, name string, plaintext []byte) (ciphertext, nonce []byte, err error) {
	g, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return g.Seal(nil, nonce, plaintext, aad(appID, name)), nonce, nil
}

// Decrypt opens a value sealed by Encrypt for the same app and name.
func Decrypt(key []byte, appID, name string, ciphertext, nonce []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, errors.New("bad nonce")
	}
	plain, err := g.Open(nil, nonce, ciphertext, aad(appID, name))
	if err != nil {
		return nil, errors.New("decrypt: wrong key, wrong app or name, or tampered data")
	}
	return plain, nil
}
