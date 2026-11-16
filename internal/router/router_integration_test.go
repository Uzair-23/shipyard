//go:build integration

package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var (
	testPool *pgxpool.Pool
	seq      atomic.Int32
)

// uniq makes slugs unique so tests can run with -count > 1.
func uniq(base string) string { return fmt.Sprintf("%s-%d", base, seq.Add(1)) }

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	dbURL, stop, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	if err := store.Migrate(ctx, dbURL); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, dbURL); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()
	return m.Run()
}

// seedApp creates an app whose current deploy has a running instance on port.
func seedApp(t *testing.T, slug string, port int) string {
	t.Helper()
	ctx := context.Background()
	var appID string
	if err := testPool.QueryRow(ctx, `INSERT INTO apps (slug, status) VALUES ($1, 'running') RETURNING id::text`, slug).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	var deployID string
	if err := testPool.QueryRow(ctx, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 1, 'sha256:x', 'live') RETURNING id::text`, appID).Scan(&deployID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO instances (deploy_id, state, host_port) VALUES ($1, 'running', $2)`, deployID, port); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE apps SET current_deploy_id = $1 WHERE id = $2`, deployID, appID); err != nil {
		t.Fatal(err)
	}
	return appID
}

func notify(t *testing.T, appID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `SELECT pg_notify('route_changes', $1)`, appID); err != nil {
		t.Fatal(err)
	}
}

func startRouter(t *testing.T, reload, flush time.Duration) *Router {
	t.Helper()
	r := New(testPool, "apps.localhost", "")
	r.ReloadInterval, r.FlushInterval = reload, flush
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("router: %v", err)
		}
	})
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func port(r *Router, slug string) int {
	route, _ := r.table.Lookup(slug)
	return route.Port
}

// Routes exist as soon as the router boots (restart restores them).
func TestRoutesLoadedAtBoot(t *testing.T) {
	slug := uniq("boot-app")
	seedApp(t, slug, 4101)
	r := startRouter(t, time.Hour, time.Hour)
	// Periodic reload is 1 h and nothing is notified: the route can only come from the boot load.
	waitFor(t, "route from the boot load", func() bool { return port(r, slug) == 4101 })
}

// NOTIFY route_changes updates one app; the periodic reload is disabled (1 h) so only the notification can do it.
func TestNotifyUpdatesAndRemovesRoute(t *testing.T) {
	r := startRouter(t, time.Hour, time.Hour)
	time.Sleep(300 * time.Millisecond) // let LISTEN attach

	slug := uniq("notify-app")
	appID := seedApp(t, slug, 4201)
	if port(r, slug) != 0 {
		t.Fatal("route appeared without a notification")
	}
	notify(t, appID)
	waitFor(t, "route after NOTIFY", func() bool { return port(r, slug) == 4201 })

	if _, err := testPool.Exec(context.Background(), `UPDATE instances SET state = 'stopped' WHERE deploy_id = (SELECT current_deploy_id FROM apps WHERE id = $1)`, appID); err != nil {
		t.Fatal(err)
	}
	notify(t, appID)
	waitFor(t, "stopped instance to clear the port", func() bool {
		route, ok := r.table.Lookup(slug)
		return ok && route.Port == 0
	})

	if _, err := testPool.Exec(context.Background(), `UPDATE apps SET deleted_at = now() WHERE id = $1`, appID); err != nil {
		t.Fatal(err)
	}
	notify(t, appID)
	waitFor(t, "deleted app to leave the table", func() bool { _, ok := r.table.Lookup(slug); return !ok })
}

// Without any notification the periodic reload still converges (fallback).
func TestPeriodicReloadFallback(t *testing.T) {
	r := startRouter(t, 150*time.Millisecond, time.Hour)
	slug := uniq("fallback-app")
	seedApp(t, slug, 4301)
	waitFor(t, "route via periodic reload", func() bool { return port(r, slug) == 4301 })
}

func TestLastRequestAtIsFlushed(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	p, _ := strconv.Atoi(u.Port())
	slug := uniq("touch-app")
	appID := seedApp(t, slug, p)

	r := startRouter(t, time.Hour, 100*time.Millisecond)
	front := httptest.NewServer(r.Handler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.Host = slug + ".apps.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	waitFor(t, "last_request_at", func() bool {
		var set bool
		err := testPool.QueryRow(context.Background(), `SELECT last_request_at IS NOT NULL FROM apps WHERE id = $1`, appID).Scan(&set)
		return err == nil && set
	})
}
