//go:build integration

// Package e2e tests the deploy path end to end against real Postgres, Docker
// and a registry: upload a sample app through the HTTP API, let the worker
// build and start it, and request it.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/api"
	"github.com/Uzair-23/shipyard/internal/apps"
	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/jobs"
	"github.com/Uzair-23/shipyard/internal/router"
	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var (
	testPool     *pgxpool.Pool
	testDocker   *client.Client
	testRegistry string
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	url, stopDB, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stopDB()
	if err := store.Migrate(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, url+"&pool_max_conns=24"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()
	var stopRegistry func()
	if testRegistry, stopRegistry, err = testdb.StartRegistry(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stopRegistry()
	if testDocker, err = client.New(client.FromEnv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = testDocker.Close() }()
	return m.Run()
}

// packDir returns a .tar.gz of dir's files, as a user would upload.
func packDir(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stack runs the API and a worker (build + start handlers) sharing one data dir.
type stack struct {
	cookie    *http.Cookie // session of the test user
	server    *httptest.Server
	pipeline  *apps.Pipeline
	routerURL string // the router, reached with a Host header
}

func newStack(t *testing.T) stack {
	t.Helper()
	dataDir := t.TempDir()
	engine, err := runtime.NewEngine(context.Background(), testDocker)
	if err != nil {
		t.Fatal(err)
	}
	masterKey := bytes.Repeat([]byte{4}, 32)
	pipeline := &apps.Pipeline{
		MasterKey: masterKey,
		Pool:      testPool,
		Builder:   &build.Builder{Docker: testDocker, Registry: testRegistry},
		Engine:    engine,
		DataDir:   dataDir,
	}
	apiApps := api.NewApps(testPool, dataDir, "shipyard.localhost")
	apiApps.MasterKey = masterKey
	srv := httptest.NewServer(api.NewRouter(nil, apiApps))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	w := &jobs.Worker{Pool: testPool, Concurrency: 2, Handlers: pipeline.Handlers(), ID: "e2e-worker", PollInterval: 200 * time.Millisecond}
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	})

	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rdone := make(chan error, 1)
	rt := router.New(testPool, "apps.localhost", "")
	rt.BaseDomain = "shipyard.localhost"
	rt.CookieKey = []byte("e2e-cookie-key-e2e-cookie-key-xx")
	rt.AccessCacheTTL = 300 * time.Millisecond
	go func() { rdone <- rt.Serve(ctx, rln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-rdone; err != nil {
			t.Errorf("router: %v", err)
		}
	})
	st := stack{server: srv, pipeline: pipeline, routerURL: "http://" + rln.Addr().String()}
	st.cookie = st.signup(t, fmt.Sprintf("e2e-%d@example.com", time.Now().UnixNano()))
	return st
}

func (s stack) signup(t *testing.T, email string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": "correct-horse-battery"})
	resp, err := http.Post(s.server.URL+"/api/v1/auth/signup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == "shipyard_session" && resp.StatusCode == http.StatusCreated {
			return c
		}
	}
	t.Fatalf("signup failed: %d", resp.StatusCode)
	return nil
}

func (s stack) post(t *testing.T, path, contentType string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(s.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// cleanupApp removes the app's containers and images, which the worker leaves running.
func cleanupApp(t *testing.T, s stack, slug string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		var appID string
		if err := testPool.QueryRow(ctx, `SELECT id::text FROM apps WHERE slug = $1`, slug).Scan(&appID); err != nil {
			return
		}
		ctrs, _ := testDocker.ContainerList(ctx, client.ContainerListOptions{
			All: true, Filters: make(client.Filters).Add("label", runtime.LabelAppID+"="+appID),
		})
		for _, c := range ctrs.Items {
			_, _ = testDocker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		}
		imgs, _ := testDocker.ImageList(ctx, client.ImageListOptions{
			Filters: make(client.Filters).Add("reference", s.pipeline.Builder.Repository(appID)+"*"),
		})
		for _, img := range imgs.Items {
			_, _ = testDocker.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		}
	})
}

var runCounter atomic.Int32

// deployTimeout is generous for slow CI runners; PRD §7.1 targets 90 s p95.
const deployTimeout = 3 * time.Minute

