//go:build integration

package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/store"
	"github.com/Uzair-23/shipyard/internal/testdb"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	url, stop, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	if err := store.Migrate(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()
	return m.Run()
}

type env struct {
	api     *Apps
	handler http.Handler
	dataDir string
	cookie  *http.Cookie // session of the default test user
}

var userSeq atomic.Int32

// serve sends req as the default test user.
func (e env) serve(rec *httptest.ResponseRecorder, req *http.Request) {
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	e.handler.ServeHTTP(rec, req)
}

// signup registers a new user and returns the session cookie.
func signup(t *testing.T, h http.Handler, email string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": "correct-horse-battery"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup %s: %d %s", email, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "shipyard_session" {
			return c
		}
	}
	t.Fatal("signup set no session cookie")
	return nil
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	a := NewApps(testPool, dir, "shipyard.localhost")
	h := NewRouter(fakePinger{}, a)
	email := fmt.Sprintf("user%d@example.com", userSeq.Add(1))
	return env{api: a, handler: h, dataDir: dir, cookie: signup(t, h, email)}
}

func targz(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := h.Name // reuse name as file content: content is irrelevant to the tests
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func goodArchive(t *testing.T, extra string) []byte {
	return targz(t, tar.Header{Name: "app.py"}, tar.Header{Name: "requirements.txt" + extra})
}

func (e env) createApp(t *testing.T, slug string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"slug": slug})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.serve(rec, req)
	return rec
}

func (e env) deploy(t *testing.T, slug, field string, archive []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if archive != nil {
		fw, err := mw.CreateFormFile(field, "src.tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(archive)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+slug+"/deploys", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.serve(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %q", rec.Body.String())
	}
	return body.Error.Code
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestCreateApp(t *testing.T) {
	e := newEnv(t)
	if rec := e.createApp(t, "create-ok"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	if rec := e.createApp(t, "create-ok"); rec.Code != http.StatusConflict || errCode(t, rec) != "slug_taken" {
		t.Fatalf("duplicate: status = %d body %s", rec.Code, rec.Body)
	}
	for _, slug := range []string{"ab", "Upper-case", "-lead", "trail-", "under_score", "www", strings.Repeat("a", 31), ""} {
		if rec := e.createApp(t, slug); rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_slug" {
			t.Errorf("slug %q: status = %d body %s", slug, rec.Code, rec.Body)
		}
	}
	bad := []struct{ name, ctype, body string }{
		{"not json type", "text/plain", `{"slug":"abcdef"}`},
		{"malformed", "application/json", `{`},
		{"unknown field", "application/json", `{"slug":"abcdef","owner":"x"}`},
		{"too big", "application/json", `{"slug":"` + strings.Repeat("a", 5000) + `"}`},
	}
	for _, b := range bad {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(b.body))
		req.Header.Set("Content-Type", b.ctype)
		rec := httptest.NewRecorder()
		e.serve(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", b.name, rec.Code)
		}
	}
}

