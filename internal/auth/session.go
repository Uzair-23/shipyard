package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
)

// SessionTTL is how long a dashboard session lasts.
const SessionTTL = 14 * 24 * time.Hour

const (
	cookieName       = "shipyard_session"
	secureCookieName = "__Host-shipyard_session" // only valid over HTTPS
)

// User is the authenticated user attached to a request.
type User struct {
	ID    pgtype.UUID
	Email string
}

// NewToken returns a random 256-bit session token and its SHA-256 hash.
// Only the hash is stored; the token exists only in the user's cookie.
func NewToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is the stored form of a token.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Sessions creates, resolves and deletes sessions in Postgres.
type Sessions struct{ Pool *pgxpool.Pool }

// Create starts a session for the user and returns its token and expiry.
func (s Sessions) Create(ctx context.Context, userID pgtype.UUID) (string, time.Time, error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(SessionTTL)
	err = store.New(s.Pool).CreateSession(ctx, store.CreateSessionParams{
		TokenHash: hash, UserID: userID, ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return token, expires, nil
}

// Lookup returns the user behind a token, or ok=false for an unknown, expired
// or suspended-user session. Tokens are looked up by their SHA-256 hash.
func (s Sessions) Lookup(ctx context.Context, token string) (User, bool, error) {
	if token == "" || len(token) > 128 {
		return User{}, false, nil
	}
	row, err := store.New(s.Pool).GetSessionUser(ctx, HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("look up session: %w", err)
	}
	return User{ID: row.ID, Email: row.Email}, true, nil
}

// Delete ends a session (logout).
func (s Sessions) Delete(ctx context.Context, token string) error {
	return store.New(s.Pool).DeleteSession(ctx, HashToken(token))
}

// IsSecure reports whether the request reached us over HTTPS (directly or via a proxy).
func IsSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// SetCookie sends the session cookie: HttpOnly, SameSite=Lax, host-only (no
// Domain attribute), and with the __Host- prefix when served over HTTPS.
func SetCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is set whenever the request is HTTPS; local dev runs over plain HTTP (ADR 014)
		Name: sessionCookieName(r), Value: token, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: IsSecure(r), SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see SetCookie
		Name: sessionCookieName(r), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: IsSecure(r), SameSite: http.SameSiteLaxMode,
	})
}

func sessionCookieName(r *http.Request) string {
	if IsSecure(r) {
		return secureCookieName
	}
	return cookieName
}

// TokenFromRequest returns the session token cookie, if any.
func TokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName(r)); err == nil {
		return c.Value
	}
	return ""
}
