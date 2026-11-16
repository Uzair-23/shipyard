//go:build integration

package apps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type logRow struct{ stream, line string }

func logsOf(t *testing.T, deploy pgtype.UUID) []logRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT stream, line FROM log_lines WHERE deploy_id = $1 ORDER BY id`, deploy)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.stream, &r.line); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func hasLine(rows []logRow, stream, sub string) int {
	for i, r := range rows {
		if r.stream == stream && strings.Contains(r.line, sub) {
			return i
		}
	}
	return -1
}

func TestBuildAndStartWriteTheDeployLog(t *testing.T) {
	f := newFixture(t, "log-ok")
	d := builtDeploy(t, f, map[string]string{"index.html": "log test"})
	if err := f.pipeline.Start(context.Background(), startJob(d)); err != nil {
		t.Fatal(err)
	}
	rows := logsOf(t, d)
	if len(rows) < 5 {
		t.Fatalf("only %d log lines: %v", len(rows), rows)
	}
	started, finished := hasLine(rows, "system", "Build started"), hasLine(rows, "system", "Build finished")
	step := hasLine(rows, "build", "Step")
	starting, live := hasLine(rows, "system", "Starting the app"), hasLine(rows, "system", "Deploy is live")
	if started != 0 || step < 0 || finished < step || starting < finished || live != len(rows)-1 {
		t.Fatalf("log is out of order or incomplete (started=%d step=%d finished=%d starting=%d live=%d of %d):\n%v",
			started, step, finished, starting, live, len(rows), rows)
	}
	for _, r := range rows {
		if r.line == "" || len(r.line) > 3*2001 || strings.ContainsAny(r.line, "\x00\x1b\n") {
			t.Fatalf("bad stored line %q", r.line)
		}
	}
}

func TestFailedBuildLogsTheReasonAfterTheBuildOutput(t *testing.T) {
	f := newFixture(t, "log-build-fail")
	id := f.upload(t, 1, map[string]string{
		"requirements.txt": "this-package-does-not-exist-shipyard-test==0.0.0\n",
		"app.py":           "print('hi')",
	})
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatal(err)
	}
	rows := logsOf(t, id)
	pip := hasLine(rows, "build", "this-package-does-not-exist")
	last := rows[len(rows)-1]
	if pip < 0 || last.stream != "system" || !strings.HasPrefix(last.line, "build failed") {
		t.Fatalf("failed build log: pip line at %d, last line %+v\n%v", pip, last, rows)
	}
	if strings.Count(strings.Join(lines(rows), "\n"), "last build output") != 0 {
		t.Error("the error tail should not be repeated in the log")
	}
}

func TestFailedStartLogsTheReason(t *testing.T) {
	f := newFixture(t, "log-start-fail")
	f.pipeline.HealthTimeout = 3 * time.Second
	d := builtDeploy(t, f, map[string]string{"index.html": "x", "shipyard.toml": `health = "/nope"`})
	if err := f.pipeline.Start(context.Background(), startJob(d)); err != nil {
		t.Fatal(err)
	}
	rows := logsOf(t, d)
	last := rows[len(rows)-1]
	if last.stream != "system" || !strings.HasPrefix(last.line, "deploy failed") || hasLine(rows, "system", "Deploy is live") >= 0 {
		t.Fatalf("log of a failed start: %v", rows)
	}
}

func lines(rows []logRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.line
	}
	return out
}
