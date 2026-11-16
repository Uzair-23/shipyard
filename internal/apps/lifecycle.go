package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
)

// AppPayload is the payload of stop, launch and restart jobs.
type AppPayload struct {
	AppID string `json:"app_id"`
}

func appID(job store.Job) (pgtype.UUID, error) {
	var payload AppPayload
	var id pgtype.UUID
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return id, fmt.Errorf("decode payload: %w", err)
	}
	if err := id.Scan(payload.AppID); err != nil {
		return id, fmt.Errorf("decode app id %q: %w", payload.AppID, err)
	}
	return id, nil
}

// The lifecycle jobs make reality match apps.desired_state, which the API sets
// when the user acts. A job that no longer matches the app's desired state (a
// stop that was followed by a start, say) does nothing, so jobs that run late
// or out of order cannot undo the user's latest choice.

// Stop is the stop job handler: it removes the app's containers and marks the
// app stopped, unless the user has since asked for it to run again.
func (p *Pipeline) Stop(ctx context.Context, job store.Job) error {
	id, err := appID(job)
	if err != nil {
		return err
	}
	app, err := store.New(p.Pool).GetAppForLifecycle(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}
	if app.DesiredState != "stopped" { // deleted apps are also "stopped"
		return nil
	}
	return p.stopApp(ctx, id, "stopped")
}

// Launch is the launch job handler: it starts a new container from the app's
// live deploy (same image, health-checked) when the app should run but doesn't.
func (p *Pipeline) Launch(ctx context.Context, job store.Job) error {
	id, deploy, ok, err := p.launchable(ctx, job)
	if err != nil || !ok {
		return err
	}
	return p.bringUp(ctx, job, id, deploy)
}

// Restart is the restart job handler: stop the app's containers, then launch.
// One job, not two, so the steps cannot be reordered (Open issue 12).
func (p *Pipeline) Restart(ctx context.Context, job store.Job) error {
	id, deploy, ok, err := p.launchable(ctx, job)
	if err != nil || !ok {
		return err
	}
	if err := p.stopApp(ctx, id, "starting"); err != nil {
		return err
	}
	return p.bringUp(ctx, job, id, deploy)
}

// launchable loads the app and reports whether it should be running from a live deploy.
func (p *Pipeline) launchable(ctx context.Context, job store.Job) (app, deploy pgtype.UUID, ok bool, err error) {
	app, err = appID(job)
	if err != nil {
		return app, deploy, false, err
	}
	row, err := store.New(p.Pool).GetAppForLifecycle(ctx, app)
	if errors.Is(err, pgx.ErrNoRows) {
		return app, deploy, false, nil
	}
	if err != nil {
		return app, deploy, false, fmt.Errorf("load app: %w", err)
	}
	if row.DeletedAt.Valid || row.DesiredState != "running" || !row.CurrentDeployID.Valid {
		return app, deploy, false, nil
	}
	return app, row.CurrentDeployID, true, nil
}

// stopApp stops and removes every active container of the app, then records
// status (and tells routers) in one transaction. Safe to repeat.
func (p *Pipeline) stopApp(ctx context.Context, appID pgtype.UUID, status string) error {
	q := store.New(p.Pool)
	ids, err := q.ActiveInstancesOfApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	for _, id := range ids {
		if err := p.Engine.Stop(ctx, id.String()); err != nil {
			return fmt.Errorf("stop instance %s: %w", id.String(), err)
		}
		if err := q.SetInstanceState(ctx, store.SetInstanceStateParams{State: "stopped", ID: id}); err != nil {
			return fmt.Errorf("mark instance %s stopped: %w", id.String(), err)
		}
	}
	return p.setStatus(ctx, appID, status)
}

// setStatus updates the app's status and notifies routers in one transaction.
func (p *Pipeline) setStatus(ctx context.Context, appID pgtype.UUID, status string) error {
	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.SetAppStatus(ctx, store.SetAppStatusParams{Status: status, ID: appID}); err != nil {
			return fmt.Errorf("set app status: %w", err)
		}
		return q.NotifyRouteChange(ctx, appID)
	})
}

// bringUp runs the app's live deploy in a new container and marks the app
// running once it is healthy. An unhealthy or exiting app is the app's fault:
// it is marked crashed, with no retry.
func (p *Pipeline) bringUp(ctx context.Context, job store.Job, appID, deployID pgtype.UUID) error {
	q := store.New(p.Pool)
	log := slog.With("app_id", appID.String(), "deploy_id", deployID.String(), "job_id", job.ID.String())
	d, err := q.GetDeployForBuild(ctx, deployID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && d.State != "live") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load deploy: %w", err)
	}
	image, plan, err := p.runInfo(d)
	if err != nil {
		return err
	}
	ins, err := q.GetActiveInstance(ctx, deployID)
	if err == nil && ins.State == "running" {
		return p.setStatus(ctx, appID, "running") // already up: a duplicate or retried job
	}
	if errors.Is(err, pgx.ErrNoRows) {
		ins, err = q.CreateInstance(ctx, deployID)
	}
	if err != nil {
		return fmt.Errorf("get or create instance: %w", err)
	}
	if err := q.SetAppStatus(ctx, store.SetAppStatusParams{Status: "starting", ID: appID}); err != nil {
		return fmt.Errorf("set app status: %w", err)
	}

	env, err := p.userEnv(ctx, appID)
	if err != nil {
		return err
	}
	started, err := p.startInstance(ctx, appID.String(), deployID.String(), ins.ID.String(), image, env, log)
	var exitedAtStart *runtime.ExitedError
	if errors.As(err, &exitedAtStart) {
		log.Info("app exited at start", "reason", err)
		p.markCrashed(ctx, appID, ins.ID)
		return nil
	}
	if err != nil {
		if ctx.Err() == nil && job.Attempts+1 >= job.MaxAttempts {
			log.Error("launch failed for good", "error", err)
			p.markCrashed(ctx, appID, ins.ID)
		}
		return err
	}
	if err := p.Engine.WaitReady(ctx, started, plan.Health, p.healthTimeout()); err != nil {
		var unhealthy *runtime.UnhealthyError
		var exited *runtime.ExitedError
		if !errors.As(err, &unhealthy) && !errors.As(err, &exited) {
			return err // shutdown or lost lease: retried
		}
		log.Info("app did not become healthy", "reason", err)
		p.markCrashed(ctx, appID, ins.ID)
		return nil
	}
	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if err := tq.SetInstanceState(ctx, store.SetInstanceStateParams{State: "running", ID: ins.ID}); err != nil {
			return fmt.Errorf("mark instance running: %w", err)
		}
		if err := tq.SetAppStatus(ctx, store.SetAppStatusParams{Status: "running", ID: appID}); err != nil {
			return fmt.Errorf("set app status: %w", err)
		}
		return tq.NotifyRouteChange(ctx, appID)
	})
}

// markCrashed removes the app's new container and records that it failed to run.
func (p *Pipeline) markCrashed(ctx context.Context, appID, instanceID pgtype.UUID) {
	cctx := context.WithoutCancel(ctx)
	if err := p.Engine.Remove(cctx, instanceID.String()); err != nil {
		slog.Error("remove container of crashed app", "instance_id", instanceID.String(), "error", err)
	}
	if err := store.New(p.Pool).FailInstance(cctx, instanceID); err != nil {
		slog.Error("mark instance failed", "instance_id", instanceID.String(), "error", err)
	}
	if err := p.setStatus(cctx, appID, "crashed"); err != nil {
		slog.Error("mark app crashed", "app_id", appID.String(), "error", err)
	}
}
