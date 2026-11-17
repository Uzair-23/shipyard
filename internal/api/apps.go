package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/apps"
	"github.com/Uzair-23/shipyard/internal/auth"
	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/store"
)

const (
	// maxUploadConcurrency bounds uploads being normalised at once (Open issue 15).
	maxUploadConcurrency = 2
	// formOverhead is the room above the archive limit for multipart framing.
	formOverhead  = 1 << 20
	maxJSONBody   = 4 << 10
	uploadTimeout = 2 * time.Minute
	// maxDrain is how much of a rejected upload is read and thrown away so the
	// browser can finish sending and see the error (see drainBody).
	maxDrain = 512 << 20
)

// Slugs become subdomains (M2): lowercase letters, digits and inner hyphens, 3-30 chars.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,28}[a-z0-9]$`)

var reservedSlugs = map[string]bool{
	"www": true, "api": true, "admin": true, "shipyard": true, "app": true, "apps": true,
	"dashboard": true, "mail": true, "static": true, "assets": true, "registry": true,
	"auth": true, "login": true, "docs": true, "git": true, "metrics": true, "status": true,
	"console": true, "localhost": true, "internal": true, "postgres": true, "caddy": true,
}

// ValidateSlug reports why slug is not allowed, or "" if it is.
func ValidateSlug(slug string) string {
	switch {
	case !slugRE.MatchString(slug):
		return "slug must be 3-30 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen"
	case len(slug) >= 4 && slug[2:4] == "--":
		return "slug may not have hyphens in the third and fourth position"
	case reservedSlugs[slug]:
		return "this slug is reserved; choose another"
	}
	return ""
}

// Apps serves the control-plane API: accounts, apps and deploys.
// It only writes desired state and enqueues jobs: it never talks to Docker.
type Apps struct {
	Pool    *pgxpool.Pool
	DataDir string
	Limits  build.Limits // zero value means build.DefaultLimits

	// AppsDomain is the suffix apps are served under (e.g. apps.localhost), for app URLs.
	AppsDomain string

	// BaseDomain is the dashboard host (e.g. shipyard.localhost). Browser requests
	// whose Origin is any other site, including app subdomains, are refused.
	BaseDomain string

	// MasterKey encrypts environment variables; nil disables them (503).
	MasterKey []byte

	sessions auth.Sessions
	uploads  chan struct{}
}

// NewApps builds the handler set.
func NewApps(pool *pgxpool.Pool, dataDir, baseDomain string) *Apps {
	return &Apps{Pool: pool, DataDir: dataDir, BaseDomain: baseDomain, AppsDomain: "apps.localhost",
		Limits: build.DefaultLimits, sessions: auth.Sessions{Pool: pool}, uploads: make(chan struct{}, maxUploadConcurrency)}
}

func (a *Apps) mount(r chi.Router) {
	r.Get("/auth/app", a.authApp) // the auth handoff, on the dashboard host; Caddy routes /auth/* to the API
	r.Route("/api/v1", func(v chi.Router) {
		v.Use(a.sameOriginOnly) // the CSRF protection for every route, mutating or not
		v.Post("/auth/signup", a.signup)
		v.Post("/auth/login", a.login)
		v.Post("/auth/logout", a.logout)
		v.Group(func(g chi.Router) {
			g.Use(a.requireUser)
			g.Get("/me", a.me)
			g.Get("/apps", a.listApps)
			g.Post("/apps", a.createApp)
			g.Get("/apps/{slug}", a.getApp)
			g.Patch("/apps/{slug}", a.patchApp)
			g.Delete("/apps/{slug}", a.deleteApp)
			g.Get("/apps/{slug}/env", a.listEnv)
			g.Put("/apps/{slug}/env/{key}", a.putEnv)
			g.Delete("/apps/{slug}/env/{key}", a.deleteEnv)
			g.Get("/apps/{slug}/members", a.listMembers)
			g.Post("/apps/{slug}/members", a.addMember)
			g.Delete("/apps/{slug}/members/{email}", a.removeMember)
			g.Post("/apps/{slug}/start", a.lifecycle(apps.KindLaunch, "running"))
			g.Post("/apps/{slug}/stop", a.lifecycle(apps.KindStop, "stopped"))
			g.Post("/apps/{slug}/restart", a.lifecycle(apps.KindRestart, "running"))
			g.Post("/apps/{slug}/deploys", a.createDeploy)
			g.Get("/apps/{slug}/deploys/{seq}/logs", a.deployLogs)
		})
	})
}

