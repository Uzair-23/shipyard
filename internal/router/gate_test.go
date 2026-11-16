package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

// fakeBackend implements Backend in memory.
type fakeBackend struct {
	codes   map[string]fakeCode // code -> issue
	allowed map[string]bool     // appID|userID -> may view
	err     error
}

type fakeCode struct {
	appID, userID, email, returnPath string
	used                             bool
}

func (f *fakeBackend) ConsumeHandoff(_ context.Context, appID, code string) (string, string, string, bool, error) {
	if f.err != nil {
		return "", "", "", false, f.err
	}
	c, ok := f.codes[code]
	if !ok || c.used || c.appID != appID {
		return "", "", "", false, nil
	}
	c.used = true
	f.codes[code] = c
	return c.userID, c.email, c.returnPath, true, nil
}

func (f *fakeBackend) CanView(_ context.Context, appID, userID, _ string) (bool, error) {
	return f.allowed[appID+"|"+userID], f.err
}

func gatedHandler(be *fakeBackend, routes ...Route) (*handler, *Table) {
	tb := NewTable()
	tb.Replace(routes)
	g := &gate{backend: be, key: testKey, baseDomain: "shipyard.localhost"}
	return newHandler(tb, "apps.localhost", newActivity(), g), tb
}

func cookieFor(app, user, email string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{Name: "_sy_app", Value: signClaims(testKey, claims{App: app, User: user, Email: email, Exp: time.Now().Add(ttl).Unix()})}
}

func get(h http.Handler, host, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestClaimsRoundTripAndTamper(t *testing.T) {
	now := time.Now()
	good := signClaims(testKey, claims{App: "a1", User: "u1", Email: "e@x.com", Exp: now.Add(time.Hour).Unix()})
	if c, ok := verifyClaims(testKey, good, now); !ok || c.App != "a1" || c.User != "u1" || c.Email != "e@x.com" {
		t.Fatalf("round trip failed: %+v %v", c, ok)
	}
	payload, sig, _ := strings.Cut(good, ".")
	other := signClaims(testKey, claims{App: "a2", User: "u1", Email: "e@x.com", Exp: now.Add(time.Hour).Unix()})
	otherPayload, _, _ := strings.Cut(other, ".")
	bad := map[string]string{
		"empty":                   "",
		"no dot":                  payload,
		"extra part":              good + ".x",
		"payload swapped":         otherPayload + "." + sig,
		"signature flipped":       payload + "." + flip(sig),
		"payload flipped":         flip(payload) + "." + sig,
		"signed with another key": signClaims([]byte("another-key-another-key-another-k"), claims{App: "a1", User: "u1", Exp: now.Add(time.Hour).Unix()}),
		"expired":                 signClaims(testKey, claims{App: "a1", User: "u1", Exp: now.Add(-time.Second).Unix()}),
		"missing app":             signClaims(testKey, claims{User: "u1", Exp: now.Add(time.Hour).Unix()}),
		"huge":                    strings.Repeat("a", 2000),
		"not base64":              "!!!.!!!",
	}
	for name, v := range bad {
		if _, ok := verifyClaims(testKey, v, now); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestPrivateAppRedirectsToHandoffWithoutCookie(t *testing.T) {
	port, seen := upstream(t)
	h, _ := gatedHandler(&fakeBackend{}, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})

	rec := get(h, "secret.apps.localhost", "/dashboard?tab=1")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Scheme != "http" || loc.Host != "shipyard.localhost" || loc.Path != "/auth/app" ||
		loc.Query().Get("app") != "secret" || loc.Query().Get("return") != "/dashboard?tab=1" {
		t.Fatalf("redirect = %s", rec.Header().Get("Location"))
	}
	select {
	case <-seen:
		t.Fatal("the request reached the app without authentication")
	default:
	}
}

func TestPrivateAppServesMemberAndSetsIdentityHeaders(t *testing.T) {
	port, seen := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{"app-a|user-1": true}}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})

	hdr := cookieFor("app-a", "user-1", "member@example.com", time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "secret.apps.localhost"
	req.AddCookie(hdr)
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	// A client trying to impersonate someone else.
	req.Header.Set("X-Shipyard-User-Email", "admin@evil.example")
	req.Header.Set("X-Shipyard-User-Id", "someone-else")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := <-seen
	if got.Header.Get("X-Shipyard-User-Email") != "member@example.com" || got.Header.Get("X-Shipyard-User-Id") != "user-1" {
		t.Fatalf("identity headers = %q / %q, want the cookie's identity", got.Header.Get("X-Shipyard-User-Email"), got.Header.Get("X-Shipyard-User-Id"))
	}
	if _, err := got.Cookie("_sy_app"); err == nil {
		t.Error("the _sy_app cookie reached the app")
	}
	if c, err := got.Cookie("theme"); err != nil || c.Value != "dark" {
		t.Errorf("other cookies must pass through: %v %v", c, err)
	}
}

