package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/store"
)

// decodeJSON reads a small JSON body into v (unknown fields are errors).
// It answers 400 itself and returns false when the body is unusable.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, example string) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusBadRequest, "invalid_request", "send a JSON body with Content-Type: application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON like "+example)
		return false
	}
	return true
}

func textOf(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

type deployView struct {
	Seq        int32      `json:"seq"`
	State      string     `json:"state"`
	Error      *string    `json:"error,omitempty"` // untrusted: may contain the app's own output; render as text
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type appView struct {
	Slug          string      `json:"slug"`
	Status        string      `json:"status"`
	URL           string      `json:"url"`
	Visibility    string      `json:"visibility"` // "private" (owner and members) or "public"
	IdleTimeoutS  int32       `json:"idle_timeout_s"`
	CurrentDeploy *deployView `json:"current_deploy"` // the deploy being served, if any
	LatestDeploy  *deployView `json:"latest_deploy"`  // the newest deploy, whatever its state (building, failed, ...)
	CreatedAt     time.Time   `json:"created_at"`
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func (a *Apps) appURL(slug string) string { return fmt.Sprintf("http://%s.%s", slug, a.AppsDomain) }

func (a *Apps) viewOf(row store.GetOwnedAppRow) appView {
	v := appView{
		Slug: row.Slug, Status: row.Status, URL: a.appURL(row.Slug), Visibility: row.Visibility, IdleTimeoutS: row.IdleTimeoutS,
		CreatedAt: row.CreatedAt.Time.UTC(),
	}
	if row.CurrentSeq.Valid {
		v.CurrentDeploy = &deployView{Seq: row.CurrentSeq.Int32, State: row.CurrentState.String, FinishedAt: timePtr(row.CurrentFinishedAt)}
	}
	if row.LatestSeq > 0 { // deploy numbers start at 1; 0 means the app has no deploys
		d := &deployView{Seq: row.LatestSeq, State: row.LatestState,
			CreatedAt: timePtr(row.LatestCreatedAt), FinishedAt: timePtr(row.LatestFinishedAt)}
		if row.LatestError.Valid {
			d.Error = &row.LatestError.String
		}
		v.LatestDeploy = d
	}
	return v
}

// writeApp answers with the caller's app as JSON, or 404 if it is not theirs.
func (a *Apps) writeApp(w http.ResponseWriter, r *http.Request, status int, slug string) {
	user, _ := userFrom(r.Context())
	row, err := store.New(a.Pool).GetOwnedApp(r.Context(), store.GetOwnedAppParams{Slug: slug, OwnerID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return
	}
	if err != nil {
		internalError(w, "load app", err)
		return
	}
	writeJSON(w, status, a.viewOf(row))
}

func (a *Apps) listApps(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	rows, err := store.New(a.Pool).ListOwnedApps(r.Context(), user.ID)
	if err != nil {
		internalError(w, "list apps", err)
		return
	}
	type item struct {
		Slug      string    `json:"slug"`
		Status    string    `json:"status"`
		URL       string    `json:"url"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := make([]item, 0, len(rows))
	for _, row := range rows {
		out = append(out, item{Slug: row.Slug, Status: row.Status, URL: a.appURL(row.Slug), CreatedAt: row.CreatedAt.Time.UTC()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (a *Apps) getApp(w http.ResponseWriter, r *http.Request) {
	a.writeApp(w, r, http.StatusOK, chi.URLParam(r, "slug"))
}

// Idle timeout bounds (PRD RUN-1): 5 to 60 minutes.
const (
	minIdleTimeoutS = 300
	maxIdleTimeoutS = 3600
)

func (a *Apps) patchApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdleTimeoutS *int32  `json:"idle_timeout_s"`
		Visibility   *string `json:"visibility"`
	}
	if !decodeJSON(w, r, &req, `{"visibility":"public","idle_timeout_s":600}`) {
		return
	}
	if req.IdleTimeoutS == nil && req.Visibility == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to change: send idle_timeout_s or visibility")
		return
	}
	if req.IdleTimeoutS != nil && (*req.IdleTimeoutS < minIdleTimeoutS || *req.IdleTimeoutS > maxIdleTimeoutS) {
		writeError(w, http.StatusBadRequest, "invalid_idle_timeout", "idle_timeout_s must be between 300 and 3600 seconds")
		return
	}
	if req.Visibility != nil && *req.Visibility != "private" && *req.Visibility != "public" {
		writeError(w, http.StatusBadRequest, "invalid_visibility", `visibility must be "private" or "public"`)
		return
	}
	slug := chi.URLParam(r, "slug")
	err := a.inOwnedAppTx(r, slug, func(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
		q := store.New(tx)
		if req.IdleTimeoutS != nil {
			if err := q.SetIdleTimeout(ctx, store.SetIdleTimeoutParams{IdleTimeoutS: *req.IdleTimeoutS, ID: app.ID}); err != nil {
				return fmt.Errorf("set idle timeout: %w", err)
			}
		}
		if req.Visibility != nil {
			if err := q.SetVisibility(ctx, store.SetVisibilityParams{Visibility: *req.Visibility, ID: app.ID}); err != nil {
				return fmt.Errorf("set visibility: %w", err)
			}
			return q.NotifyRouteChange(ctx, app.ID) // routers reload the app's visibility
		}
		return nil
	})
	if a.lifecycleFailed(w, err) {
		return
	}
	a.writeApp(w, r, http.StatusOK, slug)
}
