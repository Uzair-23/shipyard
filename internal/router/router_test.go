package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAppSlug(t *testing.T) {
	tests := []struct {
		host string
		slug string
		ok   bool
	}{
		{"my-app.apps.localhost", "my-app", true},
		{"my-app.apps.localhost:8081", "my-app", true},
		{"MY-App.Apps.Localhost", "my-app", true},
		{"my-app.apps.localhost.", "my-app", true},
		{"apps.localhost", "", false},
		{"a.b.apps.localhost", "", false},
		{".apps.localhost", "", false},
		{"my-app.apps.localhost.evil.example", "", false},
		{"evilapps.localhost", "", false},
		{"shipyard.localhost", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		slug, ok := AppSlug(tt.host, "apps.localhost")
		if slug != tt.slug || ok != tt.ok {
			t.Errorf("AppSlug(%q) = %q, %v; want %q, %v", tt.host, slug, ok, tt.slug, tt.ok)
		}
	}
}

func TestTable(t *testing.T) {
	tb := NewTable()
	tb.Set(Route{AppID: "1", Slug: "one", Port: 100})
	tb.Set(Route{AppID: "2", Slug: "two", Port: 200})
	if r, ok := tb.Lookup("one"); !ok || r.Port != 100 {
		t.Fatalf("lookup one = %+v, %v", r, ok)
	}
	tb.Set(Route{AppID: "1", Slug: "uno", Port: 101}) // slug changed: old entry must go
	if _, ok := tb.Lookup("one"); ok {
		t.Error("old slug still routes")
	}
	if r, ok := tb.Lookup("uno"); !ok || r.Port != 101 {
		t.Errorf("lookup uno = %+v, %v", r, ok)
	}
	tb.Remove("2")
	if _, ok := tb.Lookup("two"); ok {
		t.Error("removed app still routes")
	}
	tb.Replace([]Route{{AppID: "3", Slug: "three", Port: 300}})
	if _, ok := tb.Lookup("uno"); ok {
		t.Error("Replace kept a stale route")
	}
	if _, ok := tb.Lookup("three"); !ok {
		t.Error("Replace lost a route")
	}
}

// upstream records what the app would see.
func upstream(t *testing.T) (port int, seen chan *http.Request) {
	t.Helper()
	seen = make(chan *http.Request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		_, _ = io.WriteString(w, "hello from app")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	return p, seen
}

// newTestHandler builds a handler without access control; routes default to public.
func newTestHandler(routes ...Route) (*handler, *activity) {
	for i := range routes {
		if routes[i].Visibility == "" {
			routes[i].Visibility = "public"
		}
	}
	tb := NewTable()
	tb.Replace(routes)
	act := newActivity()
	return newHandler(tb, "apps.localhost", act, nil), act
}

func do(h http.Handler, host, path string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProxyForwardsToApp(t *testing.T) {
	port, seen := upstream(t)
	h, act := newTestHandler(Route{AppID: "app-1", Slug: "demo", Status: "running", Port: port})

	rec := do(h, "demo.apps.localhost", "/some/path?q=1", http.Header{"X-Forwarded-For": {"203.0.113.9"}})
	if rec.Code != http.StatusOK || rec.Body.String() != "hello from app" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body)
	}
	got := <-seen
	if got.URL.Path != "/some/path" || got.URL.RawQuery != "q=1" {
		t.Errorf("upstream saw %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Host != "demo.apps.localhost" {
		t.Errorf("upstream Host = %q", got.Host)
	}
	if xff := got.Header.Get("X-Forwarded-For"); !strings.HasPrefix(xff, "203.0.113.9") {
		t.Errorf("X-Forwarded-For = %q, want the client chain kept", xff)
	}
	if got.Header.Get("X-Forwarded-Host") != "demo.apps.localhost" {
		t.Errorf("X-Forwarded-Host = %q", got.Header.Get("X-Forwarded-Host"))
	}
	if id := got.Header.Get("X-Request-Id"); id == "" || id != rec.Header().Get("X-Request-Id") {
		t.Errorf("request id upstream %q, response %q", id, rec.Header().Get("X-Request-Id"))
	}
	if _, ok := act.drain()["app-1"]; !ok {
		t.Error("request was not recorded as activity")
	}
}

// ADR 005 / CLAUDE.md: the router strips _sy_app and every X-Shipyard-* header.
func TestProxyStripsPlatformIdentityInput(t *testing.T) {
	port, seen := upstream(t)
	h, _ := newTestHandler(Route{AppID: "app-1", Slug: "demo", Port: port})

	hdr := http.Header{
		"X-Shipyard-User-Email": {"admin@evil.example"},
		"X-Shipyard-User-Id":    {"00000000-0000-0000-0000-000000000000"},
		"X-Shipyard-Whatever":   {"1"},
		"Cookie":                {"theme=dark; _sy_app=forged.token; other=1"},
	}
	if rec := do(h, "demo.apps.localhost", "/", hdr); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := <-seen
	for k := range got.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-shipyard-") {
			t.Errorf("header %s reached the app", k)
		}
	}
	if _, err := got.Cookie("_sy_app"); err == nil {
		t.Error("_sy_app cookie reached the app")
	}
	if c, err := got.Cookie("theme"); err != nil || c.Value != "dark" {
		t.Errorf("other cookies must pass through: %v %v", c, err)
	}
	if c, err := got.Cookie("other"); err != nil || c.Value != "1" {
		t.Errorf("other cookies must pass through: %v %v", c, err)
	}
}

