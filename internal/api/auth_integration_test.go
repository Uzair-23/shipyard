//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func (e env) call(t *testing.T, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "shipyard_session" {
			return c
		}
	}
	return nil
}

func TestSignupLoginLogout(t *testing.T) {
	e := newEnv(t)
	email := fmt.Sprintf("flow%d@example.com", userSeq.Add(1))
	creds := map[string]string{"email": email, "password": "correct-horse-battery"}

	rec := e.call(t, nil, http.MethodPost, "/api/v1/auth/signup", creds)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" {
		t.Fatalf("session cookie = %+v", c)
	}
	if strings.Contains(rec.Body.String(), "correct-horse") || strings.Contains(rec.Body.String(), "argon2") {
		t.Fatalf("response leaks credentials: %s", rec.Body)
	}

	// Only a hash of the token is stored.
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE token_hash = convert_to($1, 'UTF8')`, c.Value).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw token found in the database (n=%d, err=%v)", n, err)
	}
	var stored string
	if err := testPool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE email = $1`, email).Scan(&stored); err != nil || !strings.HasPrefix(stored, "$argon2id$") {
		t.Fatalf("password not stored as argon2id: %q %v", stored, err)
	}

	if rec := e.call(t, c, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), email) {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}

	// Log in (case-insensitive email) gives a second, independent session.
	login := e.call(t, nil, http.MethodPost, "/api/v1/auth/login", map[string]string{"email": strings.ToUpper(email), "password": "correct-horse-battery"})
	c2 := sessionCookie(login)
	if login.Code != http.StatusOK || c2 == nil || c2.Value == c.Value {
		t.Fatalf("login: %d %+v", login.Code, c2)
	}

	// Logout ends only that session.
	out := e.call(t, c, http.MethodPost, "/api/v1/auth/logout", nil)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", out.Code)
	}
	if rec := e.call(t, c, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", rec.Code)
	}
	if rec := e.call(t, c2, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusOK {
		t.Errorf("other session after logout = %d, want 200", rec.Code)
	}
}

func TestSignupAndLoginRejections(t *testing.T) {
	e := newEnv(t)
	email := fmt.Sprintf("dup%d@example.com", userSeq.Add(1))
	good := map[string]string{"email": email, "password": "correct-horse-battery"}
	if rec := e.call(t, nil, http.MethodPost, "/api/v1/auth/signup", good); rec.Code != http.StatusCreated {
		t.Fatalf("signup: %d", rec.Code)
	}
	if rec := e.call(t, nil, http.MethodPost, "/api/v1/auth/signup", map[string]string{"email": strings.ToUpper(email), "password": "another-password"}); rec.Code != http.StatusConflict || errCode(t, rec) != "email_taken" {
		t.Errorf("duplicate email (other case): %d %s", rec.Code, rec.Body)
	}
	bad := []map[string]string{
		{"email": "", "password": "correct-horse-battery"},
		{"email": "not-an-email", "password": "correct-horse-battery"},
		{"email": "Name <a@example.com>", "password": "correct-horse-battery"},
		{"email": "a@example.com", "password": "short"},
		{"email": "b@example.com", "password": strings.Repeat("x", 129)},
	}
	for _, b := range bad {
		if rec := e.call(t, nil, http.MethodPost, "/api/v1/auth/signup", b); rec.Code != http.StatusBadRequest {
			t.Errorf("signup %v: %d %s", b, rec.Code, rec.Body)
		}
	}

	var wrongPw, unknown string
	rec := e.call(t, nil, http.MethodPost, "/api/v1/auth/login", map[string]string{"email": email, "password": "wrong-password-1"})
	if rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil {
		t.Errorf("wrong password: %d, cookie %v", rec.Code, sessionCookie(rec))
	}
	wrongPw = rec.Body.String()
	rec = e.call(t, nil, http.MethodPost, "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "wrong-password-1"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown email: %d", rec.Code)
	}
	unknown = rec.Body.String()
	if wrongPw != unknown {
		t.Errorf("login failures differ, which reveals which emails exist:\n%s\n%s", wrongPw, unknown)
	}

	// No cookie, a bad cookie, and an expired session are all 401.
	if rec := e.call(t, nil, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", rec.Code)
	}
	if rec := e.call(t, &http.Cookie{Name: "shipyard_session", Value: "garbage"}, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad cookie: %d", rec.Code)
	}
	c := sessionCookie(e.call(t, nil, http.MethodPost, "/api/v1/auth/login", good))
	if _, err := testPool.Exec(context.Background(), `UPDATE sessions SET expires_at = $1 WHERE user_id = (SELECT id FROM users WHERE email = $2)`, time.Now().Add(-time.Minute), email); err != nil {
		t.Fatal(err)
	}
	if rec := e.call(t, c, http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session: %d, want 401", rec.Code)
	}
}

