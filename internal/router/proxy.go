package router

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	appCookie      = "_sy_app"
	reservedPrefix = "/__shipyard/"
	callbackPath   = "/__shipyard/callback"
	readyPath      = "/__shipyard/ready"

	// wsKeepAlive is how often an open WebSocket counts as activity.
	wsKeepAlive = 10 * time.Second
)

// cleanHeaderValue is true for a non-empty value without control characters.
func cleanHeaderValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// handler routes a request by Host to an app container.
type handler struct {
	table      *Table
	appsDomain string
	act        *activity
	gate       *gate  // nil: private apps are refused
	wake       *waker // nil: sleeping apps are not woken
	proxy      *httputil.ReverseProxy
}

func newHandler(table *Table, appsDomain string, act *activity, g *gate) *handler {
	h := &handler{table: table, appsDomain: appsDomain, act: act, gate: g}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: h.rewrite,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   32,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client went away
			}
			slog.Warn("upstream error", "host", r.Host, "request_id", r.Header.Get("X-Request-Id"), "error", err)
			writePage(w, http.StatusBadGateway, "The app is not responding",
				"The app is running but did not answer. Try again in a moment.")
		},
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Request-Id")
	if id == "" || len(id) > 64 {
		id = newRequestID()
	}
	r.Header.Set("X-Request-Id", id)
	w.Header().Set("X-Request-Id", id)

	slug, ok := AppSlug(r.Host, h.appsDomain)
	if !ok {
		writePage(w, http.StatusNotFound, "Not found", "This address is not served by Shipyard.")
		return
	}
	route, ok := h.table.Lookup(slug)
	if !ok {
		writePage(w, http.StatusNotFound, "No app named "+slug, "There is no app at this address.")
		return
	}
	if strings.HasPrefix(r.URL.Path, reservedPrefix) && r.URL.Path != readyPath {
		// Reserved for the router; never proxied.
		if r.URL.Path == callbackPath && h.gate != nil {
			h.gate.callback(w, r, route)
			return
		}
		http.NotFound(w, r)
		return
	}
	var identity *Identity
	if h.gate != nil {
		var allowed bool
		if identity, allowed = h.gate.authorize(w, r, route); !allowed {
			return
		}
	} else if route.Visibility != "public" {
		writePage(w, http.StatusServiceUnavailable, "Access check unavailable", "This router has no access control configured, so it only serves public apps.")
		return
	}
	if r.URL.Path == readyPath { // reserved, never proxied; reached only after the access check
		h.ready(w, route)
		return
	}
	if route.Port == 0 {
		if h.wake == nil || (route.Status != "sleeping" && route.Status != "starting") {
			writePage(w, http.StatusServiceUnavailable, "The app is not running",
				"This app is stopped or still starting. Start it from the dashboard, or try again in a moment.")
			return
		}
		// An authorised request for a sleeping app wakes it and waits (ROADMAP 3A).
		h.act.mark(route.AppID)
		var out outcome
		route, out = h.wake.await(r.Context(), route, wantsHTML(r))
		switch out {
		case outReady:
		case outStillWaking:
			writeWakingPage(w)
			return
		case outOverloaded:
			writeBusy(w, "This app is waking up", "Many requests are waiting for it. Try again in a few seconds.")
			return
		case outFailed:
			writeBusy(w, "The app did not wake up", "It did not start in time. Try again in a moment, or start it from the dashboard.")
			return
		default: // the client went away
			return
		}
	}
	h.act.mark(route.AppID)
	if isUpgrade(r) {
		defer h.act.hold(route.AppID, wsKeepAlive)() // an open WebSocket is activity
	}
	h.proxy.ServeHTTP(w, r.WithContext(withInfo(r.Context(), reqInfo{Route: route, Identity: identity})))
}

// ready answers the waking page's poll: 200 once the app runs, 202 while it
// is waking (and makes sure a wake is under way), 503 if it is not going to.
func (h *handler) ready(w http.ResponseWriter, route Route) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case routeRunning(route):
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ready":true}`))
	case h.wake != nil && (route.Status == "sleeping" || route.Status == "starting"):
		h.wake.kick(route)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ready":false}`))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false}`))
	}
}

// wantsHTML is true for a browser loading a page.
func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// isUpgrade is true for a WebSocket handshake.
func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (h *handler) rewrite(pr *httputil.ProxyRequest) {
	info, _ := infoFrom(pr.In.Context())
	route := info.Route
	pr.Out.URL = &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(route.Port)),
		Path: pr.In.URL.Path, RawPath: pr.In.URL.RawPath, RawQuery: pr.In.URL.RawQuery}
	pr.Out.Host = pr.In.Host

	// Apps never see platform identity input: drop every X-Shipyard-* header and the
	// router's own cookie, so neither can be spoofed by a client or leak to app code.
	for k := range pr.Out.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-shipyard-") {
			delete(pr.Out.Header, k)
		}
	}
	stripCookie(pr.Out, appCookie)
	if id := info.Identity; id != nil && cleanHeaderValue(id.UserID) && cleanHeaderValue(id.Email) {
		pr.Out.Header.Set("X-Shipyard-User-Id", id.UserID)
		pr.Out.Header.Set("X-Shipyard-User-Email", id.Email)
	}

	// Keep the client chain Caddy built, then add our hop.
	if xff := pr.In.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		pr.Out.Header["X-Forwarded-For"] = xff
	}
	pr.SetXForwarded()
	if proto := pr.In.Header.Get("X-Forwarded-Proto"); proto != "" {
		pr.Out.Header.Set("X-Forwarded-Proto", proto)
	}
}

// stripCookie removes the named cookie from the request, keeping the others.
func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16) // crypto/rand does not fail in practice
	}
	return hex.EncodeToString(b[:])
}
