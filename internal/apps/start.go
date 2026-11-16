package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
)

var digestSuffixRE = regexp.MustCompile(`^@sha256:[0-9a-f]{64}$`)

// Start is the start job handler. It starts the hardened container for a
// built deploy, waits for its health check (APP-8) and then either promotes
// it to live — superseding and stopping the previous live deploy — or fails
// it, leaving any previous live deploy serving untouched.
//
// It is idempotent: a retry (e.g. after a worker crash during the health
// check) reuses the same instance row and container and checks again. The
// image must be this app's repository pinned by digest, so neither a tag
// overwritten in the registry nor a wrong row can make it run other code.
func (p *Pipeline) Start(ctx context.Context, job store.Job) error {
	id, err := deployID(job)
	if err != nil {
		return err
	}
	q := store.New(p.Pool)
	d, err := q.GetDeployForBuild(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load deploy: %w", err)
	}
	log := slog.With("deploy_id", id.String(), "app_id", d.AppID.String(), "seq", d.Seq)
	switch d.State {
	case "built":
		if n, err := q.MarkDeployStarting(ctx, id); err != nil {
			return fmt.Errorf("mark deploy starting: %w", err)
		} else if n == 0 {
			return nil
		}
	case "starting", "health_check":
		// retry: carry on with the same instance
	case "live":
		// Retry after promotion (e.g. the worker died before stopping the
		// superseded instances): finish that cleanup.
		return p.stopSuperseded(ctx, d.AppID, log)
	default:
		return nil
	}
	repo := p.Builder.Repository(d.AppID.String())
	if !d.ImageDigest.Valid || len(d.ImageDigest.String) <= len(repo) ||
		d.ImageDigest.String[:len(repo)] != repo || !digestSuffixRE.MatchString(d.ImageDigest.String[len(repo):]) {
		return fmt.Errorf("deploy %s has image %q, want %s@sha256:<digest>", id.String(), d.ImageDigest.String, repo)
	}
	var plan build.Plan
	if err := json.Unmarshal(d.ConfigSnapshot, &plan); err != nil {
		return fmt.Errorf("decode config snapshot of deploy %s: %w", id.String(), err)
	}
	if plan.Health == "" {
		return fmt.Errorf("config snapshot of deploy %s has no health path", id.String())
	}
	if err := q.SetAppStarting(ctx, d.AppID); err != nil {
		return fmt.Errorf("set app status: %w", err)
	}
	ins, err := q.GetActiveInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		ins, err = q.CreateInstance(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("get or create instance: %w", err)
	}

	started, err := p.startInstance(ctx, d.AppID.String(), id.String(), ins.ID.String(), d.ImageDigest.String, log)
	if err != nil {
		if ctx.Err() == nil && job.Attempts+1 >= job.MaxAttempts {
			log.Error("start failed for good", "error", err)
			p.abandon(ctx, d.AppID, id, ins.ID, "the app container could not be started because of an internal error; try deploying again")
		}
		return err
	}

	if n, err := q.MarkDeployHealthCheck(ctx, id); err != nil {
		return fmt.Errorf("mark deploy health_check: %w", err)
	} else if n == 0 {
		// The deploy was failed or removed meanwhile: do not leave its container running.
		p.discard(ctx, ins.ID)
		return nil
	}
	if err := p.Engine.WaitReady(ctx, started, plan.Health, p.healthTimeout()); err != nil {
		var unhealthy *runtime.UnhealthyError
		var exited *runtime.ExitedError
		if !errors.As(err, &unhealthy) && !errors.As(err, &exited) {
			return err // shutdown or lost lease: retried, and the check runs again
		}
		// The app's fault: no retry. Keep its last output for the user.
		msg := "deploy failed: " + err.Error()
		if tail, terr := p.Engine.LogTail(context.WithoutCancel(ctx), started.ContainerID, 50, 4<<10); terr == nil && tail != "" {
			msg += "\n--- last app output ---\n" + tail
		}
		log.Info("deploy failed its health check", "reason", err)
		p.abandon(ctx, d.AppID, id, ins.ID, msg)
		return nil
	}
	return p.promote(ctx, d.AppID, id, ins.ID, log)
}

func (p *Pipeline) healthTimeout() time.Duration {
	if p.HealthTimeout > 0 {
		return p.HealthTimeout
	}
	return runtime.HealthTimeout
}

// startInstance starts the container and records it on the instance row.
func (p *Pipeline) startInstance(ctx context.Context, appID, deployID, instanceID, image string, log *slog.Logger) (runtime.Instance, error) {
	started, err := p.Engine.Start(ctx, runtime.Spec{AppID: appID, DeployID: deployID, InstanceID: instanceID, Image: image})
	if err != nil {
		return runtime.Instance{}, err
	}
	var insID pgtype.UUID
	if err := insID.Scan(instanceID); err != nil {
		return runtime.Instance{}, fmt.Errorf("instance id: %w", err)
	}
	if err := store.New(p.Pool).SetInstanceContainer(ctx, store.SetInstanceContainerParams{
		ContainerID: started.ContainerID, HostPort: int32(started.HostPort), ID: insID,
	}); err != nil {
		return runtime.Instance{}, fmt.Errorf("record instance container: %w", err)
	}
	log.Info("container started", "instance_id", instanceID, "container_id", shortID(started.ContainerID),
		"host_port", started.HostPort, "runtime", p.Engine.RuntimeName())
	return started, nil
}

