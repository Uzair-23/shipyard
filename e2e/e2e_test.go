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
	pipeline := &apps.Pipeline{
		Pool:    testPool,
		Builder: &build.Builder{Docker: testDocker, Registry: testRegistry},
		Engine:  engine,
		DataDir: dataDir,
	}
	srv := httptest.NewServer(api.NewRouter(nil, api.NewApps(testPool, dataDir, "shipyard.localhost")))
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
