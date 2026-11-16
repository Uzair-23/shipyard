package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
)

// Router serves app traffic and keeps its route table current.
type Router struct {
	Pool       *pgxpool.Pool
	AppsDomain string // e.g. apps.localhost
	Addr       string // listen address, e.g. 127.0.0.1:8081

	// BaseDomain is the dashboard host (e.g. shipyard.localhost), where the auth handoff lives.
	BaseDomain string
	// CookieKey signs the _sy_app cookie. Without it the router refuses to serve private apps.
	CookieKey []byte
	// Backend overrides the database-backed access backend (tests).
	Backend Backend
	// AccessCacheTTL is how long an access decision is remembered; default 60 s.
	AccessCacheTTL time.Duration

	ReloadInterval time.Duration // full reload fallback; default 5s
	FlushInterval  time.Duration // last_request_at flush; default 10s

	table *Table
	act   *activity
}

// New returns a router for the given apps domain.
func New(pool *pgxpool.Pool, appsDomain, addr string) *Router {
	return &Router{Pool: pool, AppsDomain: appsDomain, Addr: addr, table: NewTable(), act: newActivity()}
}

// Handler is the HTTP handler (exposed for tests).
func (r *Router) Handler() http.Handler {
	var g *gate
	if len(r.CookieKey) > 0 {
		be := r.Backend
		if be == nil {
			be = newDBBackend(r.Pool, r.AccessCacheTTL)
		}
		g = &gate{backend: be, key: r.CookieKey, baseDomain: r.BaseDomain}
	}
	return newHandler(r.table, r.AppsDomain, r.act, g)
}

// Run listens on Addr and serves until ctx is cancelled.
func (r *Router) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.Addr, err)
	}
	return r.Serve(ctx, ln)
}

// Serve loads the route table, then serves on ln until ctx is cancelled.
func (r *Router) Serve(ctx context.Context, ln net.Listener) error {
	if r.ReloadInterval == 0 {
		r.ReloadInterval = 5 * time.Second
	}
	if r.FlushInterval == 0 {
		r.FlushInterval = 10 * time.Second
	}
	if err := r.reloadAll(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("load routes: %w", err)
	}

	bg, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.listen(bg)
	go r.reloadLoop(bg)
	go r.flushLoop(bg)

	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("router listening", "addr", ln.Addr().String(), "apps_domain", r.AppsDomain)

	select {
	case err := <-errc:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	err := srv.Shutdown(shutdownCtx)
	r.act.flush(shutdownCtx, r.Pool)
	return err
}

func (r *Router) reloadAll(ctx context.Context) error {
	rows, err := store.New(r.Pool).ListRoutes(ctx)
	if err != nil {
		return err
	}
	routes := make([]Route, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, Route{AppID: row.AppID.String(), Slug: row.Slug, Status: row.Status, Visibility: row.Visibility, Port: int(row.HostPort)})
	}
	r.table.Replace(routes)
	return nil
}

func (r *Router) reloadApp(ctx context.Context, appID pgtype.UUID) error {
	row, err := store.New(r.Pool).GetRoute(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		r.table.Remove(appID.String())
		return nil
	}
	if err != nil {
		return err
	}
	r.table.Set(Route{AppID: row.AppID.String(), Slug: row.Slug, Status: row.Status, Visibility: row.Visibility, Port: int(row.HostPort)})
	return nil
}

func (r *Router) reloadLoop(ctx context.Context) {
	t := time.NewTicker(r.ReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.reloadAll(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("route reload failed", "error", err)
			}
		}
	}
}

func (r *Router) flushLoop(ctx context.Context) {
	t := time.NewTicker(r.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.act.flush(ctx, r.Pool)
		}
	}
}

// listen applies route_changes notifications. When the connection drops it
// reconnects and does a full reload, since notifications may have been missed.
func (r *Router) listen(ctx context.Context) {
	for ctx.Err() == nil {
		err := r.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listen route_changes failed; relying on periodic reload", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (r *Router) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, r.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN route_changes"); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := r.reloadAll(ctx); err != nil { // anything missed while disconnected
		return fmt.Errorf("reload after listen: %w", err)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		var id pgtype.UUID
		if err := id.Scan(n.Payload); err != nil {
			slog.Warn("route_changes with a bad payload", "payload", n.Payload)
			continue
		}
		if err := r.reloadApp(ctx, id); err != nil {
			slog.Warn("route reload failed", "app_id", n.Payload, "error", err)
		}
	}
}
