package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/apps"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/store"
)

// lifecycle records what the user wants (apps.desired_state) and enqueues the
// job that makes it so. The API never touches containers; the worker does.
func (a *Apps) lifecycle(kind, desired string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		err := a.inOwnedAppTx(r, slug, func(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
			if !app.CurrentDeployID.Valid {
				return errNotDeployed
			}
			if err := store.New(tx).SetDesiredState(ctx, store.SetDesiredStateParams{DesiredState: desired, ID: app.ID}); err != nil {
				return fmt.Errorf("set desired state: %w", err)
			}
			return enqueueAppJob(ctx, tx, kind, app.ID)
		})
		if a.lifecycleFailed(w, err) {
			return
		}
		a.writeApp(w, r, http.StatusAccepted, slug)
	}
}

// deleteApp soft-deletes the app: it leaves the lists and routes at once, its
// slug is free again, unfinished deploys are failed so queued jobs do nothing,
// and a stop job removes its containers.
func (a *Apps) deleteApp(w http.ResponseWriter, r *http.Request) {
	err := a.inOwnedAppTx(r, chi.URLParam(r, "slug"), func(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
		q := store.New(tx)
		if err := q.SoftDeleteApp(ctx, app.ID); err != nil {
			return fmt.Errorf("delete app: %w", err)
		}
		if err := q.FailUnfinishedDeploys(ctx, app.ID); err != nil {
			return fmt.Errorf("fail unfinished deploys: %w", err)
		}
		if err := enqueueAppJob(ctx, tx, apps.KindStop, app.ID); err != nil {
			return err
		}
		return q.NotifyRouteChange(ctx, app.ID)
	})
	if a.lifecycleFailed(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errNotDeployed = errors.New("app has no live deploy")

// inOwnedAppTx runs fn in a transaction holding the caller's app row.
func (a *Apps) inOwnedAppTx(r *http.Request, slug string, fn func(context.Context, pgx.Tx, store.LockOwnedAppRow) error) error {
	user, _ := userFrom(r.Context())
	return pgx.BeginFunc(r.Context(), a.Pool, func(tx pgx.Tx) error {
		app, err := store.New(tx).LockOwnedApp(r.Context(), store.LockOwnedAppParams{Slug: slug, OwnerID: user.ID})
		if err != nil {
			return err
		}
		return fn(r.Context(), tx, app)
	})
}

// lifecycleFailed answers for an error from inOwnedAppTx and reports whether it did.
func (a *Apps) lifecycleFailed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "app not found")
	case errors.Is(err, errNotDeployed):
		writeError(w, http.StatusConflict, "not_deployed", "this app has no live deploy yet; upload your code first")
	default:
		internalError(w, "app lifecycle", err)
	}
	return true
}

func enqueueAppJob(ctx context.Context, tx pgx.Tx, kind string, appID pgtype.UUID) error {
	payload, err := json.Marshal(apps.AppPayload{AppID: appID.String()})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	_, err = jobs.Enqueue(ctx, tx, store.EnqueueJobParams{Kind: kind, AppID: appID, Payload: payload, MaxAttempts: 3})
	return err
}
