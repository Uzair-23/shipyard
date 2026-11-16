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
	server   *httptest.Server
	pipeline *apps.Pipeline
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
	return stack{server: srv, pipeline: pipeline}
}

func (s stack) post(t *testing.T, path, contentType string, body io.Reader) (int, []byte) {
	t.Helper()
	resp, err := http.Post(s.server.URL+path, contentType, body)
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
		})
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
