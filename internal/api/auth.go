package api

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Uzair-23/shipyard/internal/auth"
	"github.com/Uzair-23/shipyard/internal/store"
)

type userKey struct{}

func userFrom(ctx context.Context) (auth.User, bool) {
	u, ok := ctx.Value(userKey{}).(auth.User)
	return u, ok
}

// requireUser resolves the session cookie to a user, or answers 401.
func (a *Apps) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok, err := a.sessions.Lookup(r.Context(), auth.TokenFromRequest(r))
		if err != nil {
			internalError(w, "resolve session", err)
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "log in to continue")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func userJSON(u auth.User) map[string]string {
	return map[string]string{"id": u.ID.String(), "email": u.Email}
}

// validEmail returns the normalised (trimmed, lower-case) address, or "".
func validEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return ""
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" || !strings.Contains(s[strings.LastIndexByte(s, '@')+1:], ".") && !strings.HasSuffix(s, "@localhost") {
		return ""
	}
	return s
}

func (a *Apps) signup(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c, `{"email":"you@example.com","password":"at least 8 characters"}`) {
		return
	}
	email := validEmail(c.Email)
	if email == "" {
		writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
		return
	}
	if n := len(c.Password); n < auth.MinPasswordLen || n > auth.MaxPasswordLen {
		writeError(w, http.StatusBadRequest, "invalid_password",
			"the password must be 8 to 128 characters")
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		internalError(w, "hash password", err)
		return
	}
	user, err := store.New(a.Pool).CreateUser(r.Context(), store.CreateUserParams{
		Email: email, PasswordHash: textOf(hash),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "email_taken", "an account with this email already exists; log in instead")
		return
	}
	if err != nil {
		internalError(w, "create user", err)
		return
	}
	a.startSession(w, r, auth.User{ID: user.ID, Email: user.Email}, http.StatusCreated)
}

func (a *Apps) login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c, `{"email":"you@example.com","password":"..."}`) {
		return
	}
	// One generic answer for every failure, and the same work for unknown accounts.
	fail := func() {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "wrong email or password")
	}
	if len(c.Password) > auth.MaxPasswordLen {
		fail()
		return
	}
	user, err := store.New(a.Pool).GetUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(c.Email)))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!user.PasswordHash.Valid || user.SuspendedAt.Valid)) {
		auth.BurnPasswordCheck(c.Password)
		fail()
		return
	}
	if err != nil {
		internalError(w, "load user", err)
		return
	}
	ok, err := auth.VerifyPassword(c.Password, user.PasswordHash.String)
	if err != nil {
		internalError(w, "verify password", err)
		return
	}
	if !ok {
		fail()
		return
	}
	a.startSession(w, r, auth.User{ID: user.ID, Email: user.Email}, http.StatusOK)
}

func (a *Apps) startSession(w http.ResponseWriter, r *http.Request, u auth.User, status int) {
	token, expires, err := a.sessions.Create(r.Context(), u.ID)
	if err != nil {
		internalError(w, "start session", err)
		return
	}
	auth.SetCookie(w, r, token, expires)
	writeJSON(w, status, userJSON(u))
}

func (a *Apps) logout(w http.ResponseWriter, r *http.Request) {
	if token := auth.TokenFromRequest(r); token != "" {
		if err := a.sessions.Delete(r.Context(), token); err != nil {
			internalError(w, "delete session", err)
			return
		}
	}
	auth.ClearCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Apps) me(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	writeJSON(w, http.StatusOK, userJSON(u))
}
