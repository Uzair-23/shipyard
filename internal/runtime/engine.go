package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Engine starts app containers with the hardened Spec.
type Engine struct {
	Docker     *client.Client
	OCIRuntime string // "runsc" or "" (runc)
}

// NewEngine picks gVisor (runsc) when the Docker daemon has it registered,
// otherwise the default runc with a warning (ADR 007).
func NewEngine(ctx context.Context, docker *client.Client) (*Engine, error) {
	info, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	e := &Engine{Docker: docker}
	if _, ok := info.Info.Runtimes["runsc"]; ok {
		e.OCIRuntime = "runsc"
		slog.Info("app containers use gVisor", "runtime", "runsc")
	} else {
		slog.Warn("gVisor (runsc) not available; app containers use runc with the hardened profile only (ADR 007)")
	}
	return e, nil
}

// RuntimeName is the OCI runtime app containers use ("runsc" or "runc").
func (e *Engine) RuntimeName() string {
	if e.OCIRuntime == "" {
		return "runc"
	}
	return e.OCIRuntime
}

// Instance is a started app container.
type Instance struct {
	ContainerID string
	HostPort    uint16 // M1 only: 127.0.0.1 port mapped to the app's 8080
}

// Start creates and starts the container for spec. It is idempotent: an
// existing container for spec.InstanceID is reused only if it was created
// for the same app, deploy, image and spec version; otherwise it is removed
// and recreated.
func (e *Engine) Start(ctx context.Context, spec Spec) (Instance, error) {
	spec.OCIRuntime = e.OCIRuntime
	id, err := e.existing(ctx, spec)
	if err != nil {
		return Instance{}, err
	}
	if id == "" {
		volumes, err := e.ensureImage(ctx, spec.Image)
		if err != nil {
			return Instance{}, err
		}
		spec.ImageVolumes = volumes
		cfg, host := spec.Container()
		res, err := e.Docker.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config: cfg, HostConfig: host, Name: ContainerName(spec.InstanceID),
		})
		if err != nil {
			return Instance{}, fmt.Errorf("create container for instance %s: %w", spec.InstanceID, err)
		}
		id = res.ID
		for _, w := range res.Warnings {
			slog.Warn("container create warning", "container_id", id, "warning", w)
		}
	}
	if _, err := e.Docker.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return Instance{}, fmt.Errorf("start container %s: %w", id, err)
	}
	ins, err := e.Docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Instance{}, fmt.Errorf("inspect container %s: %w", id, err)
	}
	var port uint16
	if ns := ins.Container.NetworkSettings; ns != nil {
		for _, b := range ns.Ports[appPort] {
			if p, err := strconv.ParseUint(b.HostPort, 10, 16); err == nil && b.HostIP.IsLoopback() {
				port = uint16(p)
			}
		}
	}
	if port == 0 {
		if st := ins.Container.State; st != nil && !st.Running {
			// The app exited before its port could be read (it crashed at once). That is the
			// app's fault, like failing the health check; the container is kept for its logs.
			return Instance{ContainerID: id}, &ExitedError{ExitCode: st.ExitCode, OOMKilled: st.OOMKilled}
		}
		return Instance{}, fmt.Errorf("container %s has no loopback port for %s", id, appPort)
	}
	return Instance{ContainerID: id, HostPort: port}, nil
}

// ensureImage pulls ref (repository@sha256:...) from the registry if it is
// not in the local image store, e.g. after old images were cleaned up, and
// returns the VOLUME paths the image declares.
func (e *Engine) ensureImage(ctx context.Context, ref string) ([]string, error) {
	img, err := e.Docker.ImageInspect(ctx, ref)
	if errdefs.IsNotFound(err) {
		slog.Info("image not present locally; pulling", "image", ref)
		resp, perr := e.Docker.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: "e30="})
		if perr != nil {
			return nil, fmt.Errorf("pull %s: %w", ref, perr)
		}
		if perr := resp.Wait(ctx); perr != nil {
			return nil, fmt.Errorf("pull %s: %w", ref, perr)
		}
		img, err = e.Docker.ImageInspect(ctx, ref)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect image %s: %w", ref, err)
	}
	var volumes []string
	if img.Config != nil {
		for v := range img.Config.Volumes {
			volumes = append(volumes, v)
		}
	}
	return volumes, nil
}

// existing returns the container already created for spec.InstanceID, if it
// matches spec exactly. A container with the instance label but a different
// app, deploy, image or spec version is removed so it is recreated with the
// current hardened spec.
func (e *Engine) existing(ctx context.Context, spec Spec) (string, error) {
	res, err := e.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelInstanceID+"="+spec.InstanceID),
	})
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}
	switch len(res.Items) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", fmt.Errorf("%d containers carry instance label %s", len(res.Items), spec.InstanceID)
	}
	c := res.Items[0]
	if c.Labels[LabelAppID] == spec.AppID && c.Labels[LabelDeployID] == spec.DeployID &&
		c.Labels[LabelSpecVersion] == SpecVersion && c.Image == spec.Image {
		return c.ID, nil
	}
	slog.Warn("existing container does not match the current spec; recreating", "container_id", c.ID, "instance_id", spec.InstanceID)
	if err := e.removeContainer(ctx, c.ID); err != nil {
		return "", err
	}
	return "", nil
}

// Remove force-removes the container (and its anonymous volumes) of an
// instance, if any. Used when a start fails for good.
func (e *Engine) Remove(ctx context.Context, instanceID string) error {
	res, err := e.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelInstanceID+"="+instanceID),
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, c := range res.Items {
		if err := e.removeContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) removeContainer(ctx context.Context, id string) error {
	if _, err := e.Docker.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}
