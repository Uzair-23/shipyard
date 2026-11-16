//go:build integration

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/testdb"
)

var (
	testDocker   *client.Client
	testRegistry string
)

func TestMain(m *testing.M) {
	os.Exit(runIntegration(m))
}

func runIntegration(m *testing.M) int {
	var err error
	if testDocker, err = client.New(client.FromEnv); err != nil {
		fmt.Fprintln(os.Stderr, "docker client:", err)
		return 1
	}
	defer func() { _ = testDocker.Close() }()
	var terminate func()
	if testRegistry, terminate, err = testdb.StartRegistry(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer terminate()
	return m.Run()
}

// buildContext packs a normalised one-file source plus dockerfile.
func buildContext(t *testing.T, dockerfile string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteContext(&buf, bytes.NewReader(normalized(t, file("index.html", "<h1>hi</h1>"))), []byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func newBuilder(t *testing.T) *Builder {
	t.Helper()
	b := &Builder{Docker: testDocker, Registry: testRegistry}
	t.Cleanup(func() {
		imgs, _ := testDocker.ImageList(context.Background(), client.ImageListOptions{
			Filters: make(client.Filters).Add("reference", b.Repository(testAppID)+"*"),
		})
		for _, img := range imgs.Items {
			_, _ = testDocker.ImageRemove(context.Background(), img.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		}
	})
	return b
}

func assertBuildError(t *testing.T, err error, reason string) {
	t.Helper()
	var be *BuildError
	if !errors.As(err, &be) {
		t.Fatalf("got %v, want *BuildError containing %q", err, reason)
	}
	if !strings.Contains(be.Error(), reason) {
		t.Fatalf("got %q, want it to contain %q", be.Error(), reason)
	}
}

func TestBuildStaticTemplateAndPush(t *testing.T) {
	plan, err := Prepare(Source{Paths: []string{"index.html"}}, nil, testAppID)
	if err != nil {
		t.Fatal(err)
	}
	var ctxBuf bytes.Buffer
	if err := WriteContext(&ctxBuf, bytes.NewReader(normalized(t, file("index.html", "<h1>hi</h1>"), file(".env", "SECRET=1"))), plan.Dockerfile); err != nil {
		t.Fatal(err)
	}
	b := newBuilder(t)
	digest, err := b.Build(context.Background(), &ctxBuf, testAppID, "1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasPrefix(digest, b.Repository(testAppID)+"@sha256:") {
		t.Fatalf("digest ref = %q", digest)
	}
	img, err := testDocker.ImageInspect(context.Background(), digest)
	if err != nil {
		t.Fatalf("inspect by digest: %v", err)
	}
	if img.Config.User != "10001:10001" || img.Config.Labels["shipyard.app_id"] != testAppID {
		t.Fatalf("image user=%q labels=%v", img.Config.User, img.Config.Labels)
	}
}

func TestBuildFailingStepIsUserError(t *testing.T) {
	_, err := newBuilder(t).Build(context.Background(), buildContext(t, "FROM busybox\nRUN echo about to fail && exit 3\n"), testAppID, "fail")
	assertBuildError(t, err, "exit code 3")
	assertBuildError(t, err, "about to fail") // log tail included
}

// ADR 013: the classic builder must enforce the memory cap on untrusted
// install steps.
func TestBuildMemoryLimitIsEnforced(t *testing.T) {
	df := "FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b\n" +
		"RUN python -c \"x = bytearray(2 * 1024**3); print('allocated 2 GiB')\"\n"
	_, err := newBuilder(t).Build(context.Background(), buildContext(t, df), testAppID, "oom")
	assertBuildError(t, err, "ran out of memory")
}

func TestBuildOutputFloodIsAborted(t *testing.T) {
	b := newBuilder(t)
	b.MaxLogBytes = 1 << 20
	_, err := b.Build(context.Background(), buildContext(t, "FROM busybox\nRUN yes 'flood the build log' | head -c 5000000\n"), testAppID, "flood")
	assertBuildError(t, err, "build output exceeded 1 MB")
}

func TestBuildTimeout(t *testing.T) {
	b := newBuilder(t)
	b.Timeout = 5 * time.Second
	start := time.Now()
	_, err := b.Build(context.Background(), buildContext(t, "FROM busybox\nRUN sleep 120\n"), testAppID, "slow")
	assertBuildError(t, err, "longer than 5s")
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("timed-out build took %s to return", d)
	}
}

func TestBuildImageSizeLimit(t *testing.T) {
	b := newBuilder(t)
	b.MaxImageBytes = 1 << 10
	_, err := b.Build(context.Background(), buildContext(t, "FROM busybox\nRUN true\n"), testAppID, "big")
	assertBuildError(t, err, "larger than the")
}

// Build output is attacker-controlled: NUL, invalid UTF-8 and terminal escape
// sequences must not reach the stored error.
func TestBuildErrorOutputIsSanitised(t *testing.T) {
	// Raw string: the backslash escapes are for printf inside the build.
	df := "FROM busybox\n" + `RUN printf 'start x\000y \377 \033]52;c;ZXZpbA==\007 \033[31mred\033[0m end\n' && exit 1` + "\n"
	_, err := newBuilder(t).Build(context.Background(), buildContext(t, df), testAppID, "dirty")
	var be *BuildError
	if !errors.As(err, &be) {
		t.Fatalf("got %v, want *BuildError", err)
	}
	for _, bad := range []string{"\x00", "\x1b", "\x07", "\xff"} {
		if strings.Contains(be.Error(), bad) {
			t.Fatalf("error contains %q: %q", bad, be.Error())
		}
	}
	if !strings.Contains(be.LogTail, "start xy") || !strings.Contains(be.LogTail, "end") {
		t.Fatalf("useful output lost: %q", be.LogTail)
	}
}

// Failed builds must not leave this app's layers behind (disk exhaustion).
func TestFailedBuildLeavesNoLayers(t *testing.T) {
	df := "FROM busybox\nLABEL shipyard.app_id=" + testAppID + "\n" +
		"RUN dd if=/dev/zero of=/big bs=1M count=50\n" +
		"RUN echo second layer > /x\n" +
		"RUN exit 1\n"
	_, err := newBuilder(t).Build(context.Background(), buildContext(t, df), testAppID, "layers")
	assertBuildError(t, err, "exit code 1")
	imgs, err := testDocker.ImageList(context.Background(), client.ImageListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", "shipyard.app_id="+testAppID).Add("dangling", "true"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs.Items) != 0 {
		t.Fatalf("%d dangling layers left for the app", len(imgs.Items))
	}
}

// Daemon-side failures (here: a missing base image) are platform errors, not
// the user's fault, and their raw text is not a user-facing BuildError.
func TestDaemonErrorIsNotUserError(t *testing.T) {
	_, err := newBuilder(t).Build(context.Background(), buildContext(t, "FROM shipyard-test/does-not-exist:never\n"), testAppID, "nobase")
	var be *BuildError
	if err == nil || errors.As(err, &be) {
		t.Fatalf("got %v, want a platform (non-BuildError) error", err)
	}
}
