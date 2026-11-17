package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// appServer is an upstream that answers every request (unlike upstream(), whose
// small channel would block 50 concurrent requests).
func appServer(t *testing.T) (port int, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "hello from app")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	return p, hits
}

// wakeSetup returns a handler for one sleeping public app "sleepy". The fake
// worker does nothing until the test calls wakeUp.
type wakeSetup struct {
	h        *handler
	w        *waker
	tb       *Table
	enqueued atomic.Int32
	port     int
	hits     *atomic.Int32
	recorded chan time.Duration
}

func newWakeSetup(t *testing.T, status string) *wakeSetup {
	t.Helper()
	s := &wakeSetup{recorded: make(chan time.Duration, 4)}
	s.port, s.hits = appServer(t)
	s.tb = NewTable()
	s.tb.Set(Route{AppID: "app-1", Slug: "sleepy", Status: status, Visibility: "public"})
	s.w = newWaker(s.tb, func(context.Context, string) error { s.enqueued.Add(1); return nil },
		func(_, _ string, d time.Duration) { s.recorded <- d })
	s.w.timeout = 5 * time.Second
	s.h = newHandler(s.tb, "apps.localhost", newActivity(), nil)
	s.h.wake = s.w
	return s
}

// wakeUp is what the worker's launch job does: the app becomes running with a port.
func (s *wakeSetup) wakeUp() {
	s.tb.Set(Route{AppID: "app-1", Slug: "sleepy", Status: "running", Visibility: "public", Port: s.port})
}

func (s *wakeSetup) waiting() int {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	return s.w.waiting["app-1"]
}

