package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/store"
)

// ReaperInterval is how often the worker looks for idle apps.
const ReaperInterval = 30 * time.Second

// EnqueueAppJob enqueues a lifecycle job (stop, launch, restart, sleep) for an app.
func EnqueueAppJob(ctx context.Context, db store.DBTX, kind string, appID pgtype.UUID) error {
	payload, err := json.Marshal(AppPayload{AppID: appID.String()})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	_, err = jobs.Enqueue(ctx, db, store.EnqueueJobParams{Kind: kind, AppID: appID, Payload: payload, MaxAttempts: 3})
	return err
}

// RunReaper puts idle apps to sleep until ctx is cancelled: every interval it
// enqueues one sleep job per running app that has had no traffic for its
// idle timeout. The sleep job itself decides again, so a request that arrives
// in between keeps the app up.
func (p *Pipeline) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.reapOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("idle reaper failed", "error", err)
			}
		}
	}
}

func (p *Pipeline) reapOnce(ctx context.Context) error {
	ids, err := store.New(p.Pool).ListIdleApps(ctx)
	if err != nil {
		return fmt.Errorf("list idle apps: %w", err)
	}
	for _, id := range ids {
		if err := EnqueueAppJob(ctx, p.Pool, KindSleep, id); err != nil {
			return fmt.Errorf("enqueue sleep for app %s: %w", id.String(), err)
		}
		slog.Info("app is idle; sleeping it", "app_id", id.String())
	}
	return nil
}

// Sleep is the sleep job handler: it stops an idle app's container but keeps
// the deploy live and desired_state "running", so the router can wake it with a
// launch job. It does nothing unless the app is still running and still idle
// (a stop, restart or fresh request since the job was queued wins).
func (p *Pipeline) Sleep(ctx context.Context, job store.Job) error {
	id, err := appID(job)
	if err != nil {
		return err
	}
	idle, err := store.New(p.Pool).IsAppIdle(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check idle: %w", err)
	}
	if !idle {
		return nil
	}
	return p.stopApp(ctx, id, "sleeping")
}