// sameOriginOnly refuses browser requests from other sites. A multipart POST is a
// CORS "simple" request, so without this any web page (or any app served from
// *.apps.*) could upload code to an app. Non-browser clients send no Origin.
func (a *Apps) sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Hostname(), a.BaseDomain) {
				writeError(w, http.StatusForbidden, "forbidden_origin", "requests from other sites are not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "something went wrong on our side; try again")
}

func (a *Apps) createApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug string `json:"slug"`
	}
	if !decodeJSON(w, r, &req, `{"slug":"my-app"}`) {
		return
	}
	if msg := ValidateSlug(req.Slug); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_slug", msg)
		return
	}
	user, _ := userFrom(r.Context())
	_, err := store.New(a.Pool).CreateApp(r.Context(), store.CreateAppParams{Slug: req.Slug, OwnerID: user.ID})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "slug_taken", "an app with this slug already exists; choose another")
		return
	}
	if err != nil {
		internalError(w, "create app", err)
		return
	}
	a.writeApp(w, r, http.StatusCreated, req.Slug)
}

func (a *Apps) createDeploy(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if ValidateSlug(slug) != "" {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return
	}
	select {
	case a.uploads <- struct{}{}:
		defer func() { <-a.uploads }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "too_many_uploads", "too many uploads in progress; retry in a few seconds")
		return
	}
	// A slow upload must not hold an upload slot forever.
	ctx, cancel := context.WithTimeout(r.Context(), uploadTimeout)
	defer cancel()
	defer drainBody(r)                                                               // runs before the response is sent, so the client sees our answer
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout)) // unsupported in tests
	user, _ := userFrom(ctx)
	appID, err := store.New(a.Pool).GetOwnedAppID(ctx, store.GetOwnedAppIDParams{Slug: slug, OwnerID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return
	}
	if err != nil {
		internalError(w, "load app", err)
		return
	}
	lim := a.Limits
	if lim == (build.Limits{}) {
		lim = build.DefaultLimits
	}

	body := http.MaxBytesReader(w, r.Body, lim.MaxCompressedBytes+formOverhead)
	mr, err := multipartReader(r, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `send multipart/form-data with a "source" file (a .zip or .tar.gz)`)
		return
	}
	root, err := a.openDataRoot()
	if err != nil {
		internalError(w, "open data dir", err)
		return
	}
	defer func() { _ = root.Close() }()
	relDir := filepath.Join("sources", appID.String())
	if err := root.MkdirAll(relDir, 0o750); err != nil {
		internalError(w, "create source dir", err)
		return
	}
	ref, _, err := a.storeSource(ctx, root, mr, relDir, lim)
	if err != nil {
		a.uploadFailed(w, err)
		return
	}

	deploy, err := a.recordDeploy(ctx, user, slug, ref)
	if err != nil {
		// The stored file is kept even if recording fails: another deploy may use the
		// same content-addressed file. Unreferenced sources are garbage collected (M9).
		var busy errBusy
		switch {
		case errors.As(err, &busy):
			writeError(w, http.StatusConflict, "deploy_in_progress", "a deploy of this app is still in progress; wait for it to finish")
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "not_found", "app not found")
		default:
			internalError(w, "record deploy", err)
		}
		return
	}
	slog.Info("deploy queued", "app_id", appID.String(), "deploy_id", deploy.ID.String(), "seq", deploy.Seq)
	writeJSON(w, http.StatusAccepted, map[string]any{"slug": slug, "seq": deploy.Seq, "state": deploy.State})
}

