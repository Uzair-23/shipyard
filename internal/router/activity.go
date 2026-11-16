package router

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
)

// activity remembers when each app last served a request and writes the
// values to apps.last_request_at in batches (the idle reaper reads them).
type activity struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newActivity() *activity { return &activity{last: map[string]time.Time{}} }

func (a *activity) mark(appID string) {
	now := time.Now()
	a.mu.Lock()
	a.last[appID] = now
	a.mu.Unlock()
}

func (a *activity) drain() map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.last
	a.last = map[string]time.Time{}
	return out
}

// flush writes the pending timestamps. On failure they are put back, unless a
// newer request has been recorded since.
func (a *activity) flush(ctx context.Context, pool *pgxpool.Pool) {
	pending := a.drain()
	if len(pending) == 0 {
		return
	}
	var p store.TouchAppsParams
	for id, ts := range pending {
		var u pgtype.UUID
		if err := u.Scan(id); err != nil {
			continue
		}
		p.Ids = append(p.Ids, u)
		p.Times = append(p.Times, pgtype.Timestamptz{Time: ts, Valid: true})
	}
	if err := store.New(pool).TouchApps(ctx, p); err != nil {
		slog.Warn("flush last_request_at failed", "error", err)
		a.mu.Lock()
		for id, ts := range pending {
			if cur, ok := a.last[id]; !ok || cur.Before(ts) {
				a.last[id] = ts
			}
		}
		a.mu.Unlock()
	}
}
