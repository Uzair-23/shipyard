//go:build integration

package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/store"
)

func appJob(kind string, appID pgtype.UUID) store.Job {
	payload, _ := json.Marshal(AppPayload{AppID: appID.String()})
	return store.Job{Kind: kind, AppID: appID, Payload: payload, MaxAttempts: 3}
}

func setDesired(t *testing.T, f fixture, state string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE apps SET desired_state = $1 WHERE id = $2`, state, f.appID); err != nil {
		t.Fatal(err)
	}
}

// liveApp deploys a static site and returns its deploy.
func liveApp(t *testing.T, f fixture) pgtype.UUID {
	t.Helper()
	d := builtDeploy(t, f, map[string]string{"index.html": "lifecycle test"})
	if err := f.pipeline.Start(context.Background(), startJob(d)); err != nil {
		t.Fatal(err)
	}
	if st, _ := appState(t, f); st != "running" {
		t.Fatalf("app status = %q, want running", st)
	}
	return d
}

func runningInstance(t *testing.T, d pgtype.UUID) instanceRow {
	t.Helper()
	for _, ins := range instancesFor(t, d) {
		if ins.state == "running" {
			return ins
		}
	}
	t.Fatalf("deploy %s has no running instance: %+v", d, instancesFor(t, d))
	return instanceRow{}
}

func TestStopThenLaunchRestoresApp(t *testing.T) {
	f := newFixture(t, "stop-launch")
	d := liveApp(t, f)
	first := runningInstance(t, d)

	setDesired(t, f, "stopped")
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, f.appID)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, cur := appState(t, f); st != "stopped" || cur != d.String() {
		t.Fatalf("after stop: status=%q current=%q (the deploy must stay current)", st, cur)
	}
	if c := containersOf(t, d); len(c) != 0 {
		t.Fatalf("containers after stop: %v", c)
	}
	if got := getDeploy(t, d); got.state != "live" {
		t.Fatalf("deploy state after stop = %s, want live", got.state)
	}
	// A repeated stop is harmless.
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, f.appID)); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	setDesired(t, f, "running")
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, f.appID)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if st, _ := appState(t, f); st != "running" {
		t.Fatalf("after launch: status = %q", st)
	}
	second := runningInstance(t, d)
	if second.id == first.id {
		t.Fatal("launch reused the stopped instance row")
	}
	if code, body := getURL(t, liveURL(t, d)); code != http.StatusOK || !strings.Contains(body, "lifecycle test") {
		t.Fatalf("GET after launch = %d %q", code, body)
	}

	// A duplicate launch must not start a second container.
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, f.appID)); err != nil {
		t.Fatalf("duplicate Launch: %v", err)
	}
	if c := containersOf(t, d); len(c) != 1 {
		t.Fatalf("containers after duplicate launch = %v, want 1", c)
	}
}

// A stop that is followed by a start must not undo it, and a launch that is
// followed by a stop must not run (jobs can be late or reordered).
func TestLifecycleJobsFollowDesiredState(t *testing.T) {
	f := newFixture(t, "desired-state")
	d := liveApp(t, f)

	// Stop job runs while the user wants the app running again: nothing happens.
	setDesired(t, f, "running")
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, f.appID)); err != nil {
		t.Fatal(err)
	}
	if st, _ := appState(t, f); st != "running" || len(containersOf(t, d)) != 1 {
		t.Fatalf("stale stop job stopped the app: status=%q containers=%v", st, containersOf(t, d))
	}

	// Launch job runs while the user wants the app stopped: nothing starts.
	setDesired(t, f, "stopped")
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, f.appID)); err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, f.appID)); err != nil {
		t.Fatal(err)
	}
	if st, _ := appState(t, f); st != "stopped" || len(containersOf(t, d)) != 0 {
		t.Fatalf("stale launch job started the app: status=%q containers=%v", st, containersOf(t, d))
	}
}

func TestRestartReplacesContainer(t *testing.T) {
	f := newFixture(t, "restart-app")
	d := liveApp(t, f)
	before := containersOf(t, d)
	if len(before) != 1 {
		t.Fatalf("containers = %v", before)
	}
	if err := f.pipeline.Restart(context.Background(), appJob(KindRestart, f.appID)); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	after := containersOf(t, d)
	if len(after) != 1 || after[0] == before[0] {
		t.Fatalf("containers before %v after %v: want one new container", before, after)
	}
	if st, _ := appState(t, f); st != "running" {
		t.Fatalf("status = %q", st)
	}
	if code, _ := getURL(t, liveURL(t, d)); code != http.StatusOK {
		t.Fatalf("GET after restart = %d", code)
	}
}

func TestLaunchWithoutLiveDeployOrForDeletedAppDoesNothing(t *testing.T) {
	f := newFixture(t, "launch-nothing")
	setDesired(t, f, "running")
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, f.appID)); err != nil {
		t.Fatalf("Launch with no deploy: %v", err)
	}
	if st, _ := appState(t, f); st != "stopped" {
		t.Fatalf("status = %q, want stopped", st)
	}
	// An unknown app id is not an error either (the app was removed).
	var ghost pgtype.UUID
	_ = ghost.Scan("00000000-0000-0000-0000-000000000001")
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, ghost)); err != nil {
		t.Fatalf("Launch for unknown app: %v", err)
	}
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, ghost)); err != nil {
		t.Fatalf("Stop for unknown app: %v", err)
	}
}

// An app that fails its health check on relaunch is marked crashed, not retried forever.
func TestLaunchUnhealthyAppIsMarkedCrashed(t *testing.T) {
	f := newFixture(t, "launch-crash")
	d := liveApp(t, f)
	setDesired(t, f, "stopped")
	if err := f.pipeline.Stop(context.Background(), appJob(KindStop, f.appID)); err != nil {
		t.Fatal(err)
	}
	// Make the stored health path wrong, as if the app had changed under us.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE deploys SET config_snapshot = jsonb_set(config_snapshot, '{health}', '"/nope.html"') WHERE id = $1`, d); err != nil {
		t.Fatal(err)
	}
	f.pipeline.HealthTimeout = 3_000_000_000 // 3 s
	setDesired(t, f, "running")
	if err := f.pipeline.Launch(context.Background(), appJob(KindLaunch, f.appID)); err != nil {
		t.Fatalf("Launch: %v (an unhealthy app is not a job error)", err)
	}
	if st, _ := appState(t, f); st != "crashed" {
		t.Fatalf("status = %q, want crashed", st)
	}
	if c := containersOf(t, d); len(c) != 0 {
		t.Fatalf("crashed app's container left behind: %v", c)
	}
}
