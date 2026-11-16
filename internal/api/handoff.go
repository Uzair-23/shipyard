package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/access"
	"github.com/Uzair-23/shipyard/internal/auth"
	"github.com/Uzair-23/shipyard/internal/store"
)

// handoffTTL is how long a handoff code can be exchanged for an app cookie.
const handoffTTL = 60 * time.Second

// authApp is GET /auth/app?app=<slug>&return=<path>, the start of the auth
// handoff (ADR 005). The router sends a browser here when it has no valid
// cookie for a private app. A logged-in user with access gets a one-time code
// and is redirected to the app's own host, which trades it for its cookie; the
// dashboard session cookie never leaves this host.
func (a *Apps) authApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	user, ok, err := a.sessions.Lookup(r.Context(), auth.TokenFromRequest(r))
	if err != nil {
		internalError(w, "resolve session", err)
		return
	}
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	app, err := store.New(a.Pool).GetAppBySlug(r.Context(), q.Get("app"))
	if errors.Is(err, pgx.ErrNoRows) {
		a.noAccess(w, user) // the same answer as "not allowed": it does not reveal which apps exist
		return
	}
	if err != nil {
		internalError(w, "load app", err)
		return
	}
	allowed, err := access.CanView(r.Context(), a.Pool, app.ID, user.ID, user.Email)
	if err != nil {
		internalError(w, "check access", err)
		return
	}
	if !allowed {
		a.noAccess(w, user)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		internalError(w, "generate handoff code", err)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(code))
	err = store.New(a.Pool).CreateHandoffCode(r.Context(), store.CreateHandoffCodeParams{
		CodeHash: hash[:], AppID: app.ID, UserID: user.ID,
		ReturnPath: auth.SafeReturnPath(q.Get("return")),
		ExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(handoffTTL), Valid: true},
	})
	if err != nil {
		internalError(w, "store handoff code", err)
		return
	}
	scheme := "http"
	if auth.IsSecure(r) {
		scheme = "https"
	}
	to := scheme + "://" + app.Slug + "." + a.AppsDomain + "/__shipyard/callback?code=" + url.QueryEscape(code)
	http.Redirect(w, r, to, http.StatusFound)
}

var noAccessTmpl = template.Must(template.New("noaccess").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>No access</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#f6f7f9;color:#1c2024}
main{max-width:30rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#555;line-height:1.5}</style></head>
<body><main><h1>No access</h1>
<p>You are signed in as <strong>{{.Email}}</strong>, which does not have access to this app.
Ask its owner to share it with this email address.</p>
<p><a href="/">Back to the dashboard</a></p></main></body></html>`))

func (a *Apps) noAccess(w http.ResponseWriter, user auth.User) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = noAccessTmpl.Execute(w, map[string]string{"Email": user.Email})
}
