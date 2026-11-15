//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMigrateAndQueryUsers(t *testing.T) {
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("shipyard"),
		postgres.WithUsername("shipyard"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// Migrations apply cleanly, and applying them twice is a no-op.
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, url); err != nil {
			t.Fatalf("Migrate (run %d): %v", i+1, err)
		}
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)`, "Riya@Example.com", "hash"); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// email is citext, so lookup is case-insensitive.
	u, err := New(pool).GetUserByEmail(ctx, "riya@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if !u.ID.Valid || u.IsAdmin || u.IsTrusted || !u.CreatedAt.Valid {
		t.Fatalf("unexpected user defaults: %+v", u)
	}
}
