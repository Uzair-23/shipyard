//go:build integration

package apps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/runtime"
)

func liveURL(t *testing.T, deploy fmt.Stringer) string {
	t.Helper()
	for _, ins := range instancesFor(t, mustUUID(t, deploy.String())) {
		if ins.state == "running" {
			return fmt.Sprintf("http://127.0.0.1:%d/", ins.hostPort)
		}
	}
	t.Fatalf("deploy %s has no running instance", deploy)
	return ""
}

func TestHealthCheckWrongPathFailsDeploy(t *testing.T) {
	f := newFixture(t, "health-404")
	f.pipeline.HealthTimeout = 3 * time.Second
	id := builtDeploy(t, f, map[string]string{"index.html": "x", "shipyard.toml": `health = "/nope.html"`})

	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start returned %v; a failed health check is the app's fault, not a job error", err)
	}
	d := getDeploy(t, id)
	if d.state != "failed" || !strings.Contains(d.errMsg, "/nope") || !strings.Contains(d.errMsg, "HTTP 404") {
		t.Fatalf("deploy = %+v", d)
	}
	if ins := instancesFor(t, id); len(ins) != 1 || ins[0].state != "failed" {
		t.Fatalf("instances = %+v", ins)
	}
	if c := containersOf(t, id); len(c) != 0 {
		t.Fatalf("container of the failed deploy still exists: %v", c)
	}
	if st, cur := appState(t, f); st != "stopped" || cur != "" {
		t.Fatalf("app status=%q current=%q, want stopped and no current deploy", st, cur)
	}
}

// A crashing app fails at once with its exit code and output, instead of
// waiting out the 30 s health timeout.
func TestHealthCheckCrashFailsFast(t *testing.T) {
	f := newFixture(t, "health-crash")
	id := builtDeploy(t, f, map[string]string{
		"requirements.txt": "",
		"app.py":           "import sys\nprint('boom: missing API key', file=sys.stderr)\nsys.exit(3)\n",
	})
	start := time.Now()
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("crash detected after %s; want well under the 30 s timeout", d)
	}
	d := getDeploy(t, id)
	if d.state != "failed" || !strings.Contains(d.errMsg, "exited with code 3") || !strings.Contains(d.errMsg, "boom: missing API key") {
		t.Fatalf("deploy = %+v", d)
	}
}

func TestRedeploySupersedesPreviousDeploy(t *testing.T) {
	f := newFixture(t, "redeploy")
	first := builtDeploy(t, f, map[string]string{"index.html": "version one"})
	if err := f.pipeline.Start(context.Background(), startJob(first)); err != nil {
		t.Fatal(err)
	}
	second := f.upload(t, 2, map[string]string{"index.html": "version two"})
	if err := f.pipeline.Build(context.Background(), buildJob(t, second)); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(second)); err != nil {
		t.Fatal(err)
	}

	if d := getDeploy(t, first); d.state != "superseded" {
		t.Fatalf("first deploy = %s, want superseded", d.state)
	}
	if d := getDeploy(t, second); d.state != "live" {
		t.Fatalf("second deploy = %+v, want live", d)
	}
	if c := containersOf(t, first); len(c) != 0 {
		t.Fatalf("old deploy's container still exists: %v", c)
	}
	if ins := instancesFor(t, first); len(ins) != 1 || ins[0].state != "stopped" {
		t.Fatalf("old instances = %+v", ins)
	}
	if st, cur := appState(t, f); st != "running" || cur != second.String() {
		t.Fatalf("app status=%q current=%q", st, cur)
	}
	if code, body := getURL(t, liveURL(t, second)); code != http.StatusOK || !strings.Contains(body, "version two") {
		t.Fatalf("GET = %d %q", code, body)
	}
}

// APP-8: a deploy that fails its health check never replaces a healthy one.
func TestFailedRedeployKeepsPreviousLive(t *testing.T) {
	f := newFixture(t, "redeploy-fails")
	f.pipeline.HealthTimeout = 3 * time.Second
	first := builtDeploy(t, f, map[string]string{"index.html": "still serving"})
	if err := f.pipeline.Start(context.Background(), startJob(first)); err != nil {
		t.Fatal(err)
	}
	url := liveURL(t, first)

	second := f.upload(t, 2, map[string]string{"index.html": "broken", "shipyard.toml": `health = "/missing.html"`})
	if err := f.pipeline.Build(context.Background(), buildJob(t, second)); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(second)); err != nil {
		t.Fatal(err)
	}

	if d := getDeploy(t, second); d.state != "failed" {
		t.Fatalf("second deploy = %s, want failed", d.state)
	}
	if d := getDeploy(t, first); d.state != "live" {
		t.Fatalf("first deploy = %s, want still live", d.state)
	}
	if st, cur := appState(t, f); st != "running" || cur != first.String() {
		t.Fatalf("app status=%q current=%q, want running on the first deploy", st, cur)
	}
	if code, body := getURL(t, url); code != http.StatusOK || !strings.Contains(body, "still serving") {
		t.Fatalf("previous deploy stopped serving: %d %q", code, body)
	}
}

