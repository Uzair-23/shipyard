//go:build integration

package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/build"
	"github.com/Uzair-23/shipyard/internal/runtime"
	"github.com/Uzair-23/shipyard/internal/store"
)

func startJob(deploy pgtype.UUID) store.Job {
	payload, _ := json.Marshal(DeployPayload{DeployID: deploy.String()})
	return store.Job{Kind: KindStart, Payload: payload, MaxAttempts: 3}
}

func builtDeploy(t *testing.T, f fixture, files map[string]string) pgtype.UUID {
	t.Helper()
	id := f.upload(t, 1, files)
	if err := f.pipeline.Build(context.Background(), buildJob(t, id)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if d := getDeploy(t, id); d.state != "built" {
		t.Fatalf("deploy not built: %+v", d)
	}
	return id
}

type instanceRow struct {
	id, containerID, state string
	hostPort               int32
}

func instancesFor(t *testing.T, deploy pgtype.UUID) []instanceRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT id::text, coalesce(container_id, ''), state, coalesce(host_port, 0) FROM instances WHERE deploy_id = $1`, deploy)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []instanceRow
	for rows.Next() {
		var r instanceRow
		if err := rows.Scan(&r.id, &r.containerID, &r.state, &r.hostPort); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func getURL(t *testing.T, url string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:gosec // test-only loopback URL
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestStartRunsHardenedContainer(t *testing.T) {
	f := newFixture(t, "start-static")
	id := builtDeploy(t, f, map[string]string{"index.html": "<h1>started</h1>"})

	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d := getDeploy(t, id); d.state != "live" {
		t.Fatalf("deploy state = %s, want live", d.state)
	}
	if st, cur := appState(t, f); st != "running" || cur != id.String() {
		t.Fatalf("app status=%q current=%q, want running/%s", st, cur, id.String())
	}
	ins := instancesFor(t, id)
	if len(ins) != 1 || ins[0].containerID == "" || ins[0].hostPort == 0 || ins[0].state != "running" {
		t.Fatalf("instances = %+v", ins)
	}

	// What the daemon actually applied, not just what we asked for.
	res, err := testDocker.ContainerInspect(context.Background(), ins[0].containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Container
	hc := c.HostConfig
	checks := []struct {
		name string
		ok   bool
	}{
		{"running", c.State != nil && c.State.Running},
		{"user 10001", c.Config.User == "10001:10001"},
		{"image by digest", strings.Contains(c.Config.Image, "@sha256:")},
		{"not privileged", !hc.Privileged},
		{"cap-drop ALL", slices.Equal(hc.CapDrop, []string{"ALL"}) && len(hc.CapAdd) == 0},
		{"no-new-privileges", slices.Contains(hc.SecurityOpt, "no-new-privileges:true")},
		{"read-only rootfs", hc.ReadonlyRootfs},
		{"tmpfs /tmp", strings.Contains(hc.Tmpfs["/tmp"], "noexec") && strings.Contains(hc.Tmpfs["/tmp"], "size=64m")},
		// HostConfig.Tmpfs mounts are not listed in Mounts: no bind mounts or volumes at all.
		{fmt.Sprintf("no binds/mounts (got binds=%v mounts=%+v)", hc.Binds, c.Mounts), len(hc.Binds) == 0 && len(c.Mounts) == 0},
		{"memory 256 MiB, no swap", hc.Memory == 256<<20 && hc.MemorySwap == 256<<20},
		{"0.5 CPU", hc.NanoCPUs == 500_000_000},
		{"pids 128", hc.PidsLimit != nil && *hc.PidsLimit == 128},
		{"restart no", hc.RestartPolicy.Name == "no"},
		{"labels", c.Config.Labels[runtime.LabelDeployID] == id.String() && c.Config.Labels[runtime.LabelInstanceID] == ins[0].id},
	}
	for _, ch := range checks {
		if !ch.ok {
			t.Errorf("container violates: %s", ch.name)
		}
	}
	for _, b := range c.NetworkSettings.Ports {
		for _, pb := range b {
			if !pb.HostIP.IsLoopback() {
				t.Errorf("port published on %s, want loopback only", pb.HostIP)
			}
		}
	}

	code, body := getURL(t, fmt.Sprintf("http://127.0.0.1:%d/", ins[0].hostPort))
	if code != http.StatusOK || !strings.Contains(body, "started") {
		t.Fatalf("GET / = %d %q", code, body)
	}

	// Idempotent: a retried start reuses the same instance and container.
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	again := instancesFor(t, id)
	if len(again) != 1 || again[0].containerID != ins[0].containerID {
		t.Fatalf("retry created a new instance/container: %+v", again)
	}
	ctrs, err := testDocker.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", runtime.LabelDeployID+"="+id.String()),
	})
	if err != nil || len(ctrs.Items) != 1 {
		t.Fatalf("%d containers for the deploy, want 1 (err %v)", len(ctrs.Items), err)
	}
}

func TestStartIgnoresDeployThatIsNotBuilt(t *testing.T) {
	f := newFixture(t, "start-not-built")
	id := f.upload(t, 1, map[string]string{"index.html": "x"}) // still queued
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d := getDeploy(t, id); d.state != "queued" || len(instancesFor(t, id)) != 0 {
		t.Fatalf("deploy = %+v, instances = %v", d, instancesFor(t, id))
	}
}

// After local image cleanup the start job pulls the image back from the
// registry by digest.
func TestStartPullsMissingImage(t *testing.T) {
	f := newFixture(t, "start-pull")
	id := builtDeploy(t, f, map[string]string{"index.html": "<p>pulled</p>"})
	digest := getDeploy(t, id).digest
	for _, ref := range []string{digest, f.pipeline.Builder.Repository(f.appID.String()) + ":1"} {
		_, _ = testDocker.ImageRemove(context.Background(), ref, client.ImageRemoveOptions{Force: true})
	}
	if _, err := testDocker.ImageInspect(context.Background(), digest); err == nil {
		t.Fatal("image still present locally")
	}
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ins := instancesFor(t, id)
	code, body := getURL(t, fmt.Sprintf("http://127.0.0.1:%d/", ins[0].hostPort))
	if code != http.StatusOK || !strings.Contains(body, "pulled") {
		t.Fatalf("GET / = %d %q", code, body)
	}
}

func inspectMounts(t *testing.T, containerID string) (mounts int, tmpfs map[string]string) {
	t.Helper()
	res, err := testDocker.ContainerInspect(context.Background(), containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(res.Container.Mounts), res.Container.HostConfig.Tmpfs
}

// All three real templates start with no writable mounts other than tmpfs,
// and serve on $PORT. Dependency files are empty so no network is needed.
func TestStartEachTemplate(t *testing.T) {
	sources := map[string]map[string]string{
		"python": {
			"requirements.txt": "",
			"app.py": "import http.server, os\n" +
				"http.server.HTTPServer(('0.0.0.0', int(os.environ['PORT'])), http.server.SimpleHTTPRequestHandler).serve_forever()\n",
		},
		"node": {
			"package.json": `{"name":"t","version":"1.0.0","private":true,"scripts":{"start":"node server.js"}}`,
			"server.js":    "require('http').createServer((q, s) => s.end('node ok')).listen(process.env.PORT)\n",
		},
		"static": {"index.html": "static ok"},
	}
	for name, files := range sources {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "tmpl-"+name)
			id := builtDeploy(t, f, files)
			if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			ins := instancesFor(t, id)
			if mounts, tmpfs := inspectMounts(t, ins[0].containerID); mounts != 0 || len(tmpfs) != 1 {
				t.Fatalf("%s: %d mounts, tmpfs %v; want no mounts and only /tmp", name, mounts, tmpfs)
			}
			if code, _ := getURL(t, fmt.Sprintf("http://127.0.0.1:%d/", ins[0].hostPort)); code != http.StatusOK {
				t.Fatalf("%s: GET / = %d", name, code)
			}
		})
	}
}

// buildCustom builds an arbitrary Dockerfile into the app's repository with
// the Builder, for tests that need images the templates cannot produce.
func buildCustom(t *testing.T, f fixture, tag, dockerfile string) string {
	t.Helper()
	var ctxBuf bytes.Buffer
	if err := build.WriteContext(&ctxBuf, bytes.NewReader(normalizedArchive(t, map[string]string{"x": "x"})), []byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	digest, err := f.pipeline.Builder.Build(context.Background(), &ctxBuf, f.appID.String(), tag)
	if err != nil {
		t.Fatalf("build custom image: %v", err)
	}
	return digest
}

func normalizedArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	dst := filepath.Join(t.TempDir(), "n.tar.gz")
	if err := build.NormalizeFile(context.Background(), &raw, dst, build.DefaultLimits); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	return b
}

// builtDeployWithImage creates a built deploy for a custom image; its health
// path is /etc/hostname (served by busybox httpd -h /).
func builtDeployWithImage(t *testing.T, f fixture, seq int, digest string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO deploys (app_id, seq, source_ref, state, image_digest, config_snapshot)
		 VALUES ($1, $2, $3, 'built', $4, '{"runtime": "static", "health": "/etc/hostname"}') RETURNING id`,
		f.appID, seq, "sha256:"+strings.Repeat("0", 64), digest).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func appState(t *testing.T, f fixture) (status, currentDeploy string) {
	t.Helper()
	var cur pgtype.UUID
	if err := testPool.QueryRow(context.Background(), `SELECT status, current_deploy_id FROM apps WHERE id = $1`, f.appID).Scan(&status, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Valid {
		currentDeploy = cur.String()
	}
	return status, currentDeploy
}

func containersOf(t *testing.T, deploy pgtype.UUID) []string {
	t.Helper()
	res, err := testDocker.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", runtime.LabelDeployID+"="+deploy.String()),
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

// An image VOLUME gets a tmpfs instead of a writable anonymous volume.
func TestStartCoversImageVolumes(t *testing.T) {
	f := newFixture(t, "image-volume")
	// busybox httpd serving / so the health path /etc/hostname returns 200.
	digest := buildCustom(t, f, "vol", "FROM busybox\nLABEL shipyard.app_id="+f.appID.String()+"\nVOLUME /data\nUSER 10001:10001\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/\"]\n")
	id := builtDeployWithImage(t, f, 1, digest)
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mounts, tmpfs := inspectMounts(t, instancesFor(t, id)[0].containerID)
	if mounts != 0 || !strings.Contains(tmpfs["/data"], "size=16m") {
		t.Fatalf("%d mounts, tmpfs %v; want /data on tmpfs and no volumes", mounts, tmpfs)
	}
}

// A container left from an older spec version is recreated, not reused.
func TestStartRecreatesDriftedContainer(t *testing.T) {
	f := newFixture(t, "drift")
	id := builtDeploy(t, f, map[string]string{"index.html": "x"})
	ins, err := store.New(testPool).CreateInstance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	old, err := testDocker.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Config: &container.Config{Image: "busybox", Cmd: []string{"sleep", "60"}, Labels: map[string]string{
			runtime.LabelInstanceID: ins.ID.String(), runtime.LabelAppID: f.appID.String(), runtime.LabelSpecVersion: "0",
		}},
		Name: runtime.ContainerName(ins.ID.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.pipeline.Start(context.Background(), startJob(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	now := instancesFor(t, id)
	if now[0].containerID == "" || now[0].containerID == old.ID {
		t.Fatalf("drifted container %s was reused", old.ID)
	}
	if _, err := testDocker.ContainerInspect(context.Background(), old.ID, client.ContainerInspectOptions{}); err == nil {
		t.Fatal("drifted container still exists")
	}
}

// failAfterStart really creates and starts the container, then reports a
// failure, as if inspecting or recording it had failed.
type failAfterStart struct{ *runtime.Engine }

func (e failAfterStart) Start(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	if _, err := e.Engine.Start(ctx, spec); err != nil {
		return runtime.Instance{}, err
	}
	return runtime.Instance{}, errors.New("injected failure after the container started")
}

// On the final attempt a start that fails after the container exists removes
// the container and fails the deploy with a generic message. (A container
// that merely exits is a health-check failure, not a start failure.)
func TestStartFinalFailureRemovesContainer(t *testing.T) {
	f := newFixture(t, "start-fails")
	f.pipeline.Engine = failAfterStart{f.pipeline.Engine.(*runtime.Engine)}
	id := builtDeploy(t, f, map[string]string{"index.html": "x"})
	job := startJob(id)
	job.Attempts, job.MaxAttempts = 2, 3 // last attempt
	if err := f.pipeline.Start(context.Background(), job); err == nil {
		t.Fatal("Start succeeded for a container that exits immediately")
	}
	ctrs, _ := testDocker.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", runtime.LabelDeployID+"="+id.String()),
	})
	if len(ctrs.Items) != 0 {
		t.Fatalf("%d containers left after the final failed start", len(ctrs.Items))
	}
	if d := getDeploy(t, id); d.state != "failed" || !strings.Contains(d.errMsg, "could not be started") {
		t.Fatalf("deploy = %+v", d)
	}
	if ins := instancesFor(t, id); len(ins) != 1 || ins[0].state != "failed" {
		t.Fatalf("instances = %+v", ins)
	}
}

func TestStartRejectsImageFromAnotherApp(t *testing.T) {
	f := newFixture(t, "wrong-image")
	other := f.pipeline.Builder.Repository("0b5f0b4e-6f1a-4c8e-9d7a-2f3e4d5c6b7a") + "@sha256:" + strings.Repeat("a", 64)
	id := builtDeployWithImage(t, f, 1, other)
	if err := f.pipeline.Start(context.Background(), startJob(id)); err == nil {
		t.Fatal("Start accepted another app's image")
	}
	if len(instancesFor(t, id)) != 0 {
		t.Fatal("instance created for a rejected image")
	}
}
