package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Uzair-23/shipyard/internal/store"
)

const (
	logPollInterval = 500 * time.Millisecond
	logKeepalive    = 15 * time.Second
	logMaxDuration  = 20 * time.Minute // a build is limited to 5 minutes; this is only a safety net
)

// deployEnded reports whether a deploy's log can no longer grow.
func deployEnded(state string) bool {
	switch state {
	case "live", "failed", "superseded":
		return true
	}
	return false
}

// deployLogs is GET /apps/{slug}/deploys/{seq}/logs: a Server-Sent Events
// stream of the deploy's log. It sends the lines stored so far, then new ones
// as they appear, and finally an "end" event with the deploy's state.
//
// Each "log" event has id = the line id and a JSON payload
// {"id","stream","line","ts"}. A client that reconnects sends Last-Event-ID
// (or ?after=<id>) to continue where it stopped. Line text is untrusted
// (it comes from the app's build): show it as text, never as HTML.
func (a *Apps) deployLogs(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 32)
	if err != nil || seq < 1 {
		writeError(w, http.StatusNotFound, "not_found", "deploy not found")
		return
	}
	q := store.New(a.Pool)
	deploy, err := q.GetOwnedDeployForLogs(r.Context(), store.GetOwnedDeployForLogsParams{
		Slug: chi.URLParam(r, "slug"), OwnerID: user.ID, Seq: int32(seq),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "deploy not found")
		return
	}
	if err != nil {
		internalError(w, "load deploy", err)
		return
	}
	after := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	if after < 0 {
		after = 0
	}

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	ctx := r.Context()
	deadline := time.After(logMaxDuration)
	lastSend := time.Now()
	for {
		// The state is read before the lines: if the deploy has ended and nothing
		// newer than `after` exists, everything has been sent. (Lines are written
		// before the state changes, in the same transaction.)
		state, err := q.GetDeployState(ctx, deploy.ID)
		if err != nil {
			return
		}
		rows, err := q.ListDeployLogs(ctx, store.ListDeployLogsParams{DeployID: deploy.ID, After: after})
		if err != nil {
			return
		}
		for _, row := range rows {
			payload, _ := json.Marshal(map[string]any{
				"id": row.ID, "stream": row.Stream, "line": row.Line, "ts": row.Ts.Time.UTC().Format(time.RFC3339Nano),
			})
			if _, err := fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", row.ID, payload); err != nil {
				return
			}
			after = row.ID
		}
		if len(rows) > 0 {
			if rc.Flush() != nil {
				return
			}
			lastSend = time.Now()
			continue // there may be more
		}
		if deployEnded(state) {
			payload, _ := json.Marshal(map[string]string{"state": state})
			_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", payload)
			_ = rc.Flush()
			return
		}
		if time.Since(lastSend) >= logKeepalive {
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
			lastSend = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			_, _ = fmt.Fprint(w, "event: end\ndata: {\"state\":\"timeout\"}\n\n")
			_ = rc.Flush()
			return
		case <-time.After(logPollInterval):
		}
	}
}
