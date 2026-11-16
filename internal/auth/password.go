package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP's second recommended profile: 19 MiB, 2 passes).
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// Password length limits: the maximum bounds hashing work per request.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 128
)

// HashPassword returns a PHC-format argon2id hash of password with a random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches an encoded hash from HashPassword.
// The comparison is constant-time. A malformed hash is an error, not a mismatch.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var mem, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &timeCost, &threads); err != nil {
		return false, errors.New("malformed argon2 parameters")
	}
	// Our own hashes only; refuse parameters that would make verification a DoS.
	if mem == 0 || mem > 256*1024 || timeCost == 0 || timeCost > 10 || threads == 0 || threads > 8 {
		return false, errors.New("argon2 parameters out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("decode hash")
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, mem, threads, uint32(len(want))) //nolint:gosec // len(want) is bounded by the stored hash
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a login names an unknown email, so the
// response time does not reveal whether the account exists.
var dummyHash = func() string {
	h, err := HashPassword("not-a-real-password")
	if err != nil {
		return ""
	}
	return h
}()

// BurnPasswordCheck spends the same time as a real verification.
func BurnPasswordCheck(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}
