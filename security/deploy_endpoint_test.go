//go:build integration

package security

// The upload attacks of upload_test.go, sent through the real deploy endpoint:
// each must be answered 400, leave nothing on disk, create no deploy and queue
// no build job.

import (
	"archive/tar"
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/api"
	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error
)

// endpointPool starts one Postgres for the whole package, on first use.
// It is left to the test process exit and Ryuk to clean up.
func endpointPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbOnce.Do(func() {
		ctx := context.Background()
		url, _, err := testdb.Start(ctx)
		if err != nil {
			dbErr = err
			return
		}
		if dbErr = store.Migrate(ctx, url); dbErr != nil {
			return
		}
		dbPool, dbErr = pgxpool.New(ctx, url)
	})
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	return dbPool
}

func TestDeployEndpointRejectsMaliciousUploads(t *testing.T) {
	pool := endpointPool(t)
	dataDir := t.TempDir()
	h := api.NewRouter(nil, api.NewApps(pool, dataDir, "shipyard.localhost"))

	create := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(`{"slug":"victim-app"}`))
	create.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("could not create the app: %d %s", rec.Code, rec.Body)
	}

	attacks := []struct {
		name    string
		entries []tarEntry
	}{
		{"parent traversal", []tarEntry{{hdr: tar.Header{Name: "../../evil", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}}},
		{"nested traversal", []tarEntry{{hdr: tar.Header{Name: "a/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}}},
		{"absolute path", []tarEntry{{hdr: tar.Header{Name: "/etc/cron.d/evil", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}}},
		{"symlink escape", []tarEntry{{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../../../etc"}}}},
		{"write through symlink", []tarEntry{
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "."}},
			{hdr: tar.Header{Name: "l/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		}},
		{"device file", []tarEntry{{hdr: tar.Header{Name: "sda", Typeflag: tar.TypeBlock}}}},
	}
	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			fw, _ := mw.CreateFormFile("source", "x.tar.gz")
			_, _ = fw.Write(evilArchive(t, a.entries...))
			_ = mw.Close()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/victim-app/deploys", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
			assertNothingStored(t, pool, dataDir)
		})
	}
}

func assertNothingStored(t *testing.T, pool *pgxpool.Pool, dataDir string) {
	t.Helper()
	_ = filepath.WalkDir(dataDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("file left on disk: %s", p)
		}
		return nil
	})
	var deploys, jobs int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM deploys d JOIN apps a ON a.id = d.app_id WHERE a.slug = 'victim-app'),
		(SELECT count(*) FROM jobs)`).Scan(&deploys, &jobs); err != nil {
		t.Fatal(err)
	}
	if deploys != 0 || jobs != 0 {
		t.Errorf("rejected upload created %d deploys and %d jobs", deploys, jobs)
	}
}

// A web page (or an app served from *.apps.*) must not be able to deploy code
// by making the visitor's browser POST a multipart form: it is a CORS "simple"
// request, so only the Origin check stops it.
func TestDeployEndpointRefusesCrossSiteBrowserRequests(t *testing.T) {
	pool := endpointPool(t)
	dataDir := t.TempDir()
	h := api.NewRouter(nil, api.NewApps(pool, dataDir, "shipyard.localhost"))
	for _, origin := range []string{"http://evil.example", "http://victim-app.apps.localhost", "null"} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("source", "x.tar.gz")
		_, _ = fw.Write(evilArchive(t, reg("app.py")))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/victim-app/deploys", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: status = %d, want 403", origin, rec.Code)
		}
	}
}
