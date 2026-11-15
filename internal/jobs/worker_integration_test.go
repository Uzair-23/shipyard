//go:build integration

package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	url, terminate, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer terminate()
	if err := store.Migrate(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()
	return m.Run()
}

// fastWorker returns a worker tuned for tests; callers override fields as needed.
func fastWorker(id string, handlers map[string]Handler) *Worker {
	return &Worker{
		Pool:         testPool,
		Handlers:     handlers,
		ID:           id,
		Concurrency:  4,
		Lease:        2 * time.Second,
		Heartbeat:    500 * time.Millisecond,
		PollInterval: 100 * time.Millisecond,
		Backoff:      func(int32) time.Duration { return 10 * time.Millisecond },
	}
}

// start runs workers in the background and stops them when the test ends.
func start(t *testing.T, workers ...*Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func resetJobs(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM jobs`); err != nil {
		t.Fatalf("reset jobs: %v", err)
	}
}

func enqueue(t *testing.T, kind string, maxAttempts int32) store.Job {
	t.Helper()
	job, err := Enqueue(context.Background(), testPool, store.EnqueueJobParams{Kind: kind, MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return job
}

func getJob(t *testing.T, id pgtype.UUID) store.Job {
	t.Helper()
	var j store.Job
	err := testPool.QueryRow(context.Background(),
		`SELECT state, attempts, reclaims, last_error FROM jobs WHERE id = $1`, id).
		Scan(&j.State, &j.Attempts, &j.Reclaims, &j.LastError)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return j
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countState(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE state = $1`, state).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func TestEachJobRunsExactlyOnce(t *testing.T) {
	resetJobs(t)
	const n = 50
	var runs sync.Map // job id -> *int32
	handler := func(_ context.Context, job store.Job) error {
		c, _ := runs.LoadOrStore(job.ID.String(), new(int32))
		atomic.AddInt32(c.(*int32), 1)
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	for range n {
		enqueue(t, "count", 1)
	}
	h := map[string]Handler{"count": handler}
	start(t, fastWorker("w1", h), fastWorker("w2", h))

	waitFor(t, 30*time.Second, "all jobs done", func() bool { return countState(t, "done") == n })

	seen := 0
	runs.Range(func(id, c any) bool {
		seen++
		if got := atomic.LoadInt32(c.(*int32)); got != 1 {
			t.Errorf("job %s ran %d times, want 1", id, got)
		}
		return true
	})
	if seen != n {
		t.Fatalf("%d distinct jobs ran, want %d", seen, n)
	}
}

func TestFailingJobRetriesUntilMaxAttempts(t *testing.T) {
	resetJobs(t)
	var calls atomic.Int32
	job := enqueue(t, "flaky", 3)
	start(t, fastWorker("w1", map[string]Handler{"flaky": func(context.Context, store.Job) error {
		calls.Add(1)
		return errors.New("boom")
	}}))

	waitFor(t, 10*time.Second, "job failed", func() bool { return getJob(t, job.ID).State == "failed" })
	got := getJob(t, job.ID)
	if got.Attempts != 3 || calls.Load() != 3 {
		t.Fatalf("attempts=%d calls=%d, want 3 and 3", got.Attempts, calls.Load())
	}
	if got.LastError.String != "boom" {
		t.Fatalf("last_error = %q, want boom", got.LastError.String)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	resetJobs(t)
	var calls atomic.Int32
	job := enqueue(t, "second-time-lucky", 3)
	start(t, fastWorker("w1", map[string]Handler{"second-time-lucky": func(context.Context, store.Job) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}}))

	waitFor(t, 10*time.Second, "job done", func() bool { return getJob(t, job.ID).State == "done" })
	if got := getJob(t, job.ID); got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
}

func TestPanickingHandlerCountsAsFailure(t *testing.T) {
	resetJobs(t)
	job := enqueue(t, "panics", 1)
	start(t, fastWorker("w1", map[string]Handler{"panics": func(context.Context, store.Job) error {
		panic("kaboom")
	}}))

	waitFor(t, 10*time.Second, "job failed", func() bool { return getJob(t, job.ID).State == "failed" })
	if got := getJob(t, job.ID); got.LastError.String != "handler panicked: kaboom" {
		t.Fatalf("last_error = %q", got.LastError.String)
	}
}

// A worker died mid-job: its lease expired. Another worker reclaims the job
// without counting a failure (ADR 011).
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	resetJobs(t)
	job := enqueue(t, "build", 1)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET state = 'running', locked_by = 'dead-worker', locked_until = now() - interval '1 second' WHERE id = $1`,
		job.ID); err != nil {
		t.Fatalf("simulate dead worker: %v", err)
	}
	start(t, fastWorker("w1", map[string]Handler{"build": func(context.Context, store.Job) error { return nil }}))

	waitFor(t, 10*time.Second, "job done", func() bool { return getJob(t, job.ID).State == "done" })
	if got := getJob(t, job.ID); got.Reclaims != 1 || got.Attempts != 0 {
		t.Fatalf("reclaims=%d attempts=%d, want 1 and 0", got.Reclaims, got.Attempts)
	}
}

func TestJobReclaimedTooOftenIsAbandoned(t *testing.T) {
	resetJobs(t)
	job := enqueue(t, "crashes-worker", 1)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET state = 'running', locked_by = 'dead-worker', reclaims = 3, locked_until = now() - interval '1 second' WHERE id = $1`,
		job.ID); err != nil {
		t.Fatalf("simulate repeated crashes: %v", err)
	}
	var calls atomic.Int32
	start(t, fastWorker("w1", map[string]Handler{"crashes-worker": func(context.Context, store.Job) error {
		calls.Add(1)
		return nil
	}}))

	waitFor(t, 10*time.Second, "job failed", func() bool { return getJob(t, job.ID).State == "failed" })
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for an abandoned job, want 0", calls.Load())
	}
}

