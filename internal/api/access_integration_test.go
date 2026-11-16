//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVisibilityAndMembers(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "share-app")

	// New apps are private.
	if rec := e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/share-app", nil); !strings.Contains(rec.Body.String(), `"visibility":"private"`) {
		t.Fatalf("new app is not private: %s", rec.Body)
	}
	rec := e.call(t, e.cookie, http.MethodPatch, "/api/v1/apps/share-app", map[string]string{"visibility": "public"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"visibility":"public"`) {
		t.Fatalf("make public: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"internal", "", "PUBLIC"} {
		if rec := e.call(t, e.cookie, http.MethodPatch, "/api/v1/apps/share-app", map[string]string{"visibility": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("visibility %q: %d, want 400", bad, rec.Code)
		}
	}

	// Members: add (idempotent, case-insensitive), list, remove.
	if rec := e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/share-app/members", map[string]string{"email": "Friend@Example.com"}); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "friend@example.com") {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/share-app/members", map[string]string{"email": "friend@example.com"}); rec.Code != http.StatusOK {
		t.Errorf("adding the same member again: %d, want 200", rec.Code)
	}
	for _, bad := range []string{"", "nope", "x@y"} {
		if rec := e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/share-app/members", map[string]string{"email": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("member %q: %d, want 400", bad, rec.Code)
		}
	}
	rec = e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/share-app/members", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"friend@example.com"`) {
		t.Fatalf("list members: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(t, e.cookie, http.MethodDelete, "/api/v1/apps/share-app/members/"+url.PathEscape("friend@example.com"), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(t, e.cookie, http.MethodDelete, "/api/v1/apps/share-app/members/friend@example.com", nil); rec.Code != http.StatusNotFound {
		t.Errorf("removing a non-member: %d, want 404", rec.Code)
	}

	// Only the owner manages sharing; another user sees nothing.
	other := signup(t, e.handler, "other-share@example.com")
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/share-app/members"},
		{http.MethodPost, "/api/v1/apps/share-app/members"},
		{http.MethodDelete, "/api/v1/apps/share-app/members/x@example.com"},
	} {
		if rec := e.call(t, other, r.method, r.path, map[string]string{"email": "z@example.com"}); rec.Code != http.StatusNotFound {
			t.Errorf("other user %s %s: %d, want 404", r.method, r.path, rec.Code)
		}
	}
	if rec := e.call(t, other, http.MethodPatch, "/api/v1/apps/share-app", map[string]string{"visibility": "private"}); rec.Code != http.StatusNotFound {
		t.Errorf("other user changing visibility: %d, want 404", rec.Code)
	}
	// The owner can't be added as a member of their own app.
	var ownerEmail string
	if err := testPool.QueryRow(context.Background(), `SELECT u.email FROM users u JOIN apps a ON a.owner_id = u.id WHERE a.slug = 'share-app'`).Scan(&ownerEmail); err != nil {
		t.Fatal(err)
	}
	if rec := e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/share-app/members", map[string]string{"email": ownerEmail}); rec.Code != http.StatusBadRequest {
		t.Errorf("adding the owner: %d, want 400", rec.Code)
	}
}

func (e env) handoff(t *testing.T, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/app?"+query, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthHandoff(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "hand-app")
	other := signup(t, e.handler, "outsider@example.com")
	friend := signup(t, e.handler, "friend-hand@example.com")
	e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/hand-app/members", map[string]string{"email": "friend-hand@example.com"})

	// Not logged in: to the dashboard login, which comes back here afterwards.
	rec := e.handoff(t, nil, "app=hand-app&return=%2Fdocs")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Path != "/login" || loc.Query().Get("next") != "/auth/app?app=hand-app&return=%2Fdocs" {
		t.Fatalf("anonymous: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Owner: redirected to the app's host with a one-time code, stored only as a hash.
	rec = e.handoff(t, e.cookie, "app=hand-app&return=%2Fdocs%3Fx%3D1")
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Scheme != "http" || loc.Host != "hand-app.apps.localhost" || loc.Path != "/__shipyard/callback" {
		t.Fatalf("owner: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	code := loc.Query().Get("code")
	if len(code) < 40 {
		t.Fatalf("code too short: %q", code)
	}
	var n int
	var returnPath string
	var expires time.Time
	h := sha256.Sum256([]byte(code))
	if err := testPool.QueryRow(context.Background(), `SELECT count(*), max(return_path), max(expires_at) FROM handoff_codes WHERE code_hash = $1 AND used_at IS NULL`, h[:]).Scan(&n, &returnPath, &expires); err != nil || n != 1 {
		t.Fatalf("hashed code not stored (n=%d, err=%v)", n, err)
	}
	if returnPath != "/docs?x=1" || time.Until(expires) > 61*time.Second || time.Until(expires) < 30*time.Second {
		t.Errorf("stored return %q, expires in %s", returnPath, time.Until(expires))
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM handoff_codes WHERE code_hash = convert_to($1, 'UTF8')`, code).Scan(&n); err != nil || n != 0 {
		t.Errorf("raw code stored in the database (n=%d)", n)
	}

	// An open-redirect return path is replaced by "/".
	for _, ret := range []string{"//evil.example/x", "https://evil.example", `/\evil.example`, "evil"} {
		rec = e.handoff(t, e.cookie, "app=hand-app&return="+url.QueryEscape(ret))
		if rec.Code != http.StatusFound {
			t.Fatalf("return %q: %d", ret, rec.Code)
		}
		c := mustCode(t, rec)
		var stored string
		hh := sha256.Sum256([]byte(c))
		if err := testPool.QueryRow(context.Background(), `SELECT return_path FROM handoff_codes WHERE code_hash = $1`, hh[:]).Scan(&stored); err != nil || stored != "/" {
			t.Errorf("return %q stored as %q (%v), want /", ret, stored, err)
		}
	}

	// A member gets in; an outsider and an unknown app get the same "No access" page.
	if rec := e.handoff(t, friend, "app=hand-app"); rec.Code != http.StatusFound {
		t.Errorf("member: %d, want 302", rec.Code)
	}
	for name, r := range map[string]*httptest.ResponseRecorder{
		"outsider":    e.handoff(t, other, "app=hand-app"),
		"unknown app": e.handoff(t, other, "app=does-not-exist"),
	} {
		if r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), "outsider@example.com") || !strings.Contains(r.Body.String(), "No access") {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}

	// Public apps need no membership.
	e.call(t, e.cookie, http.MethodPatch, "/api/v1/apps/hand-app", map[string]string{"visibility": "public"})
	if rec := e.handoff(t, other, "app=hand-app"); rec.Code != http.StatusFound {
		t.Errorf("public app for an outsider: %d, want 302", rec.Code)
	}
}

func mustCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("no code in %q", rec.Header().Get("Location"))
	}
	return loc.Query().Get("code")
}
