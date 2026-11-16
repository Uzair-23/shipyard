package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Uzair-23/shipyard/internal/apps"
	"github.com/Uzair-23/shipyard/internal/secrets"
	"github.com/Uzair-23/shipyard/internal/store"
)

const (
	maxEnvVars     = 100
	maxEnvValueLen = 4096
	// maxEnvBody leaves room for a 4096-byte value that JSON escapes (up to 6 bytes per byte).
	maxEnvBody = 32 << 10
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// envNameProblem explains why name cannot be used, or returns "".
func envNameProblem(name string) string {
	switch {
	case !envNameRE.MatchString(name):
		return "names use letters, digits and underscores, start with a letter or underscore, and are at most 64 characters"
	case apps.ReservedEnvName(name):
		return "this name is set by Shipyard and cannot be changed"
	}
	return ""
}

func (a *Apps) secretsReady(w http.ResponseWriter) bool {
	if len(a.MasterKey) != secrets.KeyLen {
		writeError(w, http.StatusServiceUnavailable, "secrets_unavailable",
			"environment variables are unavailable because the server has no master key configured")
		return false
	}
	return true
}

type envItem struct {
	Key       string    `json:"key"`
	IsSecret  bool      `json:"is_secret"`
	Value     *string   `json:"value,omitempty"` // never set for secrets
	UpdatedAt time.Time `json:"updated_at"`
}

func (a *Apps) listEnv(w http.ResponseWriter, r *http.Request) {
	if !a.secretsReady(w) {
		return
	}
	id, ok := a.ownedAppID(w, r)
	if !ok {
		return
	}
	rows, err := store.New(a.Pool).ListEnv(r.Context(), id)
	if err != nil {
		internalError(w, "list env", err)
		return
	}
	out := make([]envItem, 0, len(rows))
	for _, row := range rows {
		item := envItem{Key: row.Key, IsSecret: row.IsSecret, UpdatedAt: row.UpdatedAt.Time.UTC()}
		if !row.IsSecret {
			plain, err := secrets.Decrypt(a.MasterKey, id.String(), row.Key, row.ValueEncrypted, row.Nonce)
			if err != nil {
				internalError(w, "decrypt env value", err)
				return
			}
			v := string(plain)
			item.Value = &v
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": out})
}

func (a *Apps) putEnv(w http.ResponseWriter, r *http.Request) {
	if !a.secretsReady(w) {
		return
	}
	name := chi.URLParam(r, "key")
	if msg := envNameProblem(name); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_name", msg)
		return
	}
	var req struct {
		Value    *string `json:"value"`
		IsSecret bool    `json:"is_secret"`
	}
	if !decodeJSONLimit(w, r, &req, `{"value":"...","is_secret":true}`, maxEnvBody) {
		return
	}
	if req.Value == nil || len(*req.Value) > maxEnvValueLen || !utf8.ValidString(*req.Value) || strings.ContainsRune(*req.Value, 0) {
		writeError(w, http.StatusBadRequest, "invalid_value", "value is required, at most 4096 bytes, and cannot contain NUL")
		return
	}
	err := a.inOwnedAppTx(r, chi.URLParam(r, "slug"), func(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
		q := store.New(tx)
		exists, err := q.EnvExists(ctx, store.EnvExistsParams{AppID: app.ID, Key: name})
		if err != nil {
			return fmt.Errorf("check variable: %w", err)
		}
		if !exists {
			n, err := q.CountEnv(ctx, app.ID)
			if err != nil {
				return fmt.Errorf("count variables: %w", err)
			}
			if n >= maxEnvVars {
				return errTooManyEnv
			}
		}
		ct, nonce, err := secrets.Encrypt(a.MasterKey, app.ID.String(), name, []byte(*req.Value))
		if err != nil {
			return fmt.Errorf("encrypt value: %w", err)
		}
		if err := q.UpsertEnv(ctx, store.UpsertEnvParams{AppID: app.ID, Key: name, ValueEncrypted: ct, Nonce: nonce, IsSecret: req.IsSecret}); err != nil {
			return fmt.Errorf("save variable: %w", err)
		}
		return restartIfRunning(ctx, tx, app)
	})
	if errors.Is(err, errTooManyEnv) {
		writeError(w, http.StatusConflict, "too_many_variables", "an app can have at most 100 environment variables")
		return
	}
	if a.lifecycleFailed(w, err) {
		return
	}
	// The value is never echoed back, secret or not.
	writeJSON(w, http.StatusOK, map[string]any{"key": name, "is_secret": req.IsSecret})
}

func (a *Apps) deleteEnv(w http.ResponseWriter, r *http.Request) {
	if !a.secretsReady(w) {
		return
	}
	name := chi.URLParam(r, "key")
	err := a.inOwnedAppTx(r, chi.URLParam(r, "slug"), func(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
		n, err := store.New(tx).DeleteEnv(ctx, store.DeleteEnvParams{AppID: app.ID, Key: name})
		if err != nil {
			return fmt.Errorf("delete variable: %w", err)
		}
		if n == 0 {
			return errNoSuchEnv
		}
		return restartIfRunning(ctx, tx, app)
	})
	if errors.Is(err, errNoSuchEnv) {
		writeError(w, http.StatusNotFound, "not_found", "no such variable")
		return
	}
	if a.lifecycleFailed(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	errTooManyEnv = errors.New("too many variables")
	errNoSuchEnv  = errors.New("no such variable")
)

// restartIfRunning applies a changed environment: a running app is restarted.
// An app that is stopped or was never deployed just picks the change up when it starts.
func restartIfRunning(ctx context.Context, tx pgx.Tx, app store.LockOwnedAppRow) error {
	if !app.CurrentDeployID.Valid || app.DesiredState != "running" {
		return nil
	}
	return enqueueAppJob(ctx, tx, apps.KindRestart, app.ID)
}
