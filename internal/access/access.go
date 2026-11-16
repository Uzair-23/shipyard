// Package access decides who may view an app. The API (for the auth handoff)
// and the router share it, so the rule lives in one place: the owner, a member
// (matched by email) or anyone if the app is public.
package access

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/store"
)

// CanView reports whether the user may view the app.
func CanView(ctx context.Context, db store.DBTX, appID, userID pgtype.UUID, email string) (bool, error) {
	ok, err := store.New(db).CanViewApp(ctx, store.CanViewAppParams{AppID: appID, UserID: userID, Email: email})
	if err != nil {
		return false, fmt.Errorf("check access: %w", err)
	}
	return ok, nil
}

// Cache remembers CanView answers for TTL, so the router does not query on
// every request. A removed member loses access after at most one TTL.
type Cache struct {
	DB  store.DBTX
	TTL time.Duration // default 60 s

	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	ok      bool
	expires time.Time
}

const maxCacheEntries = 10_000

// CanView is access.CanView with caching. Errors are not cached.
func (c *Cache) CanView(ctx context.Context, appID, userID pgtype.UUID, email string) (bool, error) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	key := appID.String() + "|" + userID.String() + "|" + strings.ToLower(email)
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.ok, nil
	}
	c.mu.Unlock()

	ok, err := CanView(ctx, c.DB, appID, userID, email)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= maxCacheEntries {
		c.entries = make(map[string]entry)
	}
	c.entries[key] = entry{ok: ok, expires: now.Add(ttl)}
	c.mu.Unlock()
	return ok, nil
}
