package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

// BuildError is a build failure caused by the user's source (a failing RUN,
// a limit exceeded). Reason and LogTail are sanitised (valid UTF-8, no
// control characters except newline and tab) and safe to store and show.
type BuildError struct {
	Reason  string
	LogTail string
}

func (e *BuildError) Error() string {
	if e.LogTail == "" {
		return "build failed: " + e.Reason
	}
	return "build failed: " + e.Reason + "\n--- last build output ---\n" + e.LogTail
}

// Builder builds images with the classic Docker builder (ADR 013), so the
// memory and CPU limits apply to the untrusted install steps, and pushes
// them to the local registry. A Builder runs one build at a time.
type Builder struct {
	Docker   *client.Client
	Registry string // host:port, e.g. localhost:5000

	Timeout       time.Duration // whole build; default 5m (ARCHITECTURE §9)
	Memory        int64         // bytes, default 1 GiB
	CPUs          float64       // default 1
	MaxLogBytes   int           // build output allowed before the build is aborted; default 8 MiB
	MaxImageBytes int64         // default 1 GiB (PRD §7.3)

	mu sync.Mutex // serialises builds and the per-app cleanup after them
}

func (b *Builder) defaults() {
	if b.Timeout == 0 {
		b.Timeout = 5 * time.Minute
	}
	if b.Memory == 0 {
		b.Memory = 1 << 30
	}
	if b.CPUs == 0 {
		b.CPUs = 1
	}
	if b.MaxLogBytes == 0 {
		b.MaxLogBytes = 8 << 20
	}
	if b.MaxImageBytes == 0 {
		b.MaxImageBytes = 1 << 30
	}
}

// Repository returns the registry repository for an app's images.
func (b *Builder) Repository(appID string) string {
	return b.Registry + "/shipyard/" + appID
}

// runFailureRE matches the classic builder's message for a RUN step that
// exited non-zero: the only daemon error that is the user's fault.
var runFailureRE = regexp.MustCompile(`returned a non-zero code: (\d+)$`)

// Build builds buildContext (see WriteContext) as Repository(appID):tag,
// checks the image size, pushes it and returns its digest reference
// (repository@sha256:...). A *BuildError means the source is at fault; any
// other error is a platform problem whose details must not reach the user.
func (b *Builder) Build(ctx context.Context, buildContext io.Reader, appID, tag string) (digestRef string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.defaults()
	// Whatever happens, remove this app's dangling build layers (failed
	// steps, oversized images) so repeated failures cannot fill the disk.
	defer b.cleanup(ctx, appID)

	ref := b.Repository(appID) + ":" + tag
	log := slog.With("app_id", appID, "image", ref)

	bctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	res, err := b.Docker.ImageBuild(bctx, buildContext, client.ImageBuildOptions{
		Version:     build.BuilderV1,
		Tags:        []string{ref},
		Dockerfile:  "Dockerfile",
		Remove:      true,
		ForceRemove: true,
		Memory:      b.Memory,
		MemorySwap:  b.Memory, // no swap beyond the memory limit
		CPUPeriod:   100_000,
		CPUQuota:    int64(b.CPUs * 100_000),
		Labels:      map[string]string{"shipyard.app_id": appID},
	})
	if err != nil {
		return "", b.classify(ctx, bctx, nil, fmt.Errorf("start image build: %w", err))
	}
	defer func() { _ = res.Body.Close() }()

	out := &tailBuffer{max: 16 << 10}
	total := 0
	dec := json.NewDecoder(res.Body)
	for {
		var m jsonstream.Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", b.classify(ctx, bctx, out, fmt.Errorf("read build output: %w", err))
		}
		if m.Error != nil {
			msg := strings.TrimSpace(m.Error.Message)
			code := runFailureRE.FindStringSubmatch(msg)
			if code == nil {
				// Daemon-side failure (disk full, base image pull, storage
				// driver...): may contain host paths; never shown to users.
				return "", fmt.Errorf("docker build %s: %s", ref, msg)
			}
			reason := "a build step failed with exit code " + code[1]
			if code[1] == "137" {
				reason += fmt.Sprintf(" (killed: the build probably ran out of memory; the limit is %d MB)", b.Memory>>20)
			}
			return "", &BuildError{Reason: reason, LogTail: out.String()}
		}
		total += len(m.Stream)
		if total > b.MaxLogBytes {
			cancel() // stop the build on the daemon
			return "", &BuildError{Reason: fmt.Sprintf("build output exceeded %d MB", b.MaxLogBytes>>20), LogTail: out.String()}
		}
		if m.Stream != "" {
			out.WriteString(m.Stream)
		}
	}
	if bctx.Err() != nil {
		return "", b.classify(ctx, bctx, out, bctx.Err())
	}

	img, err := b.Docker.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect built image: %w", err)
	}
	if img.Size > b.MaxImageBytes {
		_, _ = b.Docker.ImageRemove(context.WithoutCancel(ctx), ref, client.ImageRemoveOptions{Force: true})
		return "", &BuildError{Reason: fmt.Sprintf("image is %d MB, larger than the %d MB limit", img.Size>>20, b.MaxImageBytes>>20)}
	}

	if err := b.push(ctx, ref); err != nil {
		return "", err
	}
	img, err = b.Docker.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect pushed image: %w", err)
	}
	prefix := b.Repository(appID) + "@"
	for _, d := range img.RepoDigests {
		if strings.HasPrefix(d, prefix) {
			log.Info("image built and pushed", "digest", d, "size_mb", img.Size>>20)
			return d, nil
		}
	}
	return "", fmt.Errorf("pushed image %s has no digest for %s", ref, b.Repository(appID))
}

