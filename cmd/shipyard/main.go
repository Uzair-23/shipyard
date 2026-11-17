// Command shipyard is the single Shipyard binary. The role is chosen by the
// first argument: api, worker, router or migrate (ADR 003).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/api"
	"github.com/Uzair-23/shipyard/internal/apps"
	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/config"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/router"
	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/secrets"
	"github.com/Uzair-23/shipyard/internal/store"
)

const usage = "usage: shipyard api|worker|router|migrate"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("shipyard exited with error", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch role := args[0]; role {
	case "api":
		return runAPI(ctx)
	case "migrate":
		return runMigrate(ctx)
	case "worker":
		return runWorker(ctx)
	case "router":
		return runRouter(ctx)
	default:
		return fmt.Errorf("unknown role %q: %s", role, usage)
	}
}

func runAPI(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	apps := api.NewApps(pool, cfg.DataDir, cfg.BaseDomain)
	apps.AppsDomain = cfg.AppsDomain
	if apps.MasterKey, err = secrets.LoadMasterKey(os.Getenv, cfg.DataDir); err != nil {
		// Not fatal: the rest of the API works; environment variables answer 503.
		slog.Error("no master key: environment variables are disabled", "error", err)
	}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(pool, apps),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	slog.Info("starting", "role", "api", "addr", cfg.HTTPAddr)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func runWorker(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	const concurrency int32 = 4
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = jobs.MinPoolConns(concurrency) + 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	docker, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer func() { _ = docker.Close() }()

	engine, err := runtime.NewEngine(ctx, docker)
	if err != nil {
		return fmt.Errorf("init container engine: %w", err)
	}
	pipeline := &apps.Pipeline{
		Pool:    pool,
		Builder: &build.Builder{Docker: docker, Registry: cfg.Registry},
		Engine:  engine,
		DataDir: cfg.DataDir,
	}
	if pipeline.MasterKey, err = secrets.LoadMasterKey(os.Getenv, cfg.DataDir); err != nil {
		return fmt.Errorf("load master key: %w", err)
	}
	w := &jobs.Worker{
		Pool:        pool,
		Concurrency: concurrency,
		Handlers:    pipeline.Handlers(),
	}
	go pipeline.RunReaper(ctx, apps.ReaperInterval)
	return w.Run(ctx)
}

func runRouter(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	master, err := secrets.LoadMasterKey(os.Getenv, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load master key: %w", err)
	}
	rt := router.New(pool, cfg.AppsDomain, cfg.RouterAddr)
	rt.BaseDomain = cfg.BaseDomain
	rt.CookieKey = secrets.Derive(master, "sy_app cookie")
	return rt.Run(ctx)
}

func runMigrate(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	slog.Info("starting", "role", "migrate")
	return store.Migrate(ctx, cfg.DatabaseURL)
}
