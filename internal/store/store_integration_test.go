//go:build integration

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/Uzair-23/shipyard/internal/testdb"
	"github.com/Uzair-23/shipyard/migrations"
)

// Shared by every test in the package: one Postgres container, migrated once.
var (
	testURL  string
	testPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	var (
		terminate func()
		err       error
	)
	if testURL, terminate, err = testdb.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer terminate()

	// Migrations apply cleanly, and applying them twice is a no-op.
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, testURL); err != nil {
			fmt.Fprintf(os.Stderr, "Migrate (run %d): %v\n", i+1, err)
			return 1
		}
	}
	if testPool, err = pgxpool.New(ctx, testURL); err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer testPool.Close()
	return m.Run()
}

func exec(t *testing.T, query string, args ...any) error {
	t.Helper()
	_, err := testPool.Exec(context.Background(), query, args...)
	return err
}

func mustExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if err := exec(t, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestGetUserByEmailIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	mustExec(t, `INSERT INTO users (email, password_hash) VALUES ($1, $2)`, "Riya@Example.com", "hash")

	u, err := New(testPool).GetUserByEmail(ctx, "riya@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if !u.ID.Valid || u.IsAdmin || u.IsTrusted || !u.CreatedAt.Valid {
		t.Fatalf("unexpected user defaults: %+v", u)
	}
}

func TestAppSlugConstraint(t *testing.T) {
	tests := []struct {
		slug string
		ok   bool
	}{
		{"invoice-cleaner", true},
		{"abc", true},
		{"ab", false},                              // too short
		{"Invoice", false},                         // uppercase
		{"has_underscore", false},                  // invalid char
		{"a234567890123456789012345678901", false}, // 31 chars
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			err := exec(t, `INSERT INTO apps (slug) VALUES ($1)`, tt.slug)
			if (err == nil) != tt.ok {
				t.Fatalf("insert slug %q: err=%v, want ok=%v", tt.slug, err, tt.ok)
			}
		})
	}
	if err := exec(t, `INSERT INTO apps (slug) VALUES ('invoice-cleaner')`); err == nil {
		t.Fatal("duplicate slug accepted")
	}
}

func TestDeployAndInstanceConstraints(t *testing.T) {
	ctx := context.Background()
	var appID string
	if err := testPool.QueryRow(ctx, `INSERT INTO apps (slug) VALUES ('deploy-test') RETURNING id`).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	mustExec(t, `INSERT INTO deploys (app_id, seq, source_ref) VALUES ($1, 1, 'sha256:a')`, appID)
	if err := exec(t, `INSERT INTO deploys (app_id, seq, source_ref) VALUES ($1, 1, 'sha256:b')`, appID); err == nil {
		t.Error("duplicate (app_id, seq) accepted")
	}
	if err := exec(t, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 2, 'x', 'bogus')`, appID); err == nil {
		t.Error("invalid deploy state accepted")
	}

	var deployID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM deploys WHERE app_id = $1 AND seq = 1`, appID).Scan(&deployID); err != nil {
		t.Fatalf("select deploy: %v", err)
	}
	mustExec(t, `UPDATE apps SET current_deploy_id = $1 WHERE id = $2`, deployID, appID)
	mustExec(t, `INSERT INTO instances (deploy_id) VALUES ($1)`, deployID)
	mustExec(t, `INSERT INTO jobs (kind, app_id) VALUES ('build', $1)`, appID)

	if err := exec(t, `INSERT INTO jobs (kind, state) VALUES ('build', 'bogus')`); err == nil {
		t.Error("invalid job state accepted")
	}

	// Deleting an app cascades to its deploys, instances and jobs.
	mustExec(t, `DELETE FROM apps WHERE id = $1`, appID)
	var n int
	if err := testPool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM deploys WHERE app_id = $1)
		     + (SELECT count(*) FROM instances WHERE deploy_id = $2)
		     + (SELECT count(*) FROM jobs WHERE app_id = $1)`, appID, deployID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d rows left after deleting app, want 0", n)
	}
}

// TestMigrationsDownAndUp drops every table and recreates them; tests after it see empty tables.
func TestMigrationsDownAndUp(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

// One live deploy per app is enforced by a partial unique index.
func TestOnlyOneLiveDeployPerApp(t *testing.T) {
	ctx := context.Background()
	var appID string
	if err := testPool.QueryRow(ctx, `INSERT INTO apps (slug) VALUES ('one-live') RETURNING id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 1, 'x', 'live')`, appID)
	if err := exec(t, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 2, 'y', 'live')`, appID); err == nil {
		t.Fatal("a second live deploy for the same app was accepted")
	}
	mustExec(t, `INSERT INTO deploys (app_id, seq, source_ref, state) VALUES ($1, 3, 'z', 'superseded')`, appID)
}
