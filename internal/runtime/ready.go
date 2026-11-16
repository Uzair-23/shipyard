package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// ExitedError means the app's container stopped while Shipyard waited for it
// to become healthy. Its message is safe to show to the user.
type ExitedError struct {
	ExitCode  int
	OOMKilled bool
}

func (e *ExitedError) Error() string {
	if e.OOMKilled {
		return fmt.Sprintf("the app was killed for using more than %d MB of memory during startup", MemoryBytes>>20)
	}
	return fmt.Sprintf("the app exited with code %d during startup", e.ExitCode)
}

// WaitReady waits until the app answers its health path on the instance's
// loopback port (see WaitHealthy) while watching the container: if it stops,
// WaitReady returns an *ExitedError at once instead of waiting out the
// timeout. Health failures are *UnhealthyError; both mean the app is at fault.
func (e *Engine) WaitReady(parent context.Context, ins Instance, healthPath string, timeout time.Duration) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			res, err := e.Docker.ContainerInspect(ctx, ins.ContainerID, client.ContainerInspectOptions{})
			if err != nil {
				if errdefs.IsNotFound(err) {
					cancel(&ExitedError{ExitCode: -1})
					return
				}
				continue // transient; the health probe still decides
			}
			if st := res.Container.State; st != nil && !st.Running {
				cancel(&ExitedError{ExitCode: st.ExitCode, OOMKilled: st.OOMKilled})
				return
			}
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", ins.HostPort, healthPath)
	err := WaitHealthy(ctx, url, healthPath, HealthInterval, timeout)
	var exited *ExitedError
	if errors.As(context.Cause(ctx), &exited) && parent.Err() == nil {
		return exited
	}
	if err != nil {
		return err
	}
	// The probe hit the port read at start. Confirm the same container still
	// runs and still owns that port, so a freed port reused by another
	// container cannot make a dead deploy look healthy.
	return e.confirmRunning(parent, ins)
}

func (e *Engine) confirmRunning(ctx context.Context, ins Instance) error {
	res, err := e.Docker.ContainerInspect(ctx, ins.ContainerID, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return &ExitedError{ExitCode: -1}
	}
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", ins.ContainerID, err)
	}
	c := res.Container
	if c.State == nil || !c.State.Running {
		code, oom := -1, false
		if c.State != nil {
			code, oom = c.State.ExitCode, c.State.OOMKilled
		}
		return &ExitedError{ExitCode: code, OOMKilled: oom}
	}
	if c.NetworkSettings != nil {
		for _, b := range c.NetworkSettings.Ports[appPort] {
			if b.HostIP.IsLoopback() && b.HostPort == fmt.Sprint(ins.HostPort) {
				return nil
			}
		}
	}
	return fmt.Errorf("container %s no longer maps port %d", ins.ContainerID, ins.HostPort)
}

// LogTail returns up to maxBytes of the container's last log lines (stdout
// and stderr). The text is untrusted app output; sanitise before storing.
func (e *Engine) LogTail(ctx context.Context, containerID string, lines, maxBytes int) (string, error) {
	rc, err := e.Docker.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: fmt.Sprint(lines),
	})
	if err != nil {
		return "", fmt.Errorf("container logs %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	w := &limitedBuffer{buf: &buf, max: maxBytes}
	if _, err := stdcopy.StdCopy(w, w, rc); err != nil && !errors.Is(err, errLimit) {
		return buf.String(), fmt.Errorf("read container logs: %w", err)
	}
	return buf.String(), nil
}

// Stop stops an instance's container gracefully (SIGTERM, then SIGKILL after
// StopTimeout) and removes it.
func (e *Engine) Stop(ctx context.Context, instanceID string) error {
	res, err := e.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelInstanceID+"="+instanceID),
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	timeout := StopTimeout
	for _, c := range res.Items {
		if _, err := e.Docker.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("stop container %s: %w", c.ID, err)
		}
		if err := e.removeContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

var errLimit = errors.New("log tail limit reached")

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	room := l.max - l.buf.Len()
	if room <= 0 {
		return 0, errLimit
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		return room, errLimit
	}
	return l.buf.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)