// pollUntil reports whether cond became true within two seconds (safe in goroutines).
func pollUntil(cond func() bool) bool {
	for i := 0; i < 400; i++ {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !pollUntil(cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestFiftyConcurrentRequestsStartTheAppOnceAndAreAllServed(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	const n = 50
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(s.h, "sleepy.apps.localhost", "/", http.Header{"Accept": {"application/json"}})
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	waitUntil(t, "all 50 requests to wait", func() bool { return s.waiting() == n })
	s.wakeUp()
	wg.Wait()

	for i := range n {
		if codes[i] != http.StatusOK || bodies[i] != "hello from app" {
			t.Fatalf("request %d: %d %q", i, codes[i], bodies[i])
		}
	}
	if got := s.enqueued.Load(); got != 1 {
		t.Fatalf("launch jobs enqueued = %d, want exactly 1", got)
	}
	select {
	case d := <-s.recorded:
		if d <= 0 {
			t.Fatalf("cold start = %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("cold start was not recorded")
	}
	// Awake: further requests do not enqueue anything.
	if rec := do(s.h, "sleepy.apps.localhost", "/", nil); rec.Code != http.StatusOK || s.enqueued.Load() != 1 {
		t.Fatalf("request to an awake app: %d, enqueued %d", rec.Code, s.enqueued.Load())
	}
}

func TestWaitingRequestsAreCapped(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	s.w.maxWaiters = 2
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- do(s.h, "sleepy.apps.localhost", "/", nil).Code
		}()
	}
	waitUntil(t, "two waiters", func() bool { return s.waiting() == 2 })

	start := time.Now()
	rec := do(s.h, "sleepy.apps.localhost", "/", nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("third request: %d Retry-After=%q, want 503 and 2", rec.Code, rec.Header().Get("Retry-After"))
	}
	if time.Since(start) > time.Second {
		t.Fatalf("over-the-cap request waited %v instead of failing at once", time.Since(start))
	}
	if s.enqueued.Load() != 1 {
		t.Fatalf("enqueued = %d, want 1", s.enqueued.Load())
	}
	s.wakeUp()
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("waiting request got %d", c)
		}
	}
}

func TestWakeTimeoutAnswers503WithRetryAfter(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	s.w.timeout = 150 * time.Millisecond
	rec := do(s.h, "sleepy.apps.localhost", "/", nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("got %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if s.hits.Load() != 0 {
		t.Fatal("a request reached the app that never woke")
	}
}

func TestBrowserWaitingTooLongGetsWakingPageAndWakeContinues(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	s.w.browserWait = 100 * time.Millisecond
	html := http.Header{"Accept": {"text/html,application/xhtml+xml"}}

	rec := do(s.h, "sleepy.apps.localhost", "/some/page", html)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Waking up") ||
		!strings.Contains(rec.Body.String(), "/__shipyard/ready") {
		t.Fatalf("browser got %d %q, want the waking page", rec.Code, rec.Body.String())
	}
	if s.hits.Load() != 0 {
		t.Fatal("the waking page request reached the app")
	}

	// The wake goes on without the first request; the reload is served by the app.
	s.wakeUp()
	if rec := do(s.h, "sleepy.apps.localhost", "/some/page", html); rec.Code != http.StatusOK {
		t.Fatalf("reload after wake: %d", rec.Code)
	}
	if s.enqueued.Load() != 1 {
		t.Fatalf("enqueued = %d, want 1", s.enqueued.Load())
	}
}

func TestBrowserGetsTheAppWhenItWakesQuickly(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	s.w.browserWait = 2 * time.Second
	go func() {
		if pollUntil(func() bool { return s.waiting() == 1 }) {
			s.wakeUp()
		}
	}()
	rec := do(s.h, "sleepy.apps.localhost", "/", http.Header{"Accept": {"text/html"}})
	if rec.Code != http.StatusOK || rec.Body.String() != "hello from app" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestReadyEndpointWakesReportsAndIsNeverProxied(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	rec := do(s.h, "sleepy.apps.localhost", "/__shipyard/ready", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ready while asleep: %d, want 202", rec.Code)
	}
	waitUntil(t, "the wake to enqueue a launch", func() bool { return s.enqueued.Load() == 1 })
	do(s.h, "sleepy.apps.localhost", "/__shipyard/ready", nil) // polling again joins the same wake
	if s.enqueued.Load() != 1 {
		t.Fatalf("polling enqueued %d jobs", s.enqueued.Load())
	}
	s.wakeUp()
	waitUntil(t, "flight to end", func() bool {
		s.w.mu.Lock()
		defer s.w.mu.Unlock()
		return len(s.w.flights) == 0
	})
	if rec := do(s.h, "sleepy.apps.localhost", "/__shipyard/ready", nil); rec.Code != http.StatusOK {
		t.Fatalf("ready when running: %d", rec.Code)
	}
	if s.hits.Load() != 0 {
		t.Fatal("/__shipyard/ready was proxied to the app")
	}
	// A stopped app does not start because of a poll.
	s.tb.Set(Route{AppID: "app-1", Slug: "sleepy", Status: "stopped", Visibility: "public"})
	if rec := do(s.h, "sleepy.apps.localhost", "/__shipyard/ready", nil); rec.Code != http.StatusServiceUnavailable || s.enqueued.Load() != 1 {
		t.Fatalf("ready for a stopped app: %d, enqueued %d", rec.Code, s.enqueued.Load())
	}
}

func TestStoppedOrCrashedAppsAreNotWoken(t *testing.T) {
	for _, status := range []string{"stopped", "crashed"} {
		s := newWakeSetup(t, status)
		if rec := do(s.h, "sleepy.apps.localhost", "/", nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: got %d", status, rec.Code)
		}
		if s.enqueued.Load() != 0 {
			t.Fatalf("%s app was woken", status)
		}
	}
}

func TestStartingAppIsWaitedForWithoutAnotherStart(t *testing.T) {
	s := newWakeSetup(t, "starting")
	go func() {
		if pollUntil(func() bool { return s.waiting() == 1 }) {
			s.wakeUp()
		}
	}()
	if rec := do(s.h, "sleepy.apps.localhost", "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if s.enqueued.Load() != 0 {
		t.Fatalf("a starting app was enqueued again (%d)", s.enqueued.Load())
	}
}

func TestPrivateSleepingAppIsNotWokenByAnUnauthorizedRequest(t *testing.T) {
	h, tb := gatedHandler(&fakeBackend{}, Route{AppID: "app-1", Slug: "secret", Status: "sleeping", Visibility: "private"})
	var enqueued atomic.Int32
	h.wake = newWaker(tb, func(context.Context, string) error { enqueued.Add(1); return nil }, nil)
	for _, path := range []string{"/", "/__shipyard/ready"} {
		rec := get(h, "secret.apps.localhost", path)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/auth/app") {
			t.Fatalf("%s: got %d Location=%q, want the login handoff", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if enqueued.Load() != 0 {
		t.Fatalf("an unauthorized request woke the app (%d jobs)", enqueued.Load())
	}
}

func TestEnqueueFailureAnswers503(t *testing.T) {
	s := newWakeSetup(t, "sleeping")
	s.w.enqueue = func(context.Context, string) error { return context.DeadlineExceeded }
	if rec := do(s.h, "sleepy.apps.localhost", "/", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestHoldKeepsMarkingActivityUntilReleased(t *testing.T) {
	a := newActivity()
	release := a.hold("app-1", 10*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	first := a.drain()["app-1"]
	if first.IsZero() {
		t.Fatal("no activity recorded while the connection was open")
	}
	time.Sleep(40 * time.Millisecond)
	if a.drain()["app-1"].IsZero() {
		t.Fatal("activity stopped while the connection was still open")
	}
	release()
	time.Sleep(30 * time.Millisecond) // let the ticker goroutine exit
	a.drain()
	time.Sleep(40 * time.Millisecond)
	if !a.drain()["app-1"].IsZero() {
		t.Fatal("activity kept being recorded after the connection closed")
	}
}
