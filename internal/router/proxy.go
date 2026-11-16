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
	gate       *gate // nil: private apps are refused
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
	if strings.HasPrefix(r.URL.Path, reservedPrefix) {
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
	if route.Port == 0 {
		writePage(w, http.StatusServiceUnavailable, "The app is not running",
			"This app is stopped or still starting. Start it from the dashboard, or try again in a moment.")
		return
	}
	h.act.mark(route.AppID)
	h.proxy.ServeHTTP(w, r.WithContext(withInfo(r.Context(), reqInfo{Route: route, Identity: identity})))
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
