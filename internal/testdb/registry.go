package testdb

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartRegistry launches a throwaway registry:2 and returns its address as
// localhost:<port>, which the Docker engine may push to over plain HTTP.
func StartRegistry(ctx context.Context) (addr string, terminate func(), err error) {
	ctr, err := testcontainers.Run(ctx, "registry:2",
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return "", nil, fmt.Errorf("start registry container: %w", err)
	}
	terminate = func() { _ = testcontainers.TerminateContainer(ctr) }
	port, err := ctr.MappedPort(ctx, "5000/tcp")
	if err != nil {
		terminate()
		return "", nil, fmt.Errorf("registry port: %w", err)
	}
	return "localhost:" + port.Port(), terminate, nil
}