// promote makes the deploy live in one transaction that holds the app row:
// older live deploys become superseded first (one live deploy per app is a
// unique index), and a deploy never goes live over a newer one (newest wins).
// Superseded instances are stopped after the commit; a retried job redoes
// that cleanup, so a crash in between cannot leave them running untracked.
func (p *Pipeline) promote(ctx context.Context, appID, deployID, instanceID pgtype.UUID, log *slog.Logger) error {
	live, lost := false, false
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockAppForPromotion(ctx, appID); err != nil {
			return fmt.Errorf("lock app: %w", err)
		}
		if err := q.SupersedeOtherLiveDeploys(ctx, store.SupersedeOtherLiveDeploysParams{AppID: appID, KeepID: deployID}); err != nil {
			return fmt.Errorf("supersede previous deploys: %w", err)
		}
		n, err := q.MarkDeployLive(ctx, deployID)
		if err != nil {
			return fmt.Errorf("mark deploy live: %w", err)
		}
		if n == 0 {
			// Either a newer deploy is already live, or the deploy left
			// health_check meanwhile. In the first case it is superseded.
			m, err := q.MarkDeploySupersededIfPending(ctx, deployID)
			if err != nil {
				return fmt.Errorf("mark deploy superseded: %w", err)
			}
			lost = m > 0
			return nil
		}
		live = true
		if err := q.SetInstanceState(ctx, store.SetInstanceStateParams{State: "running", ID: instanceID}); err != nil {
			return fmt.Errorf("mark instance running: %w", err)
		}
		if err := q.SetAppLive(ctx, store.SetAppLiveParams{DeployID: deployID, ID: appID}); err != nil {
			return fmt.Errorf("set app live: %w", err)
		}
		// Delivered on commit: routers reload this app's route.
		if err := q.NotifyRouteChange(ctx, appID); err != nil {
			return fmt.Errorf("notify route change: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !live {
		if lost {
			log.Info("deploy superseded by a newer live deploy before it could go live")
		}
		p.discard(ctx, instanceID)
		return nil
	}
	log.Info("deploy live", "instance_id", instanceID.String())
	return p.stopSuperseded(ctx, appID, log)
}

// stopSuperseded stops every still-active instance of the app's superseded
// deploys. An error makes the job retry, so the cleanup eventually happens.
func (p *Pipeline) stopSuperseded(ctx context.Context, appID pgtype.UUID, log *slog.Logger) error {
	q := store.New(p.Pool)
	ids, err := q.ActiveInstancesOfSupersededDeploys(ctx, appID)
	if err != nil {
		return fmt.Errorf("list superseded instances: %w", err)
	}
	for _, oid := range ids {
		if err := p.Engine.Stop(ctx, oid.String()); err != nil {
			return fmt.Errorf("stop superseded instance %s: %w", oid.String(), err)
		}
		if err := q.SetInstanceState(ctx, store.SetInstanceStateParams{State: "stopped", ID: oid}); err != nil {
			return fmt.Errorf("mark instance %s stopped: %w", oid.String(), err)
		}
		log.Info("stopped superseded instance", "instance_id", oid.String())
	}
	return nil
}

// discard removes a started container whose deploy will not go live, and
// marks its instance stopped. The deploy itself is left as it is.
func (p *Pipeline) discard(ctx context.Context, instanceID pgtype.UUID) {
	cctx := context.WithoutCancel(ctx)
	if err := p.Engine.Remove(cctx, instanceID.String()); err != nil {
		slog.Error("remove discarded container", "instance_id", instanceID.String(), "error", err)
	}
	if err := store.New(p.Pool).SetInstanceState(cctx, store.SetInstanceStateParams{State: "stopped", ID: instanceID}); err != nil {
		slog.Error("mark discarded instance stopped", "instance_id", instanceID.String(), "error", err)
	}
}

// abandon removes the new container, fails the instance and the deploy with
// a user-facing message, and restores the app's status (running if a
// previous live deploy still serves). The previous deploy is not touched.
func (p *Pipeline) abandon(ctx context.Context, appID, deployID, instanceID pgtype.UUID, msg string) {
	cctx := context.WithoutCancel(ctx)
	q := store.New(p.Pool)
	if err := p.Engine.Remove(cctx, instanceID.String()); err != nil {
		slog.Error("remove container of failed deploy", "instance_id", instanceID.String(), "error", err)
	}
	if err := q.FailInstance(cctx, instanceID); err != nil {
		slog.Error("mark instance failed", "instance_id", instanceID.String(), "error", err)
	}
	if err := p.failDeploy(ctx, deployID, msg); err != nil {
		slog.Error("record deploy failure", "deploy_id", deployID.String(), "error", err)
	}
	if err := q.RestoreAppStatus(cctx, appID); err != nil {
		slog.Error("restore app status", "app_id", appID.String(), "error", err)
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
