//go:build integration

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const secretValue = "topsecret-hunter2-value"

func envPath(slug, key string) string { return "/api/v1/apps/" + slug + "/env/" + key }

func TestEnvVarsAndSecretsAreNeverEchoedAndStoredEncrypted(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "env-app")

	rec := e.call(t, e.cookie, http.MethodPut, envPath("env-app", "GREETING"), map[string]any{"value": "hello world", "is_secret": false})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hello world") {
		t.Fatalf("put plain: %d %s", rec.Code, rec.Body)
	}
	rec = e.call(t, e.cookie, http.MethodPut, envPath("env-app", "API_KEY"), map[string]any{"value": secretValue, "is_secret": true})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secretValue) {
		t.Fatalf("put secret: %d %s", rec.Code, rec.Body)
	}

	rec = e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/env-app/env", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, secretValue) {
		t.Fatalf("the secret value is in the API response: %s", body)
	}
	if !strings.Contains(body, `"key":"GREETING"`) || !strings.Contains(body, `"value":"hello world"`) {
		t.Errorf("a non-secret value should be shown: %s", body)
	}
	if !strings.Contains(body, `"key":"API_KEY","is_secret":true`) || strings.Count(body, `"value"`) != 1 {
		t.Errorf("a secret must be listed by name only: %s", body)
	}

	// At rest: ciphertext, never the plaintext, with a 96-bit nonce.
	var leaked, plainLeaked bool
	var nonceLen int
	err := testPool.QueryRow(context.Background(), `
		SELECT bool_or(position(convert_to($1, 'UTF8') in value_encrypted) > 0),
		       bool_or(position(convert_to('hello world', 'UTF8') in value_encrypted) > 0),
		       min(length(nonce))
		FROM env_vars WHERE app_id = (SELECT id FROM apps WHERE slug = 'env-app')`, secretValue).Scan(&leaked, &plainLeaked, &nonceLen)
	if err != nil || leaked || plainLeaked || nonceLen != 12 {
		t.Fatalf("stored values: secret leaked=%v plain leaked=%v nonce=%d (%v)", leaked, plainLeaked, nonceLen, err)
	}

	// Updating replaces the value; the same value encrypts differently each time.
	var before []byte
	_ = testPool.QueryRow(context.Background(), `SELECT value_encrypted FROM env_vars WHERE key = 'API_KEY' AND app_id = (SELECT id FROM apps WHERE slug = 'env-app')`).Scan(&before)
	e.call(t, e.cookie, http.MethodPut, envPath("env-app", "API_KEY"), map[string]any{"value": secretValue, "is_secret": true})
	var after []byte
	_ = testPool.QueryRow(context.Background(), `SELECT value_encrypted FROM env_vars WHERE key = 'API_KEY' AND app_id = (SELECT id FROM apps WHERE slug = 'env-app')`).Scan(&after)
	if string(before) == string(after) {
		t.Error("the same value produced the same ciphertext twice")
	}

	// Delete.
	if rec := e.call(t, e.cookie, http.MethodDelete, envPath("env-app", "GREETING"), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.call(t, e.cookie, http.MethodDelete, envPath("env-app", "GREETING"), nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting a missing variable: %d, want 404", rec.Code)
	}
}

func TestEnvVarValidation(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "env-valid")
	for _, name := range []string{"PORT", "HOME", "PATH", "SHIPYARD_ANYTHING", "1BAD", "a-b", "a.b", strings.Repeat("A", 65)} {
		rec := e.call(t, e.cookie, http.MethodPut, envPath("env-valid", name), map[string]any{"value": "x"})
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_name" {
			t.Errorf("name %q: %d %s, want 400 invalid_name", name, rec.Code, rec.Body)
		}
	}
	for _, name := range []string{"lower_case", "_UNDERSCORE", "A1", strings.Repeat("A", 64)} {
		if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-valid", name), map[string]any{"value": "x"}); rec.Code != http.StatusOK {
			t.Errorf("name %q: %d, want 200", name, rec.Code)
		}
	}
	for label, body := range map[string]any{
		"missing value": map[string]any{"is_secret": true},
		"too long":      map[string]any{"value": strings.Repeat("x", 4097)},
		"NUL byte":      map[string]any{"value": "a\x00b"},
		"unknown field": map[string]any{"value": "x", "oops": 1},
	} {
		if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-valid", "OK_NAME"), body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", label, rec.Code)
		}
	}
	if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-valid", "MAX_VALUE"), map[string]any{"value": strings.Repeat("x", 4096)}); rec.Code != http.StatusOK {
		t.Errorf("4096-byte value: %d, want 200", rec.Code)
	}
}

