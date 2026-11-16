package apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
)

var digestSuffixRE = regexp.MustCompile(`^@sha256:[0-9a-f]{64}$`)

// Start is the start job handler: it creates and starts the hardened
// container for a built deploy and records the instance. The deploy stays in
// "starting"; the health check (next M1 task) marks it live or failed.
//
// It is idempotent: a retry reuses the same instance row and container. The
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
	if d.State != "built" && d.State != "starting" {
		return nil
	}
	repo := p.Builder.Repository(d.AppID.String())
	if !d.ImageDigest.Valid || len(d.ImageDigest.String) <= len(repo) ||
		d.ImageDigest.String[:len(repo)] != repo || !digestSuffixRE.MatchString(d.ImageDigest.String[len(repo):]) {
		return fmt.Errorf("deploy %s has image %q, want %s@sha256:<digest>", id.String(), d.ImageDigest.String, repo)
	}
	log := slog.With("deploy_id", id.String(), "app_id", d.AppID.String(), "seq", d.Seq)

	if n, err := q.MarkDeployStarting(ctx, id); err != nil {
		return fmt.Errorf("mark deploy starting: %w", err)
	} else if n == 0 {
		return nil
	}
	if err := q.SetAppStatus(ctx, store.SetAppStatusParams{Status: "starting", ID: d.AppID}); err != nil {
		return fmt.Errorf("set app status: %w", err)
	}

	ins, err := q.GetActiveInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		ins, err = q.CreateInstance(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("get or create instance: %w", err)
	}

	err = p.startInstance(ctx, d.AppID.String(), id.String(), ins.ID.String(), d.ImageDigest.String, log)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && job.Attempts+1 >= job.MaxAttempts {
		// Out of retries: remove the container so nothing runs untracked,
		// and tell the user; details stay in platform logs.
		log.Error("start failed for good", "error", err)
		cctx := context.WithoutCancel(ctx)
		if rerr := p.Engine.Remove(cctx, ins.ID.String()); rerr != nil {
			log.Error("remove container after failed start", "error", rerr)
		}
		if ferr := q.FailInstance(cctx, ins.ID); ferr != nil {
			log.Error("mark instance failed", "error", ferr)
		}
		if ferr := p.failDeploy(ctx, id, "the app container could not be started because of an internal error; try deploying again"); ferr != nil {
			log.Error("record start failure", "error", ferr)
		}
	}
	return err
}

// startInstance starts the container and records it on the instance row.
func (p *Pipeline) startInstance(ctx context.Context, appID, deployID, instanceID, image string, log *slog.Logger) error {
	started, err := p.Engine.Start(ctx, runtime.Spec{AppID: appID, DeployID: deployID, InstanceID: instanceID, Image: image})
	if err != nil {
		return err
	}
	insID := new(pgtype.UUID)
	if err := insID.Scan(instanceID); err != nil {
		return fmt.Errorf("instance id: %w", err)
	}
	if err := store.New(p.Pool).SetInstanceContainer(ctx, store.SetInstanceContainerParams{
		ContainerID: started.ContainerID, HostPort: int32(started.HostPort), ID: *insID,
	}); err != nil {
		return fmt.Errorf("record instance container: %w", err)
	}
	log.Info("container started", "instance_id", instanceID, "container_id", shortID(started.ContainerID),
		"host_port", started.HostPort, "runtime", p.Engine.RuntimeName())
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
