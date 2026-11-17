// Package apps holds the deploy pipeline: the job handlers that turn an
// uploaded source into a built image and a running container.
package apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
)

// Job kinds handled by the pipeline.
const (
	KindBuild   = "build"
	KindStart   = "start"
	KindStop    = "stop"    // stop an app's containers (user stop, delete)
	KindLaunch  = "launch"  // start a stopped app from its live deploy
	KindRestart = "restart" // stop, then launch
	KindSleep   = "sleep"   // stop an idle app but keep it wanted; the router wakes it with launch
)

// DeployPayload is the payload of build and start jobs.
type DeployPayload struct {
	DeployID string `json:"deploy_id"`
}

var sourceRefRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// SourceRelPath is where, relative to the data dir, the normalised archive
// for sourceRef ("sha256:<hex>") of an app is stored. sourceRef is validated,
// and the pipeline opens it with os.OpenInRoot, so it cannot leave the data dir.
func SourceRelPath(appID pgtype.UUID, sourceRef string) (string, error) {
	m := sourceRefRE.FindStringSubmatch(sourceRef)
	if m == nil || !appID.Valid {
		return "", fmt.Errorf("invalid source ref %q", sourceRef)
	}
	return filepath.Join("sources", appID.String(), m[1]+".tar.gz"), nil
}

// containerEngine is the part of *runtime.Engine the pipeline uses; tests
// wrap the real engine to inject failures.
type containerEngine interface {
	Start(ctx context.Context, spec runtime.Spec) (runtime.Instance, error)
	WaitReady(ctx context.Context, ins runtime.Instance, healthPath string, timeout time.Duration) error
	LogTail(ctx context.Context, containerID string, lines, maxBytes int) (string, error)
	Stop(ctx context.Context, instanceID string) error
	Remove(ctx context.Context, instanceID string) error
	RuntimeName() string
}

// Pipeline runs the deploy job handlers.
type Pipeline struct {
	Pool    *pgxpool.Pool
	Builder *build.Builder
	Engine  containerEngine // normally *runtime.Engine
	DataDir string
	// MasterKey decrypts the apps' environment variables (see internal/secrets).
	MasterKey []byte

	HealthTimeout time.Duration // default runtime.HealthTimeout (30s)
}

// Handlers returns the job handlers to register with a jobs.Worker.
func (p *Pipeline) Handlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{
		KindBuild: p.Build, KindStart: p.Start,
		KindStop: p.Stop, KindLaunch: p.Launch, KindRestart: p.Restart, KindSleep: p.Sleep,
	}
}

func deployID(job store.Job) (pgtype.UUID, error) {
	var payload DeployPayload
	var id pgtype.UUID
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return id, fmt.Errorf("decode payload: %w", err)
	}
	if err := id.Scan(payload.DeployID); err != nil {
		return id, fmt.Errorf("decode deploy id %q: %w", payload.DeployID, err)
	}
	return id, nil
}

// Build is the build job handler: it plans the build from the stored source,
// builds and pushes the image, records the digest and config snapshot, and
// enqueues the start job in the same transaction. It is idempotent: a deploy
// that is already built (or failed) is left alone, and a deploy left in
// "building" by a crashed worker is rebuilt.
func (p *Pipeline) Build(ctx context.Context, job store.Job) error {
	id, err := deployID(job)
	if err != nil {
		return err
	}
	q := store.New(p.Pool)
	d, err := q.GetDeployForBuild(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deploy deleted (app removed); nothing to do
	}
	if err != nil {
		return fmt.Errorf("load deploy: %w", err)
	}
	if d.State != "queued" && d.State != "building" {
		return nil
	}
	log := slog.With("deploy_id", id.String(), "app_id", d.AppID.String(), "seq", d.Seq)
	if n, err := q.MarkDeployBuilding(ctx, id); err != nil {
		return fmt.Errorf("mark deploy building: %w", err)
	} else if n == 0 {
		return nil // state changed since we loaded it
	}

	sink := newLogSink(p.Pool, id)
	stopLogs := sink.Start(ctx)
	sink.Add("system", "Build started")
	digest, plan, err := p.build(ctx, d, sink.Build)
	stopLogs() // everything the build printed is stored before any state change
	var be *build.BuildError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return err // worker shutting down or lease lost: the job is retried
	case errors.As(err, &be):
		log.Info("build failed", "reason", be.Reason) // user output stays out of platform logs
		return p.failDeploy(ctx, id, err.Error())
	case isUserError(err):
		log.Info("build rejected", "reason", err)
		return p.failDeploy(ctx, id, err.Error())
	default:
		// Infrastructure failure: record it for the user and fail the job.
		if ferr := p.failDeploy(ctx, id, "build failed because of an internal error; try deploying again"); ferr != nil {
			log.Error("record build failure", "error", ferr)
		}
		return err
	}

	snapshot, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode config snapshot: %w", err)
	}
	payload, err := json.Marshal(DeployPayload{DeployID: id.String()})
	if err != nil {
		return fmt.Errorf("encode start payload: %w", err)
	}
	err = pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		n, err := store.New(tx).MarkDeployBuilt(ctx, store.MarkDeployBuiltParams{
			ImageDigest: digest, ConfigSnapshot: snapshot, ID: id,
		})
		if err != nil {
			return fmt.Errorf("mark deploy built: %w", err)
		}
		if n == 0 {
			return nil // state changed meanwhile (e.g. deleted); do not start it
		}
		if err := logSystem(ctx, tx, id, "Build finished"); err != nil {
			return err
		}
		_, err = jobs.Enqueue(ctx, tx, store.EnqueueJobParams{
			Kind: KindStart, AppID: d.AppID, Payload: payload, MaxAttempts: 3,
		})
		return err
	})
	if err != nil {
		return err
	}
	log.Info("deploy built", "image", digest, "runtime", plan.Runtime)
	return nil
}

