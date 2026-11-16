package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitHealthyBecomesHealthy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // still starting
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := WaitHealthy(context.Background(), srv.URL+"/healthz", "/healthz", 10*time.Millisecond, 2*time.Second); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d probes, want 3", calls.Load())
	}
}

func TestWaitHealthyAcceptsRedirectWithoutFollowing(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	if err := WaitHealthy(context.Background(), srv.URL+"/", "/", 10*time.Millisecond, time.Second); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
	if followed.Load() {
		t.Fatal("health check followed a redirect to another host")
	}
}

func TestWaitHealthyTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	start := time.Now()
	err := WaitHealthy(context.Background(), srv.URL+"/x", "/x", 20*time.Millisecond, 300*time.Millisecond)
	var ue *UnhealthyError
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "HTTP 500") || !strings.Contains(ue.Error(), "/x") {
		t.Fatalf("err = %v, want UnhealthyError mentioning HTTP 500 and /x", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s, want ~300ms", d)
	}
}

func TestWaitHealthyConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	err := WaitHealthy(context.Background(), url+"/", "/", 20*time.Millisecond, 200*time.Millisecond)
	var ue *UnhealthyError
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "connection failed") {
		t.Fatalf("err = %v, want UnhealthyError with connection failed", err)
	}
}

func TestWaitHealthyCallerCancelIsNotUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := WaitHealthy(ctx, srv.URL+"/", "/", 10*time.Millisecond, 10*time.Second)
	var ue *UnhealthyError
	if errors.As(err, &ue) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context error", err)
	}
}
