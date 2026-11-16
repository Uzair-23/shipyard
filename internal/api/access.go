package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/store"
)

// maxMembers bounds how many people one app can be shared with.
const maxMembers = 50

// ownedAppID returns the caller's app, or answers 404 itself.
func (a *Apps) ownedAppID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	user, _ := userFrom(r.Context())
	p := store.GetOwnedAppIDParams{Slug: chi.URLParam(r, "slug"), OwnerID: user.ID}
	id, err := store.New(a.Pool).GetOwnedAppID(r.Context(), p)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return pgtype.UUID{}, false
	}
	if err != nil {
		internalError(w, "load app", err)
		return pgtype.UUID{}, false
	}
	return id, true
}

func (a *Apps) listMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := a.ownedAppID(w, r)
	if !ok {
		return
	}
	rows, err := store.New(a.Pool).ListMembers(r.Context(), id)
	if err != nil {
		internalError(w, "list members", err)
		return
	}
	type member struct {
		Email     string    `json:"email"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := make([]member, 0, len(rows))
	for _, m := range rows {
		out = append(out, member{Email: m.Email, CreatedAt: m.CreatedAt.Time.UTC()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (a *Apps) addMember(w http.ResponseWriter, r *http.Request) {
	id, ok := a.ownedAppID(w, r)
	if !ok {
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req, `{"email":"friend@example.com"}`) {
		return
	}
	email := validEmail(req.Email)
	if email == "" {
		writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
		return
	}
	if owner, _ := userFrom(r.Context()); strings.EqualFold(owner.Email, email) {
		writeError(w, http.StatusBadRequest, "invalid_email", "you already own this app")
		return
	}
	q := store.New(a.Pool)
	n, err := q.CountMembers(r.Context(), id)
	if err != nil {
		internalError(w, "count members", err)
		return
	}
	if n >= maxMembers {
		writeError(w, http.StatusConflict, "too_many_members", "an app can be shared with at most 50 people")
		return
	}
	added, err := q.AddMember(r.Context(), store.AddMemberParams{AppID: id, Email: email})
	if err != nil {
		internalError(w, "add member", err)
		return
	}
	status := http.StatusOK // already a member: nothing changed
	if added > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]string{"email": email})
}

func (a *Apps) removeMember(w http.ResponseWriter, r *http.Request) {
	id, ok := a.ownedAppID(w, r)
	if !ok {
		return
	}
	email, err := url.PathUnescape(chi.URLParam(r, "email"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
		return
	}
	n, err := store.New(a.Pool).RemoveMember(r.Context(), store.RemoveMemberParams{AppID: id, Email: strings.ToLower(strings.TrimSpace(email))})
	if err != nil {
		internalError(w, "remove member", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "that email is not a member of this app")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