// cleanup removes dangling images carrying this app's label: layers of
// failed steps and removed oversized images. Templates label every layer
// (LABEL shipyard.app_id), so other apps' and non-Shipyard images are never
// touched.
func (b *Builder) cleanup(ctx context.Context, appID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	res, err := b.Docker.ImagePrune(cctx, client.ImagePruneOptions{
		Filters: make(client.Filters).Add("dangling", "true").Add("label", "shipyard.app_id="+appID),
	})
	if err != nil {
		slog.Warn("prune build layers", "app_id", appID, "error", err)
		return
	}
	if n := len(res.Report.ImagesDeleted); n > 0 {
		slog.Info("pruned build layers", "app_id", appID, "images", n, "reclaimed_mb", res.Report.SpaceReclaimed>>20)
	}
}

// classify turns a failed build into a user-facing timeout error when the
// build deadline (not the caller) ended it.
func (b *Builder) classify(parent, bctx context.Context, out *tailBuffer, err error) error {
	if parent.Err() == nil && errors.Is(bctx.Err(), context.DeadlineExceeded) {
		tail := ""
		if out != nil {
			tail = out.String()
		}
		return &BuildError{Reason: fmt.Sprintf("build took longer than %s", b.Timeout), LogTail: tail}
	}
	return err
}

func (b *Builder) push(ctx context.Context, ref string) error {
	// "e30=" is base64("{}"): the local registry needs no credentials.
	resp, err := b.Docker.ImagePush(ctx, ref, client.ImagePushOptions{RegistryAuth: "e30="})
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	for m, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return fmt.Errorf("push %s: %w", ref, err)
		}
		if m.Error != nil {
			return fmt.Errorf("push %s: %s", ref, m.Error.Message)
		}
	}
	return nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) WriteString(s string) {
	t.buf.WriteString(s)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
}

// String returns the tail sanitised for storage and display: valid UTF-8
// (a cut multi-byte character is dropped), no NUL, and no control characters
// other than newline and tab, so ANSI/OSC escape sequences cannot reach a
// terminal or the database.
func (t *tailBuffer) String() string { return Sanitize(t.buf.String()) }

// Sanitize makes attacker-controlled text safe to store in Postgres and show
// in a terminal or browser: valid UTF-8 without control characters (except
// newline and tab) or invisible format characters (bidi overrides,
// zero-width characters) that could make text render differently.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
