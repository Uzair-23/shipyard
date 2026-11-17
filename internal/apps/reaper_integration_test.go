//go:build integration

package apps

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// makeIdle pretends the app's last traffic and its container's start were two hours ago.
func makeIdle(t *testing.T, f fixture, d pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE apps SET last_request_at = now() - interval '2 hours' WHERE id = $1`, f.appID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE instances SET created_at = now() - interval '2 hours' WHERE deploy_id = $1`, d); err != nil {
		t.Fatal(err)
	}
}

func sleepJobs(t *testing.T, f fixture) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE app_id = $1 AND kind = 'sleep'`, f.appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReaperSleepsIdleAppAndLaunchWakesIt(t *testing.T) {
	f := newFixture(t, "sleep-wake")
	d := liveApp(t, f)
	ctx := context.Background()

	// A fresh app is not idle: the reaper queues nothing and a stray sleep job does nothing.
	if err := f.pipeline.reapOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := sleepJobs(t, f); n != 0 {
		t.Fatalf("sleep jobs for an active app = %d", n)
	}
	if err := f.pipeline.Sleep(ctx, appJob(KindSleep, f.appID)); err != nil {
		t.Fatal(err)
	}
	if st, _ := appState(t, f); st != "running" || len(containersOf(t, d)) != 1 {
		t.Fatalf("stray sleep job stopped an active app: status=%q containers=%v", st, containersOf(t, d))
	}

	// Idle: exactly one sleep job, however often the reaper runs.
	makeIdle(t, f, d)
	for range 3 {
		if err := f.pipeline.reapOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := sleepJobs(t, f); n != 1 {
		t.Fatalf("sleep jobs for an idle app = %d, want 1", n)
	}
	if err := f.pipeline.Sleep(ctx, appJob(KindSleep, f.appID)); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE jobs SET state = 'done' WHERE app_id = $1 AND kind = 'sleep'`, f.appID); err != nil {
		t.Fatal(err)
	}
	if st, cur := appState(t, f); st != "sleeping" || cur != d.String() {
		t.Fatalf("after sleep: status=%q current=%q (the deploy must stay current)", st, cur)
	}
	if c := containersOf(t, d); len(c) != 0 {
		t.Fatalf("containers while sleeping: %v", c)
	}
	if got := getDeploy(t, d); got.state != "live" {
		t.Fatalf("deploy state while sleeping = %s, want live", got.state)
	}
	var desired string
	if err := testPool.QueryRow(ctx, `SELECT desired_state FROM apps WHERE id = $1`, f.appID).Scan(&desired); err != nil || desired != "running" {
		t.Fatalf("desired_state while sleeping = %q (%v), want running so launch can wake it", desired, err)
	}

	// The wake is a launch job. It starts a new container from the same image.
	if err := f.pipeline.Launch(ctx, appJob(KindLaunch, f.appID)); err != nil {
		t.Fatalf("Launch (wake): %v", err)
	}
	if st, _ := appState(t, f); st != "running" {
		t.Fatalf("after wake: status = %q", st)
	}
	if code, body := getURL(t, liveURL(t, d)); code != http.StatusOK || !strings.Contains(body, "lifecycle test") {
		t.Fatalf("GET after wake = %d %q", code, body)
	}

	// A just-woken app is not idle again, even though last_request_at is old.
	if err := f.pipeline.reapOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := sleepJobs(t, f); n != 1 {
		t.Fatalf("a freshly woken app was queued for sleep again (%d sleep jobs)", n)
	}
}

// A sleep job that is late must not undo a user's stop or restart, or stop an
// app that got traffic after the job was queued.
func TestSleepJobIsNoOpWhenAppIsNotIdleAnymore(t *testing.T) {
	ctx := context.Background()

	t.Run("user stopped it", func(t *testing.T) {
		f := newFixture(t, "sleep-stopped")
		d := liveApp(t, f)
		makeIdle(t, f, d)
		setDesired(t, f, "stopped")
		if err := f.pipeline.Stop(ctx, appJob(KindStop, f.appID)); err != nil {
			t.Fatal(err)
		}
		if err := f.pipeline.reapOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if n := sleepJobs(t, f); n != 0 {
			t.Fatalf("sleep jobs for a stopped app = %d", n)
		}
		if err := f.pipeline.Sleep(ctx, appJob(KindSleep, f.appID)); err != nil {
			t.Fatal(err)
		}
		if st, _ := appState(t, f); st != "stopped" {
			t.Fatalf("status = %q, want stopped (not sleeping)", st)
		}
	})

	t.Run("traffic arrived after the job was queued", func(t *testing.T) {
		f := newFixture(t, "sleep-busy")
		d := liveApp(t, f)
		makeIdle(t, f, d)
		if _, err := testPool.Exec(ctx, `UPDATE apps SET last_request_at = now() WHERE id = $1`, f.appID); err != nil {
			t.Fatal(err)
		}
		if err := f.pipeline.Sleep(ctx, appJob(KindSleep, f.appID)); err != nil {
			t.Fatal(err)
		}
		if st, _ := appState(t, f); st != "running" || len(containersOf(t, d)) != 1 {
			t.Fatalf("busy app was put to sleep: status=%q containers=%v", st, containersOf(t, d))
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		f := newFixture(t, "sleep-ghost")
		var ghost pgtype.UUID
		_ = ghost.Scan("00000000-0000-0000-0000-000000000002")
		if err := f.pipeline.Sleep(ctx, appJob(KindSleep, ghost)); err != nil {
			t.Fatalf("Sleep for unknown app: %v", err)
		}
	})
}
