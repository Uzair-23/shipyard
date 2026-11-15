// Package testdb starts a throwaway PostgreSQL container for integration tests.
// Only test code may import it.
package testdb

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start launches Postgres 16 and returns its connection URL and a function
// that terminates the container.
func Start(ctx context.Context) (url string, terminate func(), err error) {
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
		return "", nil, fmt.Errorf("start postgres container: %w", err)
	}
	terminate = func() { _ = testcontainers.TerminateContainer(ctr) }

	url, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminate()
		return "", nil, fmt.Errorf("postgres connection string: %w", err)
	}
	return url, terminate, nil
}