// The heartbeat keeps the lease alive past its initial duration, so a second
// worker never steals a long-running job.
func TestHeartbeatPreventsStealingLongJob(t *testing.T) {
	resetJobs(t)
	var calls atomic.Int32
	job := enqueue(t, "slow", 1)
	h := map[string]Handler{"slow": func(ctx context.Context, _ store.Job) error {
		calls.Add(1)
		select {
		case <-time.After(3 * time.Second): // longer than the 1s lease
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	w1, w2 := fastWorker("w1", h), fastWorker("w2", h)
	for _, w := range []*Worker{w1, w2} {
		w.Lease, w.Heartbeat = time.Second, 200*time.Millisecond
	}
	start(t, w1, w2)

	waitFor(t, 15*time.Second, "job done", func() bool { return getJob(t, job.ID).State == "done" })
	if got := getJob(t, job.ID); calls.Load() != 1 || got.Reclaims != 0 {
		t.Fatalf("calls=%d reclaims=%d, want 1 and 0", calls.Load(), got.Reclaims)
	}
}

// With polling effectively disabled, a job still starts promptly via LISTEN/NOTIFY.
func TestNotifyWakesIdleWorker(t *testing.T) {
	resetJobs(t)
	ran := make(chan struct{}, 1)
	w := fastWorker("w1", map[string]Handler{"ping": func(context.Context, store.Job) error {
		ran <- struct{}{}
		return nil
	}})
	w.PollInterval = time.Hour
	start(t, w)
	time.Sleep(500 * time.Millisecond) // let the worker go idle and LISTEN

	enqueue(t, "ping", 1)
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not run within 3s; NOTIFY wake-up not working")
	}
}

func TestShutdownReleasesJob(t *testing.T) {
	resetJobs(t)
	started := make(chan struct{})
	job := enqueue(t, "long", 1)
	w := fastWorker("w1", map[string]Handler{"long": func(ctx context.Context, _ store.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done

	got := getJob(t, job.ID)
	if got.State != "queued" || got.Attempts != 0 || got.Reclaims != 0 {
		t.Fatalf("after shutdown: state=%s attempts=%d reclaims=%d, want queued/0/0", got.State, got.Attempts, got.Reclaims)
	}
}

func TestWorkerOnlyClaimsItsKinds(t *testing.T) {
	resetJobs(t)
	other := enqueue(t, "not-mine", 1)
	mine := enqueue(t, "mine", 1)
	start(t, fastWorker("w1", map[string]Handler{"mine": func(context.Context, store.Job) error { return nil }}))

	waitFor(t, 10*time.Second, "own job done", func() bool { return getJob(t, mine.ID).State == "done" })
	if got := getJob(t, other.ID); got.State != "queued" {
		t.Fatalf("foreign job state = %s, want queued", got.State)
	}
}
