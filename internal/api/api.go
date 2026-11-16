// Package api implements the Shipyard control-plane HTTP API.
//
// It must never import internal/runtime or talk to Docker: it records desired
// state and enqueues jobs; the worker executes them.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Pinger reports whether the platform database is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// NewRouter builds the API handler. apps may be nil (health check only).
func NewRouter(db Pinger, apps *Apps) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", healthz(db))
	if apps != nil {
		apps.mount(r)
	}
	return r
}

func healthz(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, body := http.StatusOK, map[string]string{"db": "ok"}
		if err := db.Ping(ctx); err != nil {
			slog.Error("healthz: database ping failed", "error", err)
			status, body = http.StatusServiceUnavailable, map[string]string{"db": "error"}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Error("healthz: write response failed", "error", err)
		}
	}
}
