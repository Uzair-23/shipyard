package router

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/access"
	"github.com/Uzair-23/shipyard/internal/auth"
	"github.com/Uzair-23/shipyard/internal/store"
)

// cookieTTL is how long an _sy_app cookie is valid. After that the browser
// goes through the handoff again, which re-checks access.
const cookieTTL = 12 * time.Hour

// Backend is what the gate needs from the database (a fake in unit tests).
type Backend interface {
	// ConsumeHandoff exchanges a one-time code that was issued for appID.
	// ok is false for an unknown, used, expired or other-app code.
	ConsumeHandoff(ctx context.Context, appID, code string) (userID, email, returnPath string, ok bool, err error)
	// CanView reports whether the user may view the app (cached by the real backend).
	CanView(ctx context.Context, appID, userID, email string) (bool, error)
}

// Identity is the user an _sy_app cookie vouches for.
type Identity struct{ UserID, Email string }

type claims struct {
	App   string `json:"a"`
	User  string `json:"u"`
	Email string `json:"e"`
	Exp   int64  `json:"x"` // unix seconds
}

// signClaims returns "<payload>.<hmac>": the cookie value. It is bound to one
// app, so a cookie from app A is useless on app B.
func signClaims(key []byte, c claims) string {
	payload, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(mac(key, p))
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// verifyClaims checks the signature (constant time) and expiry.
func verifyClaims(key []byte, value string, now time.Time) (claims, bool) {
	var c claims
	if len(value) > 1024 {
		return c, false
	}
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || strings.Contains(sig, ".") {
		return c, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, payload)) {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return claims{}, false
	}
	if c.App == "" || c.User == "" || now.Unix() >= c.Exp {
		return claims{}, false
	}
	return c, true
}

type gate struct {
	backend    Backend
	key        []byte // HMAC key for _sy_app
	baseDomain string // the dashboard host, e.g. shipyard.localhost
}

func scheme(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

// identity returns the user vouched for by a valid _sy_app cookie for this app.
// Every cookie with that name is tried: a sibling app can set a bad _sy_app on
// the parent domain, and that must not lock the user out.
func (g *gate) identity(r *http.Request, appID string) *Identity {
	for _, c := range r.Cookies() {
		if c.Name != appCookie {
			continue
		}
		if cl, ok := verifyClaims(g.key, c.Value, time.Now()); ok && cl.App == appID {
			return &Identity{UserID: cl.User, Email: cl.Email}
		}
	}
	return nil
}

// authorize decides whether the request may reach a private app. It writes the
// response itself (redirect to the handoff, or an error page) and returns false
// when the request must not be proxied.
func (g *gate) authorize(w http.ResponseWriter, r *http.Request, route Route) (*Identity, bool) {
	id := g.identity(r, route.AppID)
	if route.Visibility == "public" {
		return id, true // identity is optional; passed on if present
	}
	if route.Visibility != "private" { // unknown value: fail closed
		writePage(w, http.StatusServiceUnavailable, "Access check unavailable", "This app's access settings could not be read. Try again in a moment.")
		return nil, false
	}
	if id == nil {
		g.redirectToHandoff(w, r, route)
		return nil, false
	}
	ok, err := g.backend.CanView(r.Context(), route.AppID, id.UserID, id.Email)
	if err != nil {
		slog.Error("access check failed", "app_id", route.AppID, "error", err)
		writePage(w, http.StatusServiceUnavailable, "Access check unavailable", "Could not check access to this app. Try again in a moment.")
		return nil, false
	}
	if !ok { // access was removed: the handoff shows the "No access" page
		g.redirectToHandoff(w, r, route)
		return nil, false
	}
	return id, true
}

func (g *gate) handoffURL(r *http.Request, route Route, returnPath string) string {
	q := url.Values{"app": {route.Slug}}
	if returnPath != "" {
		q.Set("return", returnPath)
	}
	return scheme(r) + "://" + g.baseDomain + "/auth/app?" + q.Encode()
}

func (g *gate) redirectToHandoff(w http.ResponseWriter, r *http.Request, route Route) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, g.handoffURL(r, route, auth.SafeReturnPath(r.URL.RequestURI())), http.StatusFound)
}

// callback is /__shipyard/callback: it trades the one-time code for the
// _sy_app cookie and sends the browser on to the path it originally wanted.
func (g *gate) callback(w http.ResponseWriter, r *http.Request, route Route) {
	w.Header().Set("Cache-Control", "no-store")
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 200 {
		writePage(w, http.StatusBadRequest, "Invalid sign-in link", "This sign-in link is incomplete.")
		return
	}
	userID, email, returnPath, ok, err := g.backend.ConsumeHandoff(r.Context(), route.AppID, code)
	if err != nil {
		slog.Error("consume handoff code failed", "app_id", route.AppID, "error", err)
		writePage(w, http.StatusServiceUnavailable, "Sign-in unavailable", "Could not complete sign-in. Try again in a moment.")
		return
	}
	if !ok {
		writePage(w, http.StatusForbidden, "Sign-in link expired",
			fmt.Sprintf("This sign-in link has expired or was already used. Open %s://%s/auth/app?app=%s to try again.", scheme(r), g.baseDomain, route.Slug))
		return
	}
	exp := time.Now().Add(cookieTTL)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure whenever the request is HTTPS; local dev is plain HTTP (ADR 014)
		Name: appCookie, Value: signClaims(g.key, claims{App: route.AppID, User: userID, Email: email, Exp: exp.Unix()}),
		Path: "/", Expires: exp, MaxAge: int(cookieTTL.Seconds()), HttpOnly: true, Secure: scheme(r) == "https", SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, auth.SafeReturnPath(returnPath), http.StatusFound) //nolint:gosec // SafeReturnPath only lets a plain same-host path through
}

// dbBackend is the Backend over Postgres.
type dbBackend struct {
	pool  *pgxpool.Pool
	cache *access.Cache
}

func newDBBackend(pool *pgxpool.Pool, ttl time.Duration) *dbBackend {
	return &dbBackend{pool: pool, cache: &access.Cache{DB: pool, TTL: ttl}}
}

func (b *dbBackend) ConsumeHandoff(ctx context.Context, appID, code string) (string, string, string, bool, error) {
	var app pgtype.UUID
	if err := app.Scan(appID); err != nil {
		return "", "", "", false, fmt.Errorf("app id: %w", err)
	}
	h := sha256.Sum256([]byte(code))
	row, err := store.New(b.pool).ConsumeHandoffCode(ctx, store.ConsumeHandoffCodeParams{CodeHash: h[:], AppID: app})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("consume handoff code: %w", err)
	}
	return row.UserID.String(), row.Email, row.ReturnPath, true, nil
}

func (b *dbBackend) CanView(ctx context.Context, appID, userID, email string) (bool, error) {
	var app, user pgtype.UUID
	if err := app.Scan(appID); err != nil {
		return false, fmt.Errorf("app id: %w", err)
	}
	if err := user.Scan(userID); err != nil {
		return false, fmt.Errorf("user id: %w", err)
	}
	return b.cache.CanView(ctx, app, user, email)
}
