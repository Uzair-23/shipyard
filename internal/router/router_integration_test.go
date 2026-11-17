//go:build integration

package router

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
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
	if err := testPool.QueryRow(ctx, `INSERT INTO apps (slug, status, visibility) VALUES ($1, 'running', 'public') RETURNING id::text`, slug).Scan(&appID); err != nil {
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

func seedUser(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO users (email) VALUES ($1) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedPrivateApp(t *testing.T, owner string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO apps (slug, owner_id, visibility) VALUES ($1, $2, 'private') RETURNING id::text`, uniq("priv"), owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertCode(t *testing.T, code, appID, userID, returnPath string, expires time.Time) {
	t.Helper()
	h := sha256.Sum256([]byte(code))
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO handoff_codes (code_hash, app_id, user_id, return_path, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		h[:], appID, userID, returnPath, expires); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffCodeIsSingleUseAndBoundToItsApp(t *testing.T) {
	ctx := context.Background()
	be := newDBBackend(testPool, 0)
	user := seedUser(t, uniq("hand")+"@example.com")
	appA, appB := seedPrivateApp(t, user), seedPrivateApp(t, user)

	// Valid code: returns the user, email and stored return path, once.
	insertCode(t, "code-ok", appA, user, "/docs", time.Now().Add(time.Minute))
	uid, email, ret, ok, err := be.ConsumeHandoff(ctx, appA, "code-ok")
	if err != nil || !ok || uid != user || ret != "/docs" || !strings.HasSuffix(email, "@example.com") {
		t.Fatalf("first use: %q %q %q %v %v", uid, email, ret, ok, err)
	}
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appA, "code-ok"); ok {
		t.Fatal("a code was accepted twice")
	}

	// Another app's host cannot use it, and the attempt does not burn it.
	insertCode(t, "code-a", appA, user, "/", time.Now().Add(time.Minute))
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appB, "code-a"); ok {
		t.Fatal("a code for app A was accepted for app B")
	}
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appA, "code-a"); !ok {
		t.Fatal("the wrong-app attempt used the code up")
	}

	// Expired, unknown and suspended-user codes.
	insertCode(t, "code-old", appA, user, "/", time.Now().Add(-time.Second))
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appA, "code-old"); ok {
		t.Error("an expired code was accepted")
	}
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appA, "never-issued"); ok {
		t.Error("an unknown code was accepted")
	}
	suspended := seedUser(t, uniq("susp")+"@example.com")
	if _, err := testPool.Exec(ctx, `UPDATE users SET suspended_at = now() WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	insertCode(t, "code-susp", appA, suspended, "/", time.Now().Add(time.Minute))
	if _, _, _, ok, _ := be.ConsumeHandoff(ctx, appA, "code-susp"); ok {
		t.Error("a suspended user's code was accepted")
	}
}

func TestHandoffCodeConsumedByExactlyOneOfManyRacers(t *testing.T) {
	be := newDBBackend(testPool, 0)
	user := seedUser(t, uniq("race")+"@example.com")
	app := seedPrivateApp(t, user)
	insertCode(t, "race-code", app, user, "/", time.Now().Add(time.Minute))

	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, ok, err := be.ConsumeHandoff(context.Background(), app, "race-code"); err == nil && ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d of 20 concurrent requests consumed the same code, want exactly 1", wins.Load())
	}
}

func TestAccessDecisionsFollowMembershipAfterTheCacheExpires(t *testing.T) {
	ctx := context.Background()
	be := newDBBackend(testPool, 300*time.Millisecond)
	owner := seedUser(t, uniq("own")+"@example.com")
	guest := uniq("guest") + "@example.com"
	guestID := seedUser(t, guest)
	app := seedPrivateApp(t, owner)

	can := func(user, email string) bool {
		ok, err := be.CanView(ctx, app, user, email)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !can(owner, "x@example.com") {
		t.Error("the owner cannot view their own app")
	}
	if can(guestID, guest) {
		t.Error("a stranger can view a private app")
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO app_members (app_id, email) VALUES ($1, upper($2))`, app, guest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "membership to take effect after the cache TTL", func() bool { return can(guestID, guest) })

	if _, err := testPool.Exec(ctx, `DELETE FROM app_members WHERE app_id = $1`, app); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removal to take effect after the cache TTL", func() bool { return !can(guestID, guest) })

	if _, err := testPool.Exec(ctx, `UPDATE apps SET visibility = 'public' WHERE id = $1`, app); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "public visibility to open the app", func() bool { return can(guestID, guest) })
}

// seedSleeping creates a public app that went to sleep: live deploy, no running instance.
func seedSleeping(t *testing.T, slug string) string {
	t.Helper()
	ctx := context.Background()
	var appID string
	if err := testPool.QueryRow(ctx, `INSERT INTO apps (slug, status, visibility) VALUES ($1, 'sleeping', 'public') RETURNING id::text`, slug).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	var deployID string
	if err := testPool.QueryRow(ctx, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 1, 'sha256:x', 'live') RETURNING id::text`, appID).Scan(&deployID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO instances (deploy_id, state) VALUES ($1, 'stopped')`, deployID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE apps SET current_deploy_id = $1 WHERE id = $2`, deployID, appID); err != nil {
		t.Fatal(err)
	}
	return appID
}

func launchJobs(t *testing.T, appID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE app_id = $1 AND kind = 'launch'`, appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 50 concurrent requests to a sleeping app cause exactly one start job; once the
// (simulated) worker brings the app up, every request is served and the cold start is stored.
func TestFiftyRequestsToSleepingAppEnqueueOneLaunch(t *testing.T) {
	ctx := context.Background()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("awake")) }))
	defer app.Close()
	appPort, _ := strconv.Atoi(strings.TrimPrefix(app.URL, "http://127.0.0.1:"))

	slug := uniq("sleeper")
	appID := seedSleeping(t, slug)
	r := startRouter(t, time.Minute, time.Minute)
	r.WakeTimeout = 20 * time.Second
	waitUntil(t, "sleeping route", func() bool { _, ok := r.table.Lookup(slug); return ok })
	front := httptest.NewServer(r.Handler())
	defer front.Close()

	const n = 50
	codes := make(chan int, n)
	for range n {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
			req.Host = slug + ".apps.localhost"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	waitUntil(t, "the launch job", func() bool { return launchJobs(t, appID) >= 1 })
	time.Sleep(300 * time.Millisecond) // let the other requests arrive and join the wake
	if got := launchJobs(t, appID); got != 1 {
		t.Fatalf("launch jobs = %d, want exactly 1", got)
	}

	// The worker's launch job, simulated: a running instance, status running, NOTIFY.
	if _, err := testPool.Exec(ctx, `UPDATE instances SET state = 'running', host_port = $1 WHERE deploy_id = (SELECT current_deploy_id FROM apps WHERE id = $2)`, appPort, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE apps SET status = 'running' WHERE id = $1`, appID); err != nil {
		t.Fatal(err)
	}
	notify(t, appID)

	for range n {
		select {
		case c := <-codes:
			if c != http.StatusOK {
				t.Fatalf("a request got %d", c)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("requests were not released after the app woke")
		}
	}
	if got := launchJobs(t, appID); got != 1 {
		t.Fatalf("launch jobs after wake = %d, want 1", got)
	}
	var ms *int32
	if err := testPool.QueryRow(ctx, `SELECT last_cold_start_ms FROM apps WHERE id = $1`, appID).Scan(&ms); err != nil || ms == nil || *ms <= 0 {
		t.Fatalf("last_cold_start_ms = %v (%v), want a positive value", ms, err)
	}
}