func TestSpoofedIdentityHeadersAreRemovedWithoutACookie(t *testing.T) {
	port, seen := upstream(t)
	h, _ := gatedHandler(&fakeBackend{}, Route{AppID: "app-a", Slug: "open", Visibility: "public", Port: port})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "open.apps.localhost"
	req.Header.Set("X-Shipyard-User-Email", "admin@evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := (<-seen).Header.Get("X-Shipyard-User-Email"); got != "" {
		t.Fatalf("anonymous visitor reached the app with X-Shipyard-User-Email = %q", got)
	}
}

// A cookie issued for app A must not open app B, even for the same user.
func TestCookieForAnotherAppIsRejected(t *testing.T) {
	portA, _ := upstream(t)
	portB, seenB := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{"app-a|user-1": true, "app-b|user-1": true}}
	h, _ := gatedHandler(be,
		Route{AppID: "app-a", Slug: "alpha", Visibility: "private", Port: portA},
		Route{AppID: "app-b", Slug: "bravo", Visibility: "private", Port: portB})

	cookieA := cookieFor("app-a", "user-1", "u@example.com", time.Hour)
	if rec := get(h, "alpha.apps.localhost", "/", cookieA); rec.Code != http.StatusOK {
		t.Fatalf("cookie A on app A: %d", rec.Code)
	}
	rec := get(h, "bravo.apps.localhost", "/", cookieA)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/auth/app") {
		t.Fatalf("cookie A on app B: %d %s, want a redirect to the handoff", rec.Code, rec.Header().Get("Location"))
	}
	select {
	case <-seenB:
		t.Fatal("app B was reached with app A's cookie")
	default:
	}
}

func TestTamperedAndExpiredCookiesAreRejected(t *testing.T) {
	port, seen := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{"app-a|user-1": true}}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})

	good := cookieFor("app-a", "user-1", "u@example.com", time.Hour)
	tampered := &http.Cookie{Name: "_sy_app", Value: flip(good.Value)}
	expired := cookieFor("app-a", "user-1", "u@example.com", -time.Minute)
	forged := &http.Cookie{Name: "_sy_app", Value: signClaimsWith([]byte("not-the-real-key-not-the-real-ke"), "app-a", "user-1")}
	for name, c := range map[string]*http.Cookie{"tampered": tampered, "expired": expired, "forged": forged} {
		if rec := get(h, "secret.apps.localhost", "/", c); rec.Code != http.StatusFound {
			t.Errorf("%s cookie: status %d, want 302", name, rec.Code)
		}
	}
	select {
	case <-seen:
		t.Fatal("a bad cookie reached the app")
	default:
	}
}

func signClaimsWith(key []byte, app, user string) string {
	return signClaims(key, claims{App: app, User: user, Email: "x@example.com", Exp: time.Now().Add(time.Hour).Unix()})
}

// Access removed from a member: the cookie is still valid but CanView says no.
func TestRevokedMemberIsSentBackToTheHandoff(t *testing.T) {
	port, _ := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{}}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})
	rec := get(h, "secret.apps.localhost", "/", cookieFor("app-a", "user-1", "u@example.com", time.Hour))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
}

// A sibling app can plant a bad _sy_app on the shared parent domain; it must
// not lock the user out of an app they hold a good cookie for.
func TestInvalidCookieBeforeValidOneDoesNotLockUserOut(t *testing.T) {
	port, _ := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{"app-a|user-1": true}}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})
	junk := &http.Cookie{Name: "_sy_app", Value: "planted-by-another-app"}
	good := cookieFor("app-a", "user-1", "u@example.com", time.Hour)
	if rec := get(h, "secret.apps.localhost", "/", junk, good); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAccessBackendFailureDoesNotOpenTheApp(t *testing.T) {
	port, seen := upstream(t)
	be := &fakeBackend{allowed: map[string]bool{"app-a|user-1": true}, err: errors.New("database down")}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "secret", Visibility: "private", Port: port})
	rec := get(h, "secret.apps.localhost", "/", cookieFor("app-a", "user-1", "u@example.com", time.Hour))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	select {
	case <-seen:
		t.Fatal("the app was reached although the access check failed")
	default:
	}
}

