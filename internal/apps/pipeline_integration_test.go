//go:build integration

package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var (
	testPool     *pgxpool.Pool
	testDocker   *client.Client
	testRegistry string
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	url, stopDB, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stopDB()
	if err := store.Migrate(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, url+"&pool_max_conns=24"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()
	var stopRegistry func()
	if testRegistry, stopRegistry, err = testdb.StartRegistry(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stopRegistry()
	if testDocker, err = client.New(client.FromEnv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = testDocker.Close() }()
	return m.Run()
}

type fixture struct {
	pipeline *Pipeline
	appID    pgtype.UUID
}

func newFixture(t *testing.T, slug string) fixture {
	t.Helper()
	f := fixture{pipeline: &Pipeline{
		Pool:    testPool,
		Builder: &build.Builder{Docker: testDocker, Registry: testRegistry},
		DataDir: t.TempDir(),
	}}
	if err := testPool.QueryRow(context.Background(), `INSERT INTO apps (slug) VALUES ($1) RETURNING id`, slug).Scan(&f.appID); err != nil {
		t.Fatalf("create app: %v", err)
	}
	t.Cleanup(func() {
		imgs, _ := testDocker.ImageList(context.Background(), client.ImageListOptions{
			Filters: make(client.Filters).Add("reference", f.pipeline.Builder.Repository(f.appID.String())+"*"),
		})
		for _, img := range imgs.Items {
			_, _ = testDocker.ImageRemove(context.Background(), img.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		}
	})
	return f
}

// upload normalises files into the data dir the way the API will, and
// creates a queued deploy pointing at it.
func (f fixture) upload(t *testing.T, seq int, files map[string]string) pgtype.UUID {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()

	tmp := filepath.Join(t.TempDir(), "upload.tar.gz")
	if err := build.NormalizeFile(context.Background(), &raw, tmp, build.DefaultLimits); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	ref := "sha256:" + hex.EncodeToString(sum[:])
	rel, err := SourceRelPath(f.appID, ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.pipeline.DataDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO deploys (app_id, seq, source_ref) VALUES ($1, $2, $3) RETURNING id`, f.appID, seq, ref).Scan(&id); err != nil {
		t.Fatalf("create deploy: %v", err)
	}
	return id
}

func buildJob(t *testing.T, deploy pgtype.UUID) store.Job {
	t.Helper()
	payload, _ := json.Marshal(DeployPayload{DeployID: deploy.String()})
	return store.Job{Kind: KindBuild, Payload: payload}
}

type deployRow struct {
	state, digest, errMsg string
	snapshot              []byte
}

func getDeploy(t *testing.T, id pgtype.UUID) deployRow {
	t.Helper()
	var d deployRow
	var digest, errMsg pgtype.Text
	if err := testPool.QueryRow(context.Background(),
		`SELECT state, image_digest, error, config_snapshot FROM deploys WHERE id = $1`, id).
		Scan(&d.state, &digest, &errMsg, &d.snapshot); err != nil {
		t.Fatalf("get deploy: %v", err)
	}
	d.digest, d.errMsg = digest.String, errMsg.String
	return d
}

func startJobs(t *testing.T, deploy pgtype.UUID) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'start' AND payload->>'deploy_id' = $1`, deploy.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBuildStaticDeploy(t *testing.T) {
	f := newFixture(t, "static-build")
	id := f.upload(t, 1, map[string]string{"index.html": "<h1>hello</h1>", "shipyard.toml": `health = "/index.html"`})

	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	d := getDeploy(t, id)
	if d.state != "built" || !strings.HasPrefix(d.digest, f.pipeline.Builder.Repository(f.appID.String())+"@sha256:") {
		t.Fatalf("deploy = %+v", d)
	}
	var plan build.Plan
	if err := json.Unmarshal(d.snapshot, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Runtime != build.Static || plan.Health != "/index.html" || plan.IdleTimeout != build.DefaultIdle {
		t.Fatalf("snapshot = %+v", plan)
	}
	if n := startJobs(t, id); n != 1 {
		t.Fatalf("%d start jobs enqueued, want 1", n)
	}

	// Idempotent: running the job again changes nothing.
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if n := startJobs(t, id); n != 1 {
		t.Fatalf("%d start jobs after re-run, want 1", n)
	}
}

func TestBuildUserErrorFailsDeploy(t *testing.T) {
	f := newFixture(t, "no-runtime")
	id := f.upload(t, 1, map[string]string{"main.go": "package main"})

	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatalf("Build returned %v; user errors should not fail the job", err)
	}
	d := getDeploy(t, id)
	if d.state != "failed" || !strings.Contains(d.errMsg, "could not detect the app's runtime") {
		t.Fatalf("deploy = %+v", d)
	}
	if startJobs(t, id) != 0 {
		t.Fatal("start job enqueued for a failed build")
	}
}

func TestBuildFailingStepFailsDeploy(t *testing.T) {
	f := newFixture(t, "bad-deps")
	id := f.upload(t, 1, map[string]string{
		"requirements.txt": "this-package-does-not-exist-shipyard-test==0.0.0\n",
		"app.py":           "print('hi')",
	})
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	d := getDeploy(t, id)
	if d.state != "failed" || !strings.Contains(d.errMsg, "build failed") || !strings.Contains(d.errMsg, "this-package-does-not-exist") {
		t.Fatalf("deploy = %+v", d)
	}
}

func TestBuildMissingSourceIsInternalError(t *testing.T) {
	f := newFixture(t, "missing-source")
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO deploys (app_id, seq, source_ref) VALUES ($1, 1, $2) RETURNING id`, f.appID, "sha256:"+strings.Repeat("0", 64)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err == nil {
		t.Fatal("expected an error for a missing source file")
	}
	if d := getDeploy(t, id); d.state != "failed" || !strings.Contains(d.errMsg, "internal error") {
		t.Fatalf("deploy = %+v", d)
	}
}

// End to end through the real job worker.
func TestBuildViaWorker(t *testing.T) {
	f := newFixture(t, "via-worker")
	id := f.upload(t, 1, map[string]string{"index.html": "<p>queued</p>"})
	payload, _ := json.Marshal(DeployPayload{DeployID: id.String()})
	if _, err := jobs.Enqueue(context.Background(), testPool, store.EnqueueJobParams{Kind: KindBuild, AppID: f.appID, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	w := &jobs.Worker{Pool: testPool, Handlers: f.pipeline.Handlers(), ID: "test-worker", PollInterval: 200 * time.Millisecond}
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Minute)
	for getDeploy(t, id).state != "built" {
		if time.Now().After(deadline) {
			t.Fatalf("deploy not built in time: %+v", getDeploy(t, id))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// os.OpenInRoot: even if a symlink is planted inside the data dir, the
// pipeline cannot be made to read outside it.
func TestBuildDoesNotFollowSymlinkOutOfDataDir(t *testing.T) {
	f := newFixture(t, "symlink-escape")
	outside := filepath.Join(t.TempDir(), "secret.tar.gz")
	if err := os.WriteFile(outside, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := "sha256:" + strings.Repeat("1", 64)
	rel, _ := SourceRelPath(f.appID, ref)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.pipeline.DataDir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.pipeline.DataDir, rel)); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO deploys (app_id, seq, source_ref) VALUES ($1, 1, $2) RETURNING id`, f.appID, ref).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err == nil {
		t.Fatal("pipeline followed a symlink out of the data dir")
	}
}

func TestBuildRejectsSourceWithWrongHash(t *testing.T) {
	f := newFixture(t, "wrong-hash")
	id := f.upload(t, 1, map[string]string{"index.html": "x"})
	var ref string
	if err := testPool.QueryRow(context.Background(), `SELECT source_ref FROM deploys WHERE id = $1`, id).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	rel, _ := SourceRelPath(f.appID, ref)
	if err := os.WriteFile(filepath.Join(f.pipeline.DataDir, rel), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err == nil || !strings.Contains(err.Error(), "does not match its hash") {
		t.Fatalf("Build = %v, want hash mismatch", err)
	}
	if d := getDeploy(t, id); d.state != "failed" || !strings.Contains(d.errMsg, "internal error") {
		t.Fatalf("deploy = %+v", d)
	}
}

func TestFailDeployStoresSanitisedMessage(t *testing.T) {
	f := newFixture(t, "dirty-error")
	id := f.upload(t, 1, map[string]string{"index.html": "x"})
	if err := f.pipeline.failDeploy(context.Background(), id, "boom\x00 \x1b]52;c;eA==\x07 \xff done"); err != nil {
		t.Fatalf("failDeploy: %v", err)
	}
	d := getDeploy(t, id)
	if d.state != "failed" || strings.ContainsAny(d.errMsg, "\x00\x1b\x07") || !strings.Contains(d.errMsg, "done") {
		t.Fatalf("deploy = %+v", d)
	}
}
