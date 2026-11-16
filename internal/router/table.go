package router

import (
	"strings"
	"sync"
)

// Route is where one app's traffic goes. Port is 0 when no instance is running.
type Route struct {
	AppID  string
	Slug   string
	Status string
	Port   int
}

// Table is the in-memory route table, safe for concurrent use.
type Table struct {
	mu     sync.RWMutex
	bySlug map[string]Route
	slugOf map[string]string // app id -> slug, so a rename or delete drops the old entry
}

// NewTable returns an empty table.
func NewTable() *Table {
	return &Table{bySlug: map[string]Route{}, slugOf: map[string]string{}}
}

// Lookup returns the route for a slug.
func (t *Table) Lookup(slug string) (Route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.bySlug[slug]
	return r, ok
}

// Set adds or replaces one app's route.
func (t *Table) Set(r Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(r)
}

func (t *Table) setLocked(r Route) {
	if old, ok := t.slugOf[r.AppID]; ok && old != r.Slug {
		delete(t.bySlug, old)
	}
	t.bySlug[r.Slug] = r
	t.slugOf[r.AppID] = r.Slug
}

// Remove drops an app's route (the app was deleted).
func (t *Table) Remove(appID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if slug, ok := t.slugOf[appID]; ok {
		delete(t.bySlug, slug)
		delete(t.slugOf, appID)
	}
}

// Replace swaps in a complete set of routes atomically.
func (t *Table) Replace(routes []Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bySlug = make(map[string]Route, len(routes))
	t.slugOf = make(map[string]string, len(routes))
	for _, r := range routes {
		t.setLocked(r)
	}
}

// AppSlug extracts the app slug from a Host header such as
// "my-app.apps.localhost:8081". It accepts exactly one label in front of
// appsDomain, so "a.b.apps.localhost" and "apps.localhost" are not apps.
func AppSlug(host, appsDomain string) (string, bool) {
	host = strings.ToLower(host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	slug, ok := strings.CutSuffix(host, "."+strings.ToLower(appsDomain))
	if !ok || slug == "" || strings.Contains(slug, ".") {
		return "", false
	}
	return slug, true
}