func TestSampleAppsDeployAndServe(t *testing.T) {
	samples := []struct{ dir, slug, want string }{
		{"hello-python", "e2e-hello-python", "Hello from Shipyard (python)"},
		{"hello-node", "e2e-hello-node", "Hello from Shipyard (node)"},
		{"static-site", "e2e-static-site", "Hello from Shipyard (static)"},
	}
	s := newStack(t)
	run := runCounter.Add(1) // unique slugs, so the test can run with -count > 1
	for _, sample := range samples {
		sample.slug = fmt.Sprintf("%s-%d", sample.slug, run)
		t.Run(sample.dir, func(t *testing.T) {
			cleanupApp(t, s, sample.slug)

			body, _ := json.Marshal(map[string]string{"slug": sample.slug})
			if code, resp := s.post(t, "/api/v1/apps", "application/json", bytes.NewReader(body)); code != http.StatusCreated {
				t.Fatalf("create app: %d %s", code, resp)
			}
			s.makePublic(t, sample.slug) // these tests are about building and serving; access has its own test

			var form bytes.Buffer
			mw := multipart.NewWriter(&form)
			fw, err := mw.CreateFormFile("source", sample.dir+".tar.gz")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write(packDir(t, filepath.Join("..", "testdata", "apps", sample.dir)))
			_ = mw.Close()
			start := time.Now()
			if code, resp := s.post(t, "/api/v1/apps/"+sample.slug+"/deploys", mw.FormDataContentType(), &form); code != http.StatusAccepted {
				t.Fatalf("deploy: %d %s", code, resp)
			}

			state, deployErr := waitFinished(t, sample.slug)
			elapsed := time.Since(start)
			if state != "live" {
				t.Fatalf("deploy ended %q after %s: %s", state, elapsed.Round(time.Second), deployErr)
			}
			t.Logf("%s live after %s", sample.dir, elapsed.Round(100*time.Millisecond))

			var port int
			var status string
			err = testPool.QueryRow(context.Background(), `
				SELECT i.host_port, a.status FROM apps a
				JOIN deploys d ON d.id = a.current_deploy_id
				JOIN instances i ON i.deploy_id = d.id AND i.state = 'running'
				WHERE a.slug = $1`, sample.slug).Scan(&port, &status)
			if err != nil {
				t.Fatalf("find running instance: %v", err)
			}
			if status != "running" {
				t.Errorf("app status = %q, want running", status)
			}
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if err != nil {
				t.Fatalf("GET app: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			page, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), sample.want) {
				t.Fatalf("GET / = %d %q, want 200 containing %q", resp.StatusCode, page, sample.want)
			}

			// The same app through the router, by Host header. The route arrives via
			// NOTIFY route_changes from the worker's promote step (no manual reload).
			via := getViaRouter(t, s.routerURL, sample.slug)
			if via.status != http.StatusOK || !strings.Contains(via.body, sample.want) {
				t.Fatalf("via router = %d %q, want 200 containing %q", via.status, via.body, sample.want)
			}
		})
	}
}

type routed struct {
	status int
	body   string
}

// getViaRouter requests / from the app's host name, retrying briefly while the
// route notification propagates.
func getViaRouter(t *testing.T, routerURL, slug string) routed {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodGet, routerURL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = slug + ".apps.localhost"
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || time.Now().After(deadline) {
				return routed{resp.StatusCode, string(body)}
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("GET via router: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitFinished polls until the app's latest deploy is live or failed.
func waitFinished(t *testing.T, slug string) (state, errText string) {
	t.Helper()
	deadline := time.Now().Add(deployTimeout)
	for {
		err := testPool.QueryRow(context.Background(), `
			SELECT d.state, coalesce(d.error, '') FROM deploys d JOIN apps a ON a.id = d.app_id
			WHERE a.slug = $1 ORDER BY d.seq DESC LIMIT 1`, slug).Scan(&state, &errText)
		if err != nil {
			t.Fatalf("read deploy state: %v", err)
		}
		if state == "live" || state == "failed" {
			return state, errText
		}
		if time.Now().After(deadline) {
			t.Fatalf("deploy still %q after %s", state, deployTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// api sends a JSON request as the given session (nil = anonymous).
func (s stack) api(t *testing.T, cookie *http.Cookie, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.server.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (s stack) waitStatus(t *testing.T, slug, want string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, body := s.api(t, s.cookie, http.MethodGet, "/api/v1/apps/"+slug, nil)
		var app struct{ Status string }
		if code == http.StatusOK && json.Unmarshal(body, &app) == nil {
			if last = app.Status; last == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("app %s did not reach status %q (last %q)", slug, want, last)
}

// waitRouter polls the app through the router until it answers want.
func waitRouter(t *testing.T, routerURL, slug string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := 0
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, routerURL+"/", nil)
		req.Host = slug + ".apps.localhost"
		if resp, err := http.DefaultClient.Do(req); err == nil {
			last = resp.StatusCode
			_ = resp.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("router answered %d for %s, want %d", last, slug, want)
}

func (s stack) containerIDs(t *testing.T, slug string) []string {
	t.Helper()
	var appID string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM apps WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&appID); err != nil {
		return nil
	}
	return s.containersOf(t, appID)
}

func (s stack) containersOf(t *testing.T, appID string) []string {
	t.Helper()
	res, err := testDocker.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", runtime.LabelAppID+"="+appID),
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(res.Items))
	for i, c := range res.Items {
		ids[i] = c.ID
	}
	return ids
}

// The user-visible lifecycle through the API, worker and router: stop, start,
// restart and delete, with another user refused throughout.
func TestAppLifecycleThroughAPI(t *testing.T) {
	s := newStack(t)
	slug := fmt.Sprintf("e2e-life-%d", runCounter.Add(1))
	cleanupApp(t, s, slug)
	const want = "Hello from Shipyard (static)"

	if code, resp := s.post(t, "/api/v1/apps", "application/json", strings.NewReader(`{"slug":"`+slug+`"}`)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, resp)
	}
	s.makePublic(t, slug)
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("source", "static.tar.gz")
	_, _ = fw.Write(packDir(t, filepath.Join("..", "testdata", "apps", "static-site")))
	_ = mw.Close()
	if code, resp := s.post(t, "/api/v1/apps/"+slug+"/deploys", mw.FormDataContentType(), &form); code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", code, resp)
	}
	if state, e := waitFinished(t, slug); state != "live" {
		t.Fatalf("deploy %s: %s", state, e)
	}
	if got := getViaRouter(t, s.routerURL, slug); got.status != http.StatusOK || !strings.Contains(got.body, want) {
		t.Fatalf("live app via router = %d %q", got.status, got.body)
	}

	other := s.signup(t, fmt.Sprintf("other-%d@example.com", time.Now().UnixNano()))
	for _, action := range []string{"stop", "start", "restart"} {
		if code, _ := s.api(t, other, http.MethodPost, "/api/v1/apps/"+slug+"/"+action, nil); code != http.StatusNotFound {
			t.Errorf("another user's %s = %d, want 404", action, code)
		}
	}
	if code, _ := s.api(t, other, http.MethodDelete, "/api/v1/apps/"+slug, nil); code != http.StatusNotFound {
		t.Errorf("another user's delete = %d, want 404", code)
	}

	// Stop: container gone, router shows "not running", the deploy stays live.
	if code, resp := s.api(t, s.cookie, http.MethodPost, "/api/v1/apps/"+slug+"/stop", nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", code, resp)
	}
	s.waitStatus(t, slug, "stopped")
	waitRouter(t, s.routerURL, slug, http.StatusServiceUnavailable)
	if ids := s.containerIDs(t, slug); len(ids) != 0 {
		t.Fatalf("containers after stop: %v", ids)
	}

	// Start: back, from the same live deploy.
	if code, resp := s.api(t, s.cookie, http.MethodPost, "/api/v1/apps/"+slug+"/start", nil); code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, resp)
	}
	s.waitStatus(t, slug, "running")
	waitRouter(t, s.routerURL, slug, http.StatusOK)
	before := s.containerIDs(t, slug)
	if len(before) != 1 {
		t.Fatalf("containers after start: %v", before)
	}

	// Restart: a different container, serving again.
	if code, resp := s.api(t, s.cookie, http.MethodPost, "/api/v1/apps/"+slug+"/restart", nil); code != http.StatusAccepted {
		t.Fatalf("restart: %d %s", code, resp)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if ids := s.containerIDs(t, slug); len(ids) == 1 && ids[0] != before[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart did not replace the container (still %v)", s.containerIDs(t, slug))
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.waitStatus(t, slug, "running")
	waitRouter(t, s.routerURL, slug, http.StatusOK)

	// Delete: gone from the API and router, container removed, slug free again.
	var appID string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM apps WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if code, resp := s.api(t, s.cookie, http.MethodDelete, "/api/v1/apps/"+slug, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, resp)
	}
	if code, _ := s.api(t, s.cookie, http.MethodGet, "/api/v1/apps/"+slug, nil); code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", code)
	}
	waitRouter(t, s.routerURL, slug, http.StatusNotFound)
	deadline = time.Now().Add(30 * time.Second)
	for len(s.containersOf(t, appID)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("containers after delete: %v", s.containersOf(t, appID))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code, resp := s.post(t, "/api/v1/apps", "application/json", strings.NewReader(`{"slug":"`+slug+`"}`)); code != http.StatusCreated {
		t.Fatalf("re-creating the deleted slug: %d %s", code, resp)
	}
	if code, resp := s.api(t, s.cookie, http.MethodPost, "/api/v1/apps/"+slug+"/start", nil); code != http.StatusConflict {
		t.Fatalf("start of an app that was never deployed = %d %s, want 409", code, resp)
	}
}

func (s stack) makePublic(t *testing.T, slug string) {
	t.Helper()
	if code, body := s.api(t, s.cookie, http.MethodPatch, "/api/v1/apps/"+slug, map[string]string{"visibility": "public"}); code != http.StatusOK {
		t.Fatalf("make %s public: %d %s", slug, code, body)
	}
}

// A browser would follow redirects across hosts; the test drives each hop itself.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// viaRouter sends a request to the router as <slug>.apps.localhost.
func (s stack) viaRouter(t *testing.T, slug, path string, hdr map[string]string, cookies ...*http.Cookie) (int, http.Header, []byte, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.routerURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = slug + ".apps.localhost"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("GET %s via router: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body, resp.Cookies()
}

// viaAPI sends a GET to the API (a browser on shipyard.localhost) with an optional session.
func (s stack) viaAPI(t *testing.T, pathAndQuery string, session *http.Cookie) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.server.URL+pathAndQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		req.AddCookie(session)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", pathAndQuery, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// handoff runs the browser's steps from the router redirect to holding an
// _sy_app cookie: router -> /auth/app -> router callback. It returns the
// cookie, or nil and the status where it stopped.
func (s stack) handoff(t *testing.T, slug string, session *http.Cookie) (*http.Cookie, int) {
	t.Helper()
	code, hdr, _, _ := s.viaRouter(t, slug, "/", nil)
	if code != http.StatusFound {
		t.Fatalf("router without a cookie: %d, want 302", code)
	}
	loc, err := url.Parse(hdr.Get("Location"))
	if err != nil || loc.Host != "shipyard.localhost" || loc.Path != "/auth/app" {
		t.Fatalf("router redirected to %q", hdr.Get("Location"))
	}
	code, hdr, _ = s.viaAPI(t, loc.RequestURI(), session)
	if code != http.StatusFound {
		return nil, code
	}
	cb, err := url.Parse(hdr.Get("Location"))
	if err != nil || cb.Host != slug+".apps.localhost" || cb.Path != "/__shipyard/callback" {
		t.Fatalf("handoff redirected to %q", hdr.Get("Location"))
	}
	code, hdr, _, cookies := s.viaRouter(t, slug, cb.RequestURI(), nil)
	if code != http.StatusFound || hdr.Get("Location") != "/" {
		t.Fatalf("callback: %d %q", code, hdr.Get("Location"))
	}
	for _, c := range cookies {
		if c.Name == "_sy_app" {
			return c, code
		}
	}
	t.Fatal("callback set no _sy_app cookie")
	return nil, 0
}

type whoamiReply struct {
	UserEmail string            `json:"user_email"`
	UserID    string            `json:"user_id"`
	Cookies   map[string]string `json:"cookies"`
	Headers   map[string]any    `json:"headers"`
	Env       map[string]string `json:"env"`
}

func (s stack) deployDir(t *testing.T, slug, dir string) {
	t.Helper()
	if code, resp := s.post(t, "/api/v1/apps", "application/json", strings.NewReader(`{"slug":"`+slug+`"}`)); code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", slug, code, resp)
	}
	s.uploadDir(t, slug, dir)
}

func (s stack) uploadDir(t *testing.T, slug, dir string) {
	t.Helper()
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("source", dir+".tar.gz")
	_, _ = fw.Write(packDir(t, filepath.Join("..", "testdata", "apps", dir)))
	_ = mw.Close()
	if code, resp := s.post(t, "/api/v1/apps/"+slug+"/deploys", mw.FormDataContentType(), &form); code != http.StatusAccepted {
		t.Fatalf("deploy %s: %d %s", slug, code, resp)
	}
}

// The core of the product: a private app, shared with one person, through the
// auth handoff, and what the app itself is allowed to see.
func TestPrivateAppAuthHandoff(t *testing.T) {
	s := newStack(t)
	slug := fmt.Sprintf("e2e-priv-%d", runCounter.Add(1))
	cleanupApp(t, s, slug)
	s.deployDir(t, slug, "whoami")
	if state, e := waitFinished(t, slug); state != "live" {
		t.Fatalf("deploy %s: %s", state, e)
	}
	ownerEmail := s.emailOf(t, s.cookie)

	// 1. Anonymous: the handoff sends the browser to the dashboard login.
	if code, hdr, _ := s.viaAPI(t, "/auth/app?app="+slug, nil); code != http.StatusFound || !strings.HasPrefix(hdr.Get("Location"), "/login?next=") {
		t.Fatalf("anonymous handoff: %d %q", code, hdr.Get("Location"))
	}

	// 2. The owner passes the handoff and reaches the app.
	ownerCookie, _ := s.handoff(t, slug, s.cookie)
	spoof := map[string]string{"X-Shipyard-User-Email": "admin@evil.example", "X-Shipyard-User-Id": "evil"}
	code, _, body, _ := s.viaRouter(t, slug, "/", spoof, ownerCookie, &http.Cookie{Name: "theme", Value: "dark"})
	if code != http.StatusOK {
		t.Fatalf("owner via router: %d %s", code, body)
	}
	var who whoamiReply
	if err := json.Unmarshal(body, &who); err != nil {
		t.Fatalf("whoami reply: %v %s", err, body)
	}
	if who.UserEmail != ownerEmail || who.UserID == "" || who.UserID == "evil" {
		t.Errorf("the app sees user %q / %q, want the owner %q (spoofed headers must be overwritten)", who.UserEmail, who.UserID, ownerEmail)
	}
	if _, ok := who.Cookies["_sy_app"]; ok {
		t.Error("the app received the _sy_app cookie")
	}
	if _, ok := who.Cookies["shipyard_session"]; ok {
		t.Error("the app received the dashboard session cookie")
	}
	if who.Cookies["theme"] != "dark" {
		t.Errorf("ordinary cookies must reach the app: %v", who.Cookies)
	}

	// 3. A handoff code works once.
	_, hdr, _ := s.viaAPI(t, "/auth/app?app="+slug+"&return=%2Fx", s.cookie)
	cb, _ := url.Parse(hdr.Get("Location"))
	if code, _, _, _ := s.viaRouter(t, slug, cb.RequestURI(), nil); code != http.StatusFound {
		t.Fatalf("first use of the code: %d", code)
	}
	if code, _, _, cookies := s.viaRouter(t, slug, cb.RequestURI(), nil); code != http.StatusForbidden || len(cookies) != 0 {
		t.Fatalf("second use of the code: %d, cookies %v", code, cookies)
	}

	// 4. A stranger is refused; once the owner adds them they are in.
	friendEmail := fmt.Sprintf("friend-%d@example.com", time.Now().UnixNano())
	friend := s.signupAs(t, friendEmail)
	if code, _, body := s.viaAPI(t, "/auth/app?app="+slug, friend); code != http.StatusForbidden || !strings.Contains(string(body), "No access") {
		t.Fatalf("stranger: %d %s", code, body)
	}
	if code, resp := s.api(t, s.cookie, http.MethodPost, "/api/v1/apps/"+slug+"/members", map[string]string{"email": strings.ToUpper(friendEmail)}); code != http.StatusCreated {
		t.Fatalf("add member: %d %s", code, resp)
	}
	friendCookie, _ := s.handoff(t, slug, friend)
	code, _, body, _ = s.viaRouter(t, slug, "/", nil, friendCookie)
	if err := json.Unmarshal(body, &who); code != http.StatusOK || err != nil || who.UserEmail != friendEmail {
		t.Fatalf("member via router: %d %s", code, body)
	}

	// 5. The owner's cookie is not the friend's identity, and the friend's cookie opens only this app.
	if code, _, _, _ := s.viaRouter(t, slug, "/", nil, &http.Cookie{Name: "_sy_app", Value: "forged"}); code != http.StatusFound {
		t.Errorf("forged cookie: %d, want 302", code)
	}

	// 6. Removing the member ends their access (once the router's cache expires).
	if code, resp := s.api(t, s.cookie, http.MethodDelete, "/api/v1/apps/"+slug+"/members/"+url.PathEscape(friendEmail), nil); code != http.StatusNoContent {
		t.Fatalf("remove member: %d %s", code, resp)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _, _, _ := s.viaRouter(t, slug, "/", nil, friendCookie); code == http.StatusFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removed member still reaches the app")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code, _, _ := s.viaAPI(t, "/auth/app?app="+slug, friend); code != http.StatusForbidden {
		t.Errorf("removed member's handoff: %d, want 403", code)
	}

	// 7. Public: anyone can use the app, and an anonymous visitor has no identity.
	s.makePublic(t, slug)
	deadline = time.Now().Add(10 * time.Second)
	for {
		code, _, body, _ := s.viaRouter(t, slug, "/", spoof)
		if code == http.StatusOK {
			var anon whoamiReply // fresh: unmarshalling JSON null keeps an old value
			if err := json.Unmarshal(body, &anon); err != nil || anon.UserEmail != "" || anon.UserID != "" {
				t.Fatalf("anonymous visitor of a public app has identity %q / %q (%v)", anon.UserEmail, anon.UserID, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public app still answers %d", code)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 8. Environment variables and secrets reach the running app; a change restarts it.
	const secret = "e2e-secret-value-123"
	if code, resp := s.api(t, s.cookie, http.MethodPut, "/api/v1/apps/"+slug+"/env/DEMO_SECRET", map[string]any{"value": secret, "is_secret": true}); code != http.StatusOK || strings.Contains(string(resp), secret) {
		t.Fatalf("put secret: %d %s", code, resp)
	}
	if code, resp := s.api(t, s.cookie, http.MethodPut, "/api/v1/apps/"+slug+"/env/DEMO_PLAIN", map[string]any{"value": "visible-value"}); code != http.StatusOK {
		t.Fatalf("put plain: %d %s", code, resp)
	}
	s.waitEnv(t, slug, func(env map[string]string) bool {
		return env["DEMO_SECRET"] == secret && env["DEMO_PLAIN"] == "visible-value"
	})
	if code, resp := s.api(t, s.cookie, http.MethodGet, "/api/v1/apps/"+slug+"/env", nil); code != http.StatusOK || strings.Contains(string(resp), secret) || !strings.Contains(string(resp), "visible-value") {
		t.Fatalf("list env: %d %s", code, resp)
	}
	if code, resp := s.api(t, s.cookie, http.MethodDelete, "/api/v1/apps/"+slug+"/env/DEMO_SECRET", nil); code != http.StatusNoContent {
		t.Fatalf("delete secret: %d %s", code, resp)
	}
	s.waitEnv(t, slug, func(env map[string]string) bool {
		_, has := env["DEMO_SECRET"]
		return !has && env["DEMO_PLAIN"] == "visible-value"
	})
}

// waitEnv polls the public whoami app until its environment satisfies ok (the app restarts in between).
func (s stack) waitEnv(t *testing.T, slug string, ok func(map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, _, body, _ := s.viaRouter(t, slug, "/", nil)
		var who whoamiReply
		if code == http.StatusOK && json.Unmarshal(body, &who) == nil {
			last = fmt.Sprint(who.Env)
			if ok(who.Env) {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the app's environment never matched (last %s)", last)
}

func (s stack) emailOf(t *testing.T, session *http.Cookie) string {
	t.Helper()
	code, body := s.api(t, session, http.MethodGet, "/api/v1/me", nil)
	var me struct{ Email string }
	if code != http.StatusOK || json.Unmarshal(body, &me) != nil {
		t.Fatalf("me: %d %s", code, body)
	}
	return me.Email
}

func (s stack) signupAs(t *testing.T, email string) *http.Cookie { return s.signup(t, email) }