func TestDeployQueuesBuildJob(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "deploy-ok")
	archive := goodArchive(t, "")

	rec := e.deploy(t, "deploy-ok", "source", archive)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Slug  string
		Seq   int
		State string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Seq != 1 || resp.State != "queued" || resp.Slug != "deploy-ok" {
		t.Fatalf("response = %s (%v)", rec.Body, err)
	}

	// The stored file is the normalised archive, named by its own hash.
	files := filesUnder(t, e.dataDir)
	if len(files) != 1 {
		t.Fatalf("files on disk = %v, want exactly one", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if filepath.Base(files[0]) != hex.EncodeToString(sum[:])+".tar.gz" {
		t.Errorf("file %s is not named by its sha256", files[0])
	}
	if bytes.Equal(data, archive) {
		t.Error("the raw upload was stored instead of the normalised archive")
	}

	var ref, kind, state string
	var attempts int32
	err = testPool.QueryRow(context.Background(), `
		SELECT d.source_ref, j.kind, j.state, j.max_attempts
		FROM deploys d JOIN apps a ON a.id = d.app_id
		JOIN jobs j ON j.payload->>'deploy_id' = d.id::text
		WHERE a.slug = 'deploy-ok'`).Scan(&ref, &kind, &state, &attempts)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "sha256:"+hex.EncodeToString(sum[:]) || kind != "build" || state != "queued" || attempts != 3 {
		t.Errorf("deploy/job = %s %s %s %d", ref, kind, state, attempts)
	}

	// A deploy in progress blocks the next one.
	if rec := e.deploy(t, "deploy-ok", "source", goodArchive(t, "x")); rec.Code != http.StatusConflict || errCode(t, rec) != "deploy_in_progress" {
		t.Fatalf("second deploy: status = %d body %s", rec.Code, rec.Body)
	}
	// The rejected upload's file is kept on purpose (content-addressed, another deploy
	// may use it; unreferenced sources are garbage collected in M9).
	if n := len(filesUnder(t, e.dataDir)); n != 2 {
		t.Errorf("files on disk = %d, want 2 (the first source and the rejected one)", n)
	}

	// Once finished, the next deploy gets seq 2; identical source reuses the file.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE deploys SET state = 'failed' WHERE app_id = (SELECT id FROM apps WHERE slug = 'deploy-ok')`); err != nil {
		t.Fatal(err)
	}
	rec = e.deploy(t, "deploy-ok", "source", archive)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"seq":2`) {
		t.Fatalf("redeploy: status = %d body %s", rec.Code, rec.Body)
	}
	if n := len(filesUnder(t, e.dataDir)); n != 2 {
		t.Errorf("identical source stored twice (%d files, want 2)", n)
	}
}

func TestDeployRejections(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "reject-me")

	if rec := e.deploy(t, "no-such-app", "source", goodArchive(t, "")); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app: status = %d", rec.Code)
	}
	if rec := e.deploy(t, "A", "source", goodArchive(t, "")); rec.Code != http.StatusNotFound {
		t.Errorf("invalid slug: status = %d", rec.Code)
	}
	if rec := e.deploy(t, "reject-me", "other", goodArchive(t, "")); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong field: status = %d", rec.Code)
	}
	if rec := e.deploy(t, "reject-me", "source", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no file: status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/reject-me/deploys", strings.NewReader("raw"))
	req.Header.Set("Content-Type", "application/gzip")
	rec := httptest.NewRecorder()
	e.serve(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("not multipart: status = %d", rec.Code)
	}

	attacks := map[string][]byte{
		"traversal":      targz(t, tar.Header{Name: "../evil.py"}),
		"absolute":       targz(t, tar.Header{Name: "/etc/passwd"}),
		"symlink escape": targz(t, tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc"}),
		"device":         targz(t, tar.Header{Name: "dev", Typeflag: tar.TypeChar}),
		"not gzip":       []byte("this is not an archive"),
	}
	for name, archive := range attacks {
		rec := e.deploy(t, "reject-me", "source", archive)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_upload" {
			t.Errorf("%s: status = %d body %s", name, rec.Code, rec.Body)
		}
	}

	e.api.Limits = build.Limits{MaxCompressedBytes: 1 << 10, MaxUnpackedBytes: 1 << 20, MaxEntries: 10, MaxOutputBytes: 1 << 20}
	big := targz(t, tar.Header{Name: strings.Repeat("a", 200)})
	pad := bytes.Repeat([]byte{0xAB}, 3<<20) // incompressible-ish trailing garbage past the body limit
	if rec := e.deploy(t, "reject-me", "source", append(big, pad...)); rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("oversized: status = %d", rec.Code)
	}

	if files := filesUnder(t, e.dataDir); len(files) != 0 {
		t.Errorf("rejected uploads left files: %v", files)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM deploys d JOIN apps a ON a.id = d.app_id WHERE a.slug = 'reject-me'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rejected uploads created %d deploys (%v)", n, err)
	}
}

func TestDeployGlobalUploadCap(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "cap-app")
	for i := 0; i < maxUploadConcurrency; i++ {
		e.api.uploads <- struct{}{}
	}
	rec := e.deploy(t, "cap-app", "source", goodArchive(t, ""))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	<-e.api.uploads
	if rec := e.deploy(t, "cap-app", "source", goodArchive(t, "")); rec.Code != http.StatusAccepted {
		t.Fatalf("after a slot frees: status = %d body %s", rec.Code, rec.Body)
	}
}

func TestConcurrentDeploysOfOneAppQueueOnlyOne(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "race-app")
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			codes <- e.deploy(t, "race-app", "source", goodArchive(t, fmt.Sprint(i))).Code
		}(i)
	}
	a, b := <-codes, <-codes
	if a+b != http.StatusAccepted+http.StatusConflict {
		t.Fatalf("codes = %d, %d; want one 202 and one 409", a, b)
	}
	// The loser must not delete the source the winner's build job will read.
	if n := len(filesUnder(t, e.dataDir)); n < 1 {
		t.Fatalf("source file missing after concurrent deploys (%d files)", n)
	}
	var ref string
	if err := testPool.QueryRow(context.Background(), `SELECT d.source_ref FROM deploys d JOIN apps a ON a.id = d.app_id WHERE a.slug = 'race-app'`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range filesUnder(t, e.dataDir) {
		found = found || filepath.Base(f) == strings.TrimPrefix(ref, "sha256:")+".tar.gz"
	}
	if !found {
		t.Fatalf("the winning deploy's source %s is not on disk", ref)
	}
}

func TestCrossOriginRequestsRefused(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "origin-app")
	for _, origin := range []string{
		"http://evil.example", "http://origin-app.apps.localhost", "http://shipyard.localhost.evil.example",
		"null", "file://shipyard.localhost", "http://localhost",
	} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("source", "s.tar.gz")
		_, _ = fw.Write(goodArchive(t, ""))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/origin-app/deploys", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		e.serve(rec, req)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden_origin" {
			t.Errorf("Origin %q: status = %d", origin, rec.Code)
		}
	}
	if n := len(filesUnder(t, e.dataDir)); n != 0 {
		t.Errorf("cross-origin deploys left %d files", n)
	}
	// The dashboard origin (any port) and clients without an Origin header are fine.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(`{"slug":"origin-ok"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://shipyard.localhost:5173")
	rec := httptest.NewRecorder()
	e.serve(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("dashboard origin: status = %d body %s", rec.Code, rec.Body)
	}
}
