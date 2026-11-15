// Package jobs implements the Postgres-backed job queue (ADR 002) and the
// worker that runs job handlers with leases (ADR 011).
package jobs

import (
	"context"
	"fmt"

	"github.com/Uzair-23/shipyard/internal/store"
)

// Enqueue inserts a job and notifies workers on the jobs_new channel.
//
// Pass the transaction that changes the related state as db, so the job and
// the state change commit together; the notification is delivered on commit.
func Enqueue(ctx context.Context, db store.DBTX, p store.EnqueueJobParams) (store.Job, error) {
	if p.Payload == nil {
		p.Payload = []byte("{}")
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 1
	}
	q := store.New(db)
	job, err := q.EnqueueJob(ctx, p)
	if err != nil {
		return store.Job{}, fmt.Errorf("enqueue %s job: %w", p.Kind, err)
	}
	if err := q.NotifyJobsNew(ctx); err != nil {
		return store.Job{}, fmt.Errorf("notify jobs_new: %w", err)
	}
	return job, nil
}
