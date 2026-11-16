package runtime

import (
	"slices"
	"strings"
	"testing"
)

// Every property of ARCHITECTURE §10 that the spec controls. A failure here
// means user code would get more power than the security model allows.
func TestContainerSpecIsHardened(t *testing.T) {
	s := Spec{AppID: "app-1", DeployID: "dep-1", InstanceID: "ins-1", Image: "localhost:5000/shipyard/app-1@sha256:abc",
		Env: []string{"FOO=bar"}}
	cfg, host := s.Container()

	checks := []struct {
		name string
		ok   bool
	}{
		{"non-root user 10001", cfg.User == "10001:10001"},
		{"image pinned by digest", strings.Contains(cfg.Image, "@sha256:")},
		{"PORT=8080 set first", len(cfg.Env) > 0 && cfg.Env[0] == "PORT=8080"},
		{"extra env kept", slices.Contains(cfg.Env, "FOO=bar")},
		{"labels", cfg.Labels[LabelAppID] == "app-1" && cfg.Labels[LabelDeployID] == "dep-1" && cfg.Labels[LabelInstanceID] == "ins-1"},
		{"graceful stop", cfg.StopSignal == "SIGTERM" && cfg.StopTimeout != nil && *cfg.StopTimeout == 10},
		{"not privileged", !host.Privileged},
		{"all capabilities dropped", slices.Equal(host.CapDrop, []string{"ALL"}) && len(host.CapAdd) == 0},
		{"no-new-privileges", slices.Contains(host.SecurityOpt, "no-new-privileges:true")},
		{"no seccomp/apparmor opt-out", !slices.ContainsFunc(host.SecurityOpt, func(o string) bool { return strings.Contains(o, "unconfined") })},
		{"read-only rootfs", host.ReadonlyRootfs},
		{"tmpfs /tmp only, noexec/nosuid/nodev, 64m", len(host.Tmpfs) == 1 && host.Tmpfs["/tmp"] == "rw,noexec,nosuid,nodev,size=64m"},
		{"no bind mounts", len(host.Binds) == 0 && len(host.Mounts) == 0},
		{"no host or container network/pid/ipc/userns sharing", host.NetworkMode == "bridge" && host.PidMode == "" && host.IpcMode == "private" && host.UsernsMode == ""},
		{"no devices, sysctls or extra groups", len(host.Devices) == 0 && len(host.Sysctls) == 0 && len(host.GroupAdd) == 0},
		{"no publish-all", !host.PublishAllPorts},
		{"no restart policy (Shipyard restarts)", host.RestartPolicy.Name == "no"},
		{"not auto-removed (logs/exit code kept)", !host.AutoRemove},
		{"memory 256 MiB, no swap", host.Memory == 256<<20 && host.MemorySwap == 256<<20},
		{"0.5 CPU", host.NanoCPUs == 500_000_000},
		{"pids 128", host.PidsLimit != nil && *host.PidsLimit == 128},
		{"OOM killer enabled", host.OomKillDisable == nil || !*host.OomKillDisable},
		{"nofile 1024", len(host.Ulimits) == 1 && host.Ulimits[0].Name == "nofile" && host.Ulimits[0].Soft == 1024 && host.Ulimits[0].Hard == 1024},
		{"bounded json-file logs", host.LogConfig.Type == "json-file" && host.LogConfig.Config["max-size"] == "10m" && host.LogConfig.Config["max-file"] == "3"},
		{"init process", host.Init != nil && *host.Init},
		{"spec version label", cfg.Labels[LabelSpecVersion] == SpecVersion},
		{"small /dev/shm", host.ShmSize == 8<<20},
		{"OOM-killed before platform services", host.OomScoreAdj == 500},
		{"non-blocking logs", host.LogConfig.Config["mode"] == "non-blocking" && host.LogConfig.Config["max-buffer-size"] == "1m"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("spec violates: %s", c.name)
		}
	}

	// M1: exactly one port, 8080, bound to loopback with a random host port.
	if len(host.PortBindings) != 1 {
		t.Fatalf("port bindings = %v, want only 8080", host.PortBindings)
	}
	for port, bindings := range host.PortBindings {
		if port != appPort || len(bindings) != 1 || !bindings[0].HostIP.IsLoopback() || bindings[0].HostPort != "" {
			t.Errorf("port %v bindings %v: want 8080/tcp on 127.0.0.1 with a random host port", port, bindings)
		}
	}
}

func TestContainerSpecRuntime(t *testing.T) {
	_, host := Spec{OCIRuntime: "runsc"}.Container()
	if host.Runtime != "runsc" {
		t.Fatalf("runtime = %q, want runsc", host.Runtime)
	}
	_, host = Spec{}.Container()
	if host.Runtime != "" {
		t.Fatalf("runtime = %q, want default", host.Runtime)
	}
}

// Image VOLUMEs would otherwise become writable, unbounded anonymous volumes.
func TestContainerSpecCoversImageVolumesWithTmpfs(t *testing.T) {
	_, host := Spec{ImageVolumes: []string{"/data", "/config/", "/tmp", "/", "relative"}}.Container()
	want := map[string]string{
		"/tmp":    "rw,noexec,nosuid,nodev,size=64m",
		"/data":   "rw,noexec,nosuid,nodev,size=16m",
		"/config": "rw,noexec,nosuid,nodev,size=16m",
	}
	if len(host.Tmpfs) != len(want) {
		t.Fatalf("tmpfs = %v, want %v", host.Tmpfs, want)
	}
	for k, v := range want {
		if host.Tmpfs[k] != v {
			t.Errorf("tmpfs[%s] = %q, want %q", k, host.Tmpfs[k], v)
		}
	}
}