// drainBody reads and discards what is left of the request body (up to
// maxDrain, within the read deadline). When the server answers and closes while
// the browser is still sending a large file, the browser loses the response and
// shows "could not reach the server" instead of "file too large". Reading the
// rest first lets the browser finish and receive the real error.
func drainBody(r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrain))
}

func multipartReader(r *http.Request, body io.Reader) (*multipartPartReader, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("not multipart")
	}
	return newPartReader(body, params["boundary"]), nil
}

func (a *Apps) openDataRoot() (*os.Root, error) {
	if err := os.MkdirAll(a.DataDir, 0o750); err != nil {
		return nil, err
	}
	return os.OpenRoot(a.DataDir)
}

// storeSource streams the "source" part through build.NormalizeFile and moves
// the result to <relDir>/<sha256>.tar.gz inside root. The raw upload is never
// stored.
func (a *Apps) storeSource(ctx context.Context, root *os.Root, mr *multipartPartReader, relDir string, lim build.Limits) (ref, finalRel string, err error) {
	part, err := mr.source()
	if err != nil {
		return "", "", err
	}
	// A private name per upload: concurrent uploads of one app must not share a path.
	placeholder, err := os.CreateTemp(filepath.Join(root.Name(), relDir), "incoming-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("create upload file: %w", err)
	}
	tmpRel := filepath.Join(relDir, filepath.Base(placeholder.Name()))
	_ = placeholder.Close()
	defer func() { _ = root.Remove(tmpRel) }() // no-op once renamed
	if err := build.NormalizeFile(ctx, part, placeholder.Name(), lim); err != nil {
		return "", "", err
	}

	f, err := root.Open(tmpRel)
	if err != nil {
		return "", "", fmt.Errorf("open normalised archive: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return "", "", fmt.Errorf("hash normalised archive: %w", err)
	}
	hexSum := hex.EncodeToString(h.Sum(nil))
	ref = "sha256:" + hexSum
	finalRel = filepath.Join(relDir, hexSum+".tar.gz")
	if _, statErr := root.Stat(finalRel); statErr == nil {
		return ref, finalRel, nil
	}
	if err := root.Rename(tmpRel, finalRel); err != nil {
		return "", "", fmt.Errorf("store normalised archive: %w", err)
	}
	return ref, finalRel, nil
}

func (a *Apps) uploadFailed(w http.ResponseWriter, err error) {
	var ue *build.UploadError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ue):
		writeError(w, http.StatusBadRequest, "invalid_upload", ue.Error())
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, "upload_too_large", "the upload is too large (limit 50 MB)")
	case errors.Is(err, errNoSource):
		writeError(w, http.StatusBadRequest, "invalid_request", `the form needs a "source" file (a .zip or .tar.gz)`)
	case errors.Is(err, errBadForm):
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed multipart body")
	default:
		internalError(w, "store upload", err)
	}
}

type errBusy struct{}

func (errBusy) Error() string { return "deploy in progress" }

func (a *Apps) recordDeploy(ctx context.Context, user auth.User, slug, ref string) (store.CreateDeployRow, error) {
	var out store.CreateDeployRow
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		app, err := q.LockOwnedApp(ctx, store.LockOwnedAppParams{Slug: slug, OwnerID: user.ID})
		if err != nil {
			return err
		}
		appID := app.ID
		if busy, err := q.HasUnfinishedDeploy(ctx, appID); err != nil {
			return fmt.Errorf("check unfinished deploys: %w", err)
		} else if busy {
			return errBusy{}
		}
		out, err = q.CreateDeploy(ctx, store.CreateDeployParams{AppID: appID, SourceRef: ref})
		if err != nil {
			return fmt.Errorf("create deploy: %w", err)
		}
		payload, err := json.Marshal(apps.DeployPayload{DeployID: out.ID.String()})
		if err != nil {
			return fmt.Errorf("encode payload: %w", err)
		}
		_, err = jobs.Enqueue(ctx, tx, store.EnqueueJobParams{
			Kind: apps.KindBuild, AppID: appID, Payload: payload, MaxAttempts: 3,
		})
		return err
	})
	return out, err
}
