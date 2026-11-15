package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
)

// Handler runs one job. Handlers must be idempotent: after a worker crash the
// same job runs again on another worker (ADR 002, ADR 011).
type Handler func(ctx context.Context, job store.Job) error

// Worker claims jobs for the kinds in Handlers and runs them.
// Zero-valued tuning fields get the ADR 011 defaults.
type Worker struct {
	Pool     *pgxpool.Pool
	Handlers map[string]Handler

	ID           string        // default: hostname-pid
	Concurrency  int           // default: 4
	Lease        time.Duration // default: 60s
	Heartbeat    time.Duration // default: 20s
	PollInterval time.Duration // default: 2s; fallback when no NOTIFY arrives
	MaxReclaims  int32         // default: 3; more lost leases than this fails the job
	// Backoff returns the delay before retrying after the given number of failures.
	Backoff func(failures int32) time.Duration // default: 2s doubling, max 5m
}

func (w *Worker) setDefaults() {
	if w.ID == "" {
		host, _ := os.Hostname()
		w.ID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if w.Concurrency == 0 {
		w.Concurrency = 4
	}
	if w.Lease == 0 {
		w.Lease = 60 * time.Second
	}
	if w.Heartbeat == 0 {
		w.Heartbeat = 20 * time.Second
	}
	if w.PollInterval == 0 {
		w.PollInterval = 2 * time.Second
	}
	if w.MaxReclaims == 0 {
		w.MaxReclaims = 3
	}
	if w.Backoff == nil {
		w.Backoff = func(failures int32) time.Duration {
			d := 2 * time.Second << min(failures-1, 8)
			return min(d, 5*time.Minute)
		}
	}
}

// Run processes jobs until ctx is cancelled. In-flight jobs are released back
// to the queue on shutdown.
func (w *Worker) Run(ctx context.Context) {
	w.setDefaults()
	kinds := make([]string, 0, len(w.Handlers))
	for k := range w.Handlers {
		kinds = append(kinds, k)
	}
	slog.Info("worker started", "worker_id", w.ID, "kinds", kinds, "concurrency", w.Concurrency)

	wake := make(chan struct{}, w.Concurrency)
	var wg sync.WaitGroup
	wg.Add(1 + w.Concurrency)
	go func() { defer wg.Done(); w.listen(ctx, wake) }()
	for range w.Concurrency {
		go func() { defer wg.Done(); w.loop(ctx, kinds, wake) }()
	}
	wg.Wait()
	slog.Info("worker stopped", "worker_id", w.ID)
}

func (w *Worker) loop(ctx context.Context, kinds []string, wake <-chan struct{}) {
	q := store.New(w.Pool)
	for ctx.Err() == nil {
		job, err := q.ClaimJob(ctx, store.ClaimJobParams{
			WorkerID: w.ID, LeaseSecs: w.Lease.Seconds(), Kinds: kinds,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			w.idle(ctx, wake)
		case err != nil:
			if ctx.Err() == nil {
				slog.Error("claim job", "worker_id", w.ID, "error", err)
				w.idle(ctx, wake)
			}
		default:
			w.process(ctx, job)
		}
	}
}

func (w *Worker) idle(ctx context.Context, wake <-chan struct{}) {
	t := time.NewTimer(w.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-t.C:
	}
}

func (w *Worker) process(ctx context.Context, job store.Job) {
	log := slog.With("job_id", job.ID.String(), "kind", job.Kind, "worker_id", w.ID)
	q := store.New(w.Pool)

	// Writes after the handler use a context that survives shutdown.
	finish := func(op string, f func(context.Context) (int64, error)) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		n, err := f(fctx)
		switch {
		case err != nil:
			log.Error("job "+op, "error", err)
		case n == 0:
			log.Warn("job " + op + " skipped: lease lost to another worker")
		}
	}

	if job.Reclaims > w.MaxReclaims {
		msg := fmt.Sprintf("worker lost this job %d times; giving up", job.Reclaims)
		log.Error("job abandoned", "reclaims", job.Reclaims)
		finish("abandon", func(c context.Context) (int64, error) {
			return q.FailJobPermanently(c, store.FailJobPermanentlyParams{LastError: msg, ID: job.ID, WorkerID: w.ID})
		})
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); w.heartbeat(jobCtx, cancel, job.ID, log) }()

	start := time.Now()
	err := runHandler(jobCtx, w.Handlers[job.Kind], job)
	cancel()
	<-hbDone

	switch {
	case ctx.Err() != nil:
		log.Info("worker shutting down; releasing job")
		finish("release", func(c context.Context) (int64, error) {
			return q.ReleaseJob(c, store.ReleaseJobParams{ID: job.ID, WorkerID: w.ID})
		})
	case err == nil:
		log.Info("job done", "duration_ms", time.Since(start).Milliseconds())
		finish("complete", func(c context.Context) (int64, error) {
			return q.CompleteJob(c, store.CompleteJobParams{ID: job.ID, WorkerID: w.ID})
		})
	default:
		failures := job.Attempts + 1
		backoff := w.Backoff(failures)
		log.Warn("job failed", "error", err, "failures", failures, "max_attempts", job.MaxAttempts)
		finish("fail", func(c context.Context) (int64, error) {
			return q.FailJob(c, store.FailJobParams{
				LastError: err.Error(), BackoffSecs: backoff.Seconds(), ID: job.ID, WorkerID: w.ID,
			})
		})
	}
}

// heartbeat extends the job's lease until ctx ends. If the lease was lost
// (another worker reclaimed the job) it cancels the handler.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, id pgtype.UUID, log *slog.Logger) {
	q := store.New(w.Pool)
	t := time.NewTicker(w.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := q.ExtendJobLease(ctx, store.ExtendJobLeaseParams{
				LeaseSecs: w.Lease.Seconds(), ID: id, WorkerID: w.ID,
			})
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("extend lease", "error", err) // transient; retry on next tick
				}
				continue
			}
			if n == 0 {
				log.Warn("lease lost; cancelling handler")
				cancel()
				return
			}
		}
	}
}

func runHandler(ctx context.Context, h Handler, job store.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return h(ctx, job)
}

// listen wakes idle loops when a job is enqueued. Polling covers gaps while
// the LISTEN connection is down.
func (w *Worker) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		err := w.listenOnce(ctx, wake)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listen jobs_new failed; relying on polling", "worker_id", w.ID, "error", err)
		t := time.NewTimer(w.PollInterval)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
}

func (w *Worker) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := pgx.ConnectConfig(ctx, w.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, "LISTEN jobs_new"); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		for range cap(wake) {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}