func (p *Pipeline) build(ctx context.Context, d store.GetDeployForBuildRow, onLine func(string)) (string, build.Plan, error) {
	rel, err := SourceRelPath(d.AppID, d.SourceRef)
	if err != nil {
		return "", build.Plan{}, err
	}
	f, err := os.OpenInRoot(p.DataDir, rel)
	if err != nil {
		return "", build.Plan{}, fmt.Errorf("open source: %w", err)
	}
	// The file name is its content hash: verify it, so a tampered or
	// corrupted source is never built.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return "", build.Plan{}, fmt.Errorf("hash source: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.SourceRef {
		_ = f.Close()
		return "", build.Plan{}, fmt.Errorf("source %s does not match its hash (got %s)", rel, got)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return "", build.Plan{}, fmt.Errorf("rewind source: %w", err)
	}
	src, config, err := build.ReadSource(f)
	_ = f.Close()
	if err != nil {
		return "", build.Plan{}, err
	}
	plan, err := build.Prepare(src, config, d.AppID.String())
	if err != nil {
		return "", build.Plan{}, userError{err}
	}

	// Stream the build context: the normalised archive re-packed under src/.
	f, err = os.OpenInRoot(p.DataDir, rel)
	if err != nil {
		return "", build.Plan{}, fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = f.Close() }()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(build.WriteContext(pw, f, plan.Dockerfile)) }()
	defer func() { _ = pr.Close() }()

	digest, err := p.Builder.BuildLogged(ctx, pr, d.AppID.String(), strconv.Itoa(int(d.Seq)), onLine)
	return digest, plan, err
}

// failDeploy records a user-visible failure. The message is sanitised (it
// may contain build output); if storing it still fails, a fixed message is
// stored so the deploy never stays stuck in "building".
func (p *Pipeline) failDeploy(ctx context.Context, id pgtype.UUID, msg string) error {
	wctx := context.WithoutCancel(ctx)
	fail := func(text string) error {
		return pgx.BeginFunc(wctx, p.Pool, func(tx pgx.Tx) error {
			n, err := store.New(tx).FailDeploy(wctx, store.FailDeployParams{Error: build.Sanitize(text), ID: id})
			if err != nil || n == 0 {
				return err
			}
			// The reason goes into the log in the same transaction as the state change, so a
			// client following the log sees it before it sees the deploy has ended.
			return logSystem(wctx, tx, id, firstLine(build.Sanitize(text)))
		})
	}
	err := fail(msg)
	if err == nil {
		return nil
	}
	slog.Error("record deploy failure; storing a generic message", "deploy_id", id.String(), "error", err)
	if err := fail("build failed; the build output could not be stored"); err != nil {
		return fmt.Errorf("mark deploy failed: %w", err)
	}
	return nil
}

// userError marks errors caused by the user's source or config; their
// messages are shown to the user and the job is not retried.
type userError struct{ error }

func (e userError) Unwrap() error { return e.error }

func isUserError(err error) bool {
	var ue userError
	return errors.As(err, &ue)
}