func TestMissingGateOrUnknownVisibilityFailsClosed(t *testing.T) {
	port, seen := upstream(t)
	// No gate configured: only public apps are served.
	tb := NewTable()
	tb.Replace([]Route{{AppID: "a", Slug: "secret", Visibility: "private", Port: port}, {AppID: "b", Slug: "blank", Visibility: "", Port: port}})
	h := newHandler(tb, "apps.localhost", newActivity(), nil)
	for _, host := range []string{"secret.apps.localhost", "blank.apps.localhost"} {
		if rec := get(h, host, "/"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without a gate: status %d, want 503", host, rec.Code)
		}
	}
	// A gate, but an unknown visibility value.
	hg, _ := gatedHandler(&fakeBackend{}, Route{AppID: "c", Slug: "weird", Visibility: "internal", Port: port})
	if rec := get(hg, "weird.apps.localhost", "/"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unknown visibility: status %d, want 503", rec.Code)
	}
	select {
	case <-seen:
		t.Fatal("an app was reached although access could not be decided")
	default:
	}
}

func TestCallbackIssuesCookieOnceAndOnlyForTheRightApp(t *testing.T) {
	be := &fakeBackend{codes: map[string]fakeCode{
		"good-code":  {appID: "app-a", userID: "user-1", email: "u@example.com", returnPath: "/docs?x=1"},
		"evil-code":  {appID: "app-a", userID: "user-1", email: "u@example.com", returnPath: "//evil.example/steal"},
		"other-code": {appID: "app-b", userID: "user-1", email: "u@example.com", returnPath: "/"},
	}}
	port, _ := upstream(t)
	h, _ := gatedHandler(be,
		Route{AppID: "app-a", Slug: "alpha", Visibility: "private", Port: port},
		Route{AppID: "app-b", Slug: "bravo", Visibility: "private", Port: port})

	rec := get(h, "alpha.apps.localhost", "/__shipyard/callback?code=good-code")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docs?x=1" {
		t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var c *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "_sy_app" {
			c = ck
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" || c.MaxAge != 12*3600 {
		t.Fatalf("cookie attributes = %+v", c)
	}
	if cl, ok := verifyClaims(testKey, c.Value, time.Now()); !ok || cl.App != "app-a" || cl.User != "user-1" || cl.Email != "u@example.com" {
		t.Fatalf("cookie claims = %+v, %v", cl, ok)
	}

	// The same code again: refused, no cookie.
	rec = get(h, "alpha.apps.localhost", "/__shipyard/callback?code=good-code")
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("reused code = %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	// A code issued for app B does not work on app A.
	rec = get(h, "alpha.apps.localhost", "/__shipyard/callback?code=other-code")
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("other app's code = %d", rec.Code)
	}
	// Unknown and missing codes.
	for _, q := range []string{"?code=nope", "", "?code=" + strings.Repeat("a", 500)} {
		if rec := get(h, "alpha.apps.localhost", "/__shipyard/callback"+q); rec.Code == http.StatusFound {
			t.Errorf("callback%s redirected", q)
		}
	}
	// A bad return path is replaced by "/" (no open redirect).
	rec = get(h, "alpha.apps.localhost", "/__shipyard/callback?code=evil-code")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCallbackSetsSecureCookieOverHTTPS(t *testing.T) {
	be := &fakeBackend{codes: map[string]fakeCode{"c": {appID: "app-a", userID: "u", email: "u@example.com", returnPath: "/"}}}
	h, _ := gatedHandler(be, Route{AppID: "app-a", Slug: "alpha", Visibility: "private", Port: 1})
	req := httptest.NewRequest(http.MethodGet, "/__shipyard/callback?code=c", nil)
	req.Host = "alpha.apps.localhost"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cs := rec.Result().Cookies()
	if len(cs) != 1 || !cs[0].Secure {
		t.Fatalf("cookies = %+v, want one Secure cookie", cs)
	}
}

func TestPublicAppPassesIdentityWhenPresent(t *testing.T) {
	port, seen := upstream(t)
	h, _ := gatedHandler(&fakeBackend{}, Route{AppID: "app-a", Slug: "open", Visibility: "public", Port: port})
	if rec := get(h, "open.apps.localhost", "/", cookieFor("app-a", "user-1", "u@example.com", time.Hour)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := (<-seen).Header.Get("X-Shipyard-User-Email"); got != "u@example.com" {
		t.Fatalf("identity on a public app = %q", got)
	}
}
