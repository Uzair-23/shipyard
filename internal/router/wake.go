package router

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Defaults for waking a sleeping app (ROADMAP 3A).
const (
	defaultWakeTimeout = 30 * time.Second
	defaultBrowserWait = 1500 * time.Millisecond
	defaultMaxWaiters  = 100
	wakePollInterval   = 50 * time.Millisecond
)

// outcome is how a request's wait for a sleeping app ended.
type outcome int

const (
	outReady       outcome = iota // the app is running; proxy the request
	outStillWaking                // a browser waited long enough; show the waking page
	outOverloaded                 // too many requests already wait for this app
	outFailed                     // the wake could not be started or timed out
	outGone                       // the client went away
)

// waker wakes sleeping apps. For each app there is at most one wake in flight
// (a "flight"): the first request enqueues a launch job, every other request
// for that app just waits on the same flight. A flight ends when the route
// table (kept current by route_changes) shows the app running, or on timeout.
type waker struct {
	table *Table
	// enqueue asks the worker to start the app's current deploy again.
	enqueue func(ctx context.Context, appID string) error
	// record is told how long a wake took, from enqueue to a running route.
	record func(appID, slug string, d time.Duration)

	timeout     time.Duration // total wait for one wake
	browserWait time.Duration // how long a browser page request waits before the waking page
	maxWaiters  int           // waiting requests per app

	mu      sync.Mutex
	flights map[string]*flight
	waiting map[string]int
}

type flight struct {
	done  chan struct{}
	ready bool
}

func newWaker(table *Table, enqueue func(context.Context, string) error, record func(appID, slug string, d time.Duration)) *waker {
	return &waker{
		table: table, enqueue: enqueue, record: record,
		timeout: defaultWakeTimeout, browserWait: defaultBrowserWait, maxWaiters: defaultMaxWaiters,
		flights: map[string]*flight{}, waiting: map[string]int{},
	}
}

func routeRunning(r Route) bool { return r.Port != 0 && r.Status == "running" }

// join returns the app's flight, starting one if none is in flight.
func (w *waker) join(route Route) *flight {
	w.mu.Lock()
	defer w.mu.Unlock()
	if f, ok := w.flights[route.AppID]; ok {
		return f
	}
	f := &flight{done: make(chan struct{})}
	w.flights[route.AppID] = f
	go w.run(f, route)
	return f
}

// run is the single goroutine of a flight.
func (w *waker) run(f *flight, route Route) {
	start := time.Now()
	defer func() {
		w.mu.Lock()
		delete(w.flights, route.AppID)
		w.mu.Unlock()
		close(f.done)
	}()
	enqueued := false
	if route.Status == "sleeping" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := w.enqueue(ctx, route.AppID)
		cancel()
		if err != nil {
			slog.Error("wake: enqueue launch failed", "app_id", route.AppID, "error", err)
			return
		}
		enqueued = true
	}
	deadline := time.NewTimer(w.timeout)
	defer deadline.Stop()
	tick := time.NewTicker(wakePollInterval)
	defer tick.Stop()
	for {
		cur, ok := w.table.Lookup(route.Slug)
		if !ok || cur.AppID != route.AppID {
			return // the app was deleted or renamed
		}
		if routeRunning(cur) {
			f.ready = true
			if enqueued && w.record != nil {
				w.record(route.AppID, route.Slug, time.Since(start))
			}
			return
		}
		select {
		case <-deadline.C:
			slog.Warn("wake timed out", "app_id", route.AppID, "slug", route.Slug)
			return
		case <-tick.C:
		}
	}
}

// await waits until the app is running. A browser page request (patience =
// browserWait) gives up early so the caller can show the waking page; the wake
// goes on in the background.
func (w *waker) await(ctx context.Context, route Route, browser bool) (Route, outcome) {
	w.mu.Lock()
	if w.waiting[route.AppID] >= w.maxWaiters {
		w.mu.Unlock()
		return route, outOverloaded
	}
	w.waiting[route.AppID]++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.waiting[route.AppID]--; w.waiting[route.AppID] <= 0 {
			delete(w.waiting, route.AppID)
		}
		w.mu.Unlock()
	}()

	f := w.join(route)
	var patience <-chan time.Time
	if browser {
		t := time.NewTimer(w.browserWait)
		defer t.Stop()
		patience = t.C
	}
	select {
	case <-f.done:
		if cur, ok := w.table.Lookup(route.Slug); ok && f.ready && routeRunning(cur) {
			return cur, outReady
		}
		return route, outFailed
	case <-patience:
		return route, outStillWaking
	case <-ctx.Done():
		return route, outGone
	}
}

// kick makes sure a wake is under way (used by the waking page's polling).
func (w *waker) kick(route Route) { w.join(route) }