// Every app route answers 404 (never 403) for someone else's app, and 401 without a session.
func TestOwnershipIsolation(t *testing.T) {
	a := newEnv(t)
	owner := a.cookie
	other := signup(t, a.handler, fmt.Sprintf("other%d@example.com", userSeq.Add(1)))

	if rec := a.call(t, owner, http.MethodPost, "/api/v1/apps", map[string]string{"slug": "owned-app"}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var appID string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM apps WHERE slug = 'owned-app'`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	var ownerID string
	if err := testPool.QueryRow(context.Background(), `SELECT owner_id::text FROM apps WHERE slug = 'owned-app'`).Scan(&ownerID); err != nil || ownerID == "" {
		t.Fatalf("app has no owner: %q %v", ownerID, err)
	}
	if rec := a.deploy(t, "owned-app", "source", goodArchive(t, "")); rec.Code != http.StatusAccepted {
		t.Fatalf("owner deploy: %d %s", rec.Code, rec.Body)
	}
	if rec := a.call(t, owner, http.MethodGet, "/api/v1/apps/owned-app", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"slug":"owned-app"`) || !strings.Contains(rec.Body.String(), `"url":"http://owned-app.apps.localhost"`) ||
		!strings.Contains(rec.Body.String(), `"latest_deploy":{"seq":1,"state":"queued"`) {
		t.Fatalf("owner get: %d %s", rec.Code, rec.Body)
	}

	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/apps/owned-app", nil},
		{http.MethodPatch, "/api/v1/apps/owned-app", map[string]int{"idle_timeout_s": 600}},
	}
	for _, r := range routes {
		rec := a.call(t, other, r.method, r.path, r.body)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Errorf("other user %s %s: %d %s, want 404", r.method, r.path, rec.Code, rec.Body)
		}
		rec = a.call(t, nil, r.method, r.path, r.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no session %s %s: %d, want 401", r.method, r.path, rec.Code)
		}
	}

	// The other user cannot deploy to it, and the list only shows their own apps.
	saved := a.cookie
	a.cookie = other
	if rec := a.deploy(t, "owned-app", "source", goodArchive(t, "other")); rec.Code != http.StatusNotFound {
		t.Errorf("other user deploy: %d, want 404", rec.Code)
	}
	if rec := a.createApp(t, "owned-app"); rec.Code != http.StatusConflict {
		t.Errorf("taking an existing slug: %d, want 409", rec.Code)
	}
	a.cookie = saved
	if rec := a.call(t, other, http.MethodGet, "/api/v1/apps", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "owned-app") || !strings.Contains(rec.Body.String(), `"apps":[]`) {
		t.Errorf("other user's list: %d %s", rec.Code, rec.Body)
	}
	if rec := a.call(t, owner, http.MethodGet, "/api/v1/apps", nil); !strings.Contains(rec.Body.String(), "owned-app") {
		t.Errorf("owner's list: %s", rec.Body)
	}
	// The failed attempts must not have created a deploy for the other user.
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM deploys WHERE app_id = $1`, appID).Scan(&n); err != nil || n != 1 {
		t.Errorf("deploys of the app = %d (%v), want 1", n, err)
	}
}

func TestPatchIdleTimeout(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "patch-app")
	rec := e.call(t, e.cookie, http.MethodPatch, "/api/v1/apps/patch-app", map[string]int{"idle_timeout_s": 1800})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"idle_timeout_s":1800`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []any{map[string]int{"idle_timeout_s": 299}, map[string]int{"idle_timeout_s": 3601}, map[string]string{}, map[string]string{"unknown_field": "x"}} {
		if rec := e.call(t, e.cookie, http.MethodPatch, "/api/v1/apps/patch-app", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %v: %d, want 400", bad, rec.Code)
		}
	}
}

// The app JSON carries how long the last wake took (set by the router), null before any.
func TestAppShowsLastColdStart(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "cold-app")
	if rec := e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/cold-app", nil); !strings.Contains(rec.Body.String(), `"last_cold_start_ms":null`) {
		t.Fatalf("before any wake: %s", rec.Body)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE apps SET last_cold_start_ms = 1834 WHERE slug = 'cold-app'`); err != nil {
		t.Fatal(err)
	}
	if rec := e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/cold-app", nil); !strings.Contains(rec.Body.String(), `"last_cold_start_ms":1834`) {
		t.Fatalf("after a wake: %s", rec.Body)
	}
}

// Mutating auth routes get the same Origin check as every other route.
func TestAuthRoutesRefuseCrossSiteOrigin(t *testing.T) {
	e := newEnv(t)
	body, _ := json.Marshal(map[string]string{"email": "csrf@example.com", "password": "correct-horse-battery"})
	for _, path := range []string{"/api/v1/auth/signup", "/api/v1/auth/login", "/api/v1/auth/logout"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a foreign Origin: %d, want 403", path, rec.Code)
		}
	}
}