func TestEnvVarsAreOwnerOnly(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "env-owner")
	e.call(t, e.cookie, http.MethodPut, envPath("env-owner", "K"), map[string]any{"value": "v"})
	other := signup(t, e.handler, "env-other@example.com")
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/env-owner/env"},
		{http.MethodPut, envPath("env-owner", "K")},
		{http.MethodDelete, envPath("env-owner", "K")},
	} {
		if rec := e.call(t, other, r.method, r.path, map[string]any{"value": "hijack"}); rec.Code != http.StatusNotFound {
			t.Errorf("other user %s %s: %d, want 404", r.method, r.path, rec.Code)
		}
		if rec := e.call(t, nil, r.method, r.path, map[string]any{"value": "x"}); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d, want 401", r.method, r.path, rec.Code)
		}
	}
	if rec := e.call(t, e.cookie, http.MethodGet, "/api/v1/apps/env-owner/env", nil); !strings.Contains(rec.Body.String(), `"value":"v"`) {
		t.Errorf("the other user's attempts changed the value: %s", rec.Body)
	}
}

func TestEnvVarLimitAndMissingMasterKey(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "env-limit")
	for i := 0; i < maxEnvVars; i++ {
		if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-limit", fmt.Sprintf("VAR_%d", i)), map[string]any{"value": "v"}); rec.Code != http.StatusOK {
			t.Fatalf("variable %d: %d", i, rec.Code)
		}
	}
	if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-limit", "ONE_TOO_MANY"), map[string]any{"value": "v"}); rec.Code != http.StatusConflict {
		t.Errorf("101st variable: %d, want 409", rec.Code)
	}
	if rec := e.call(t, e.cookie, http.MethodPut, envPath("env-limit", "VAR_0"), map[string]any{"value": "changed"}); rec.Code != http.StatusOK {
		t.Errorf("updating an existing variable at the limit: %d, want 200", rec.Code)
	}

	e.api.MasterKey = nil
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/env-limit/env"}, {http.MethodPut, envPath("env-limit", "K")}, {http.MethodDelete, envPath("env-limit", "VAR_0")},
	} {
		if rec := e.call(t, e.cookie, r.method, r.path, map[string]any{"value": "x"}); rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "secrets_unavailable" {
			t.Errorf("%s %s without a master key: %d, want 503", r.method, r.path, rec.Code)
		}
	}
}

func (e env) restartJobs(t *testing.T, slug string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'restart' AND app_id = (SELECT id FROM apps WHERE slug = $1)`, slug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Changing the environment restarts an app that is running, and only then.
func TestEnvChangeRestartsOnlyARunningApp(t *testing.T) {
	e := newEnv(t)
	e.createApp(t, "env-restart")

	// Never deployed: nothing to restart.
	e.call(t, e.cookie, http.MethodPut, envPath("env-restart", "A"), map[string]any{"value": "1"})
	if n := e.restartJobs(t, "env-restart"); n != 0 {
		t.Fatalf("restart jobs for an undeployed app = %d, want 0", n)
	}

	// Deployed and running: each change enqueues one restart.
	if _, err := testPool.Exec(context.Background(), `
		WITH d AS (INSERT INTO deploys (app_id, seq, source_ref, state) SELECT id, 1, 'sha256:x', 'live' FROM apps WHERE slug = 'env-restart' RETURNING id, app_id)
		UPDATE apps SET current_deploy_id = d.id, status = 'running' FROM d WHERE apps.id = d.app_id`); err != nil {
		t.Fatal(err)
	}
	e.call(t, e.cookie, http.MethodPut, envPath("env-restart", "B"), map[string]any{"value": "2"})
	if n := e.restartJobs(t, "env-restart"); n != 1 {
		t.Fatalf("restart jobs after a change = %d, want 1", n)
	}
	e.call(t, e.cookie, http.MethodDelete, envPath("env-restart", "B"), nil)
	if n := e.restartJobs(t, "env-restart"); n != 2 {
		t.Fatalf("restart jobs after a delete = %d, want 2", n)
	}

	// Stopped by the user: the change waits for the next start.
	e.call(t, e.cookie, http.MethodPost, "/api/v1/apps/env-restart/stop", nil)
	e.call(t, e.cookie, http.MethodPut, envPath("env-restart", "C"), map[string]any{"value": "3"})
	if n := e.restartJobs(t, "env-restart"); n != 2 {
		t.Fatalf("a stopped app was restarted: jobs = %d, want 2", n)
	}
}
