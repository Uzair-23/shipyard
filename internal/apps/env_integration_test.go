//go:build integration

package apps

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/Uzair-23/shipyard/internal/secrets"
)

func putEnv(t *testing.T, f fixture, key []byte, name, value string, secret bool) {
	t.Helper()
	ct, nonce, err := secrets.Encrypt(key, f.appID.String(), name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO env_vars (app_id, key, value_encrypted, nonce, is_secret) VALUES ($1, $2, $3, $4, $5)`,
		f.appID, name, ct, nonce, secret); err != nil {
		t.Fatal(err)
	}
}

func containerEnv(t *testing.T, id string) []string {
	t.Helper()
	res, err := testDocker.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Container.Config.Env
}

func TestStartInjectsDecryptedEnvironment(t *testing.T) {
	f := newFixture(t, "env-inject")
	key := bytes.Repeat([]byte{5}, secrets.KeyLen)
	f.pipeline.MasterKey = key
	putEnv(t, f, key, "DEMO_PLAIN", "hello world", false)
	putEnv(t, f, key, "DEMO_SECRET", "s3cr3t=with=equals", true)
	// A reserved name that somehow reached the database must not override PORT.
	putEnv(t, f, key, "PORT", "1", false)

	d := builtDeploy(t, f, map[string]string{"index.html": "env test"})
	if err := f.pipeline.Start(context.Background(), startJob(d)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctrs := containersOf(t, d)
	if len(ctrs) != 1 {
		t.Fatalf("containers = %v", ctrs)
	}
	env := containerEnv(t, ctrs[0])
	for _, want := range []string{"DEMO_PLAIN=hello world", "DEMO_SECRET=s3cr3t=with=equals", "PORT=8080"} {
		if !slices.Contains(env, want) {
			t.Errorf("container env is missing %q: %v", want, env)
		}
	}
	ports := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PORT=") {
			ports++
		}
	}
	if ports != 1 {
		t.Errorf("PORT appears %d times in %v", ports, env)
	}
}

// An app with variables must not start without its secrets because the key is missing.
func TestStartWithoutMasterKeyDoesNotStartTheApp(t *testing.T) {
	f := newFixture(t, "env-nokey")
	putEnv(t, f, bytes.Repeat([]byte{5}, secrets.KeyLen), "DEMO_SECRET", "x", true)
	d := builtDeploy(t, f, map[string]string{"index.html": "env test"})
	err := f.pipeline.Start(context.Background(), startJob(d))
	if err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("Start error = %v, want one about the master key", err)
	}
	if c := containersOf(t, d); len(c) != 0 {
		t.Fatalf("a container was started without the app's secrets: %v", c)
	}
}

// The wrong key (say, a rotated or regenerated one) fails to decrypt instead of starting with garbage.
func TestStartWithWrongMasterKeyFails(t *testing.T) {
	f := newFixture(t, "env-wrongkey")
	putEnv(t, f, bytes.Repeat([]byte{5}, secrets.KeyLen), "DEMO_SECRET", "x", true)
	f.pipeline.MasterKey = bytes.Repeat([]byte{6}, secrets.KeyLen)
	d := builtDeploy(t, f, map[string]string{"index.html": "env test"})
	if err := f.pipeline.Start(context.Background(), startJob(d)); err == nil {
		t.Fatal("Start succeeded with the wrong master key")
	}
	if c := containersOf(t, d); len(c) != 0 {
		t.Fatalf("a container was started: %v", c)
	}
}