// A worker that died during the health check leaves the deploy in
// health_check; the retried job reuses the container and finishes the job.
func TestStartRetryDuringHealthCheck(t *testing.T) {
	f := newFixture(t, "health-retry")
	id := builtDeploy(t, f, map[string]string{"index.html": "retry"})
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatal(err)
	}
	before := instancesFor(t, id)[0]
	// Rewind to the state a crash mid-health-check would leave behind.
	if _, err := testPool.Exec(context.Background(), `UPDATE deploys SET state = 'health_check' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE instances SET state = 'starting' WHERE deploy_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE apps SET current_deploy_id = NULL, status = 'starting' WHERE id = $1`, f.appID); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	after := instancesFor(t, id)
	if d := getDeploy(t, id); d.state != "live" || len(after) != 1 || after[0].containerID != before.containerID {
		t.Fatalf("deploy=%s instances=%+v (before %+v)", d.state, after, before)
	}
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatal(err)
	}
	return id
}

// Newest wins: a delayed older deploy can never replace a newer live one.
func TestOlderDeployCannotReplaceNewer(t *testing.T) {
	f := newFixture(t, "newest-wins")
	older := builtDeploy(t, f, map[string]string{"index.html": "old"})
	newer := f.upload(t, 2, map[string]string{"index.html": "new"})
	if err := f.pipeline.Build(context.Background(), buildJob(t, newer)); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(newer)); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(older)); err != nil {
		t.Fatalf("Start older: %v", err)
	}
	if d := getDeploy(t, older); d.state != "superseded" {
		t.Fatalf("older deploy = %s, want superseded", d.state)
	}
	if d := getDeploy(t, newer); d.state != "live" {
		t.Fatalf("newer deploy = %s, want still live", d.state)
	}
	if c := containersOf(t, older); len(c) != 0 {
		t.Fatalf("older deploy's container still exists: %v", c)
	}
	if st, cur := appState(t, f); st != "running" || cur != newer.String() {
		t.Fatalf("app status=%q current=%q, want running on the newer deploy", st, cur)
	}
}

// stopFailsOnce fails the first Stop, as if the worker crashed after the
// promotion committed but before the old container was stopped.
type stopFailsOnce struct {
	*runtime.Engine
	failed *bool
}

func (e stopFailsOnce) Stop(ctx context.Context, instanceID string) error {
	if !*e.failed {
		*e.failed = true
		return errors.New("injected stop failure")
	}
	return e.Engine.Stop(ctx, instanceID)
}

// The cleanup of superseded instances is retried until it succeeds, so an
// old container cannot keep running untracked.
func TestSupersededCleanupIsRetried(t *testing.T) {
	f := newFixture(t, "cleanup-retry")
	real := f.pipeline.Engine.(*runtime.Engine)
	first := builtDeploy(t, f, map[string]string{"index.html": "one"})
	if err := f.pipeline.Start(context.Background(), startJob(first)); err != nil {
		t.Fatal(err)
	}
	second := f.upload(t, 2, map[string]string{"index.html": "two"})
	if err := f.pipeline.Build(context.Background(), buildJob(t, second)); err != nil {
		t.Fatal(err)
	}
	failed := false
	f.pipeline.Engine = stopFailsOnce{Engine: real, failed: &failed}
	if err := f.pipeline.Start(context.Background(), startJob(second)); err == nil {
		t.Fatal("Start hid the failed cleanup; the job must retry")
	}
	if getDeploy(t, second).state != "live" || len(containersOf(t, first)) != 1 {
		t.Fatal("expected the new deploy live and the old container still running after the failed stop")
	}
	// The retried job sees the deploy live and finishes the cleanup.
	if err := f.pipeline.Start(context.Background(), startJob(second)); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	if c := containersOf(t, first); len(c) != 0 {
		t.Fatalf("old container still running after the retry: %v", c)
	}
	if ins := instancesFor(t, first); ins[0].state != "stopped" {
		t.Fatalf("old instance = %+v, want stopped", ins)
	}
}