func TestProxyPages(t *testing.T) {
	h, _ := newTestHandler(
		Route{AppID: "s", Slug: "stopped", Status: "stopped", Port: 0},
		Route{AppID: "d", Slug: "dead", Status: "running", Port: 1}, // nothing listens on port 1
	)
	tests := []struct {
		name, host, path string
		want             int
		contains         string
	}{
		{"unknown app", "nope.apps.localhost", "/", http.StatusNotFound, "No app named nope"},
		{"not an apps host", "shipyard.localhost", "/", http.StatusNotFound, "not served by Shipyard"},
		{"stopped app", "stopped.apps.localhost", "/", http.StatusServiceUnavailable, "not running"},
		{"upstream down", "dead.apps.localhost", "/", http.StatusBadGateway, "not responding"},
		{"reserved path", "stopped.apps.localhost", "/__shipyard/callback", http.StatusNotFound, ""},
		{"reserved path on unknown app", "nope.apps.localhost", "/__shipyard/ready", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, tt.host, tt.path, nil)
			if rec.Code != tt.want || !strings.Contains(rec.Body.String(), tt.contains) {
				t.Fatalf("got %d %q, want %d containing %q", rec.Code, rec.Body, tt.want, tt.contains)
			}
		})
	}
}

func TestPagesEscapeTheSlug(t *testing.T) {
	h, _ := newTestHandler()
	rec := do(h, "<script>alert(1)</script>.apps.localhost", "/", nil)
	if strings.Contains(rec.Body.String(), "<script>alert") {
		t.Fatalf("unescaped slug in page: %s", rec.Body)
	}
}

func TestReservedPathNeverReachesApp(t *testing.T) {
	port, seen := upstream(t)
	h, _ := newTestHandler(Route{AppID: "a", Slug: "demo", Port: port})
	for _, p := range []string{"/__shipyard/callback", "/__shipyard/ready", "/__shipyard/anything"} {
		if rec := do(h, "demo.apps.localhost", p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
	select {
	case r := <-seen:
		t.Fatalf("reserved path was proxied: %s", r.URL.Path)
	default:
	}
}

// An Upgrade (WebSocket) connection must pass through the proxy as a raw tunnel.
func TestProxyPassesUpgradeConnections(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "echo") {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(rw, buf); err == nil {
			_, _ = rw.Write(buf)
			_ = rw.Flush()
		}
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	h, _ := newTestHandler(Route{AppID: "ws", Slug: "ws", Port: port})
	front := httptest.NewServer(h)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: ws.apps.localhost\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("upgrade response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "hello")
	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "hello" {
		t.Fatalf("echo = %q, %v", echo, err)
	}
}
