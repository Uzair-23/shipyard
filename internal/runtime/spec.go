// Package runtime wraps the Docker engine for app containers: the hardened
// container spec (ARCHITECTURE §10) and starting containers.
package runtime

import (
	"net/netip"
	"path"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// AppPort is the port every app listens on inside its container ($PORT).
const AppPort = 8080

// Labels on every app container; the drift reconciler (M7) relies on them.
const (
	LabelAppID       = "shipyard.app_id"
	LabelDeployID    = "shipyard.deploy_id"
	LabelInstanceID  = "shipyard.instance_id"
	LabelSpecVersion = "shipyard.spec_version"
)

// SpecVersion changes whenever Container() changes, so containers created
// with an older spec are recreated instead of reused.
const SpecVersion = "1"

// Resource limits per app (PRD §7.3).
const (
	MemoryBytes = 256 << 20
	NanoCPUs    = 500_000_000 // 0.5 CPU
	PidsLimit   = 128
	NoFile      = 1024
	TmpfsSize   = "64m"
	VolumeTmpfs = "16m" // tmpfs mounted over each image VOLUME
	ShmBytes    = 8 << 20
	OOMScoreAdj = 500 // the kernel kills apps before platform services
	StopTimeout = 10  // seconds between SIGTERM and SIGKILL
)

var appPort = network.MustParsePort(strconv.Itoa(AppPort) + "/tcp")

// Spec describes one app container.
type Spec struct {
	AppID, DeployID, InstanceID string
	Image                       string   // image reference, normally repository@sha256:...
	OCIRuntime                  string   // "runsc" when gVisor is available, "" for the default runc
	Env                         []string // extra KEY=value pairs; PORT is always set
	// ImageVolumes are the VOLUME paths declared by the image. Each gets a
	// small tmpfs so Docker never creates a writable, unbounded anonymous
	// volume for it.
	ImageVolumes []string
}

// ContainerName is the Docker name of an instance's container.
func ContainerName(instanceID string) string { return "shipyard-" + instanceID }

// Container returns the hardened Docker config for s. User code is untrusted:
// no extra privileges, no host mounts or Docker socket, no host network,
// read-only root filesystem, and kernel-enforced resource limits.
//
// M1 only (ADR 009, ADR 011): port 8080 is published on 127.0.0.1 with a
// random host port so the host-run worker can reach it. M2 replaces this
// with per-app networks behind the router.
func (s Spec) Container() (*container.Config, *container.HostConfig) {
	stopTimeout := StopTimeout
	pids := int64(PidsLimit)
	initProc := true
	env := append([]string{"PORT=" + strconv.Itoa(AppPort)}, s.Env...)

	tmpfs := map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=" + TmpfsSize}
	for _, v := range s.ImageVolumes {
		if v = path.Clean(v); path.IsAbs(v) && v != "/" && v != "/tmp" {
			tmpfs[v] = "rw,noexec,nosuid,nodev,size=" + VolumeTmpfs
		}
	}

	cfg := &container.Config{
		Image:        s.Image,
		User:         "10001:10001",
		Hostname:     "app",
		Env:          env,
		ExposedPorts: network.PortSet{appPort: struct{}{}},
		Labels: map[string]string{
			LabelAppID:       s.AppID,
			LabelDeployID:    s.DeployID,
			LabelInstanceID:  s.InstanceID,
			LabelSpecVersion: SpecVersion,
		},
		StopSignal:  "SIGTERM",
		StopTimeout: &stopTimeout,
	}
	host := &container.HostConfig{
		Runtime:        s.OCIRuntime,
		NetworkMode:    "bridge",
		PortBindings:   network.PortMap{appPort: {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: ""}}},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ReadonlyRootfs: true,
		Tmpfs:          tmpfs,
		IpcMode:        "private",
		ShmSize:        ShmBytes,
		OomScoreAdj:    OOMScoreAdj,
		Init:           &initProc,
		LogConfig: container.LogConfig{
			Type: "json-file",
			// Bounded on disk, and non-blocking so a chatty app cannot stall
			// dockerd (logs are dropped when the buffer is full).
			Config: map[string]string{"max-size": "10m", "max-file": "3", "mode": "non-blocking", "max-buffer-size": "1m"},
		},
		Resources: container.Resources{
			Memory:     MemoryBytes,
			MemorySwap: MemoryBytes, // no swap beyond the memory limit
			NanoCPUs:   NanoCPUs,
			PidsLimit:  &pids,
			Ulimits:    []*container.Ulimit{{Name: "nofile", Soft: NoFile, Hard: NoFile}},
		},
	}
	return cfg, host
}
