//go:build integration

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sseEvent struct{ id, event, data string }

// readSSE reads events from a stream until it ends, or fails after timeout.
func readSSE(t *testing.T, resp *http.Response, timeout time.Duration) []sseEvent {
	t.Helper()
	type result struct {
		events []sseEvent
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var events []sseEvent
		var cur sseEvent
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur != (sseEvent{}) {
					events = append(events, cur)
					cur = sseEvent{}
				}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data = line[6:]
			}
		}
		done <- result{events, sc.Err()}
	}()
	defer func() { _ = resp.Body.Close() }()
	select {
	case r := <-done:
		return r.events
	case <-time.After(timeout):
		_ = resp.Body.Close()
		t.Fatalf("the stream did not end within %s", timeout)
		return nil
	}
}

func (e env) openLogs(t *testing.T, srv *httptest.Server, cookie *http.Cookie, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// seedDeploy creates an app with one deploy in the given state and returns the deploy id.
func seedDeploy(t *testing.T, e env, slug, state string) string {
	t.Helper()
	e.createApp(t, slug)
	var id string
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO deploys (app_id, seq, source_ref, state) SELECT id, 1, 'sha256:x', $2 FROM apps WHERE slug = $1 RETURNING id::text`, slug, state).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addLogs(t *testing.T, deploy, stream string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO log_lines (app_id, deploy_id, stream, line) SELECT app_id, id, $2, $3 FROM deploys WHERE id = $1`, deploy, stream, l); err != nil {
			t.Fatal(err)
		}
	}
}

func logLines(t *testing.T, events []sseEvent) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		if ev.event != "log" {
			continue
		}
		var p struct {
			ID     int64
			Stream string
			Line   string
		}
		if err := json.Unmarshal([]byte(ev.data), &p); err != nil || fmt.Sprint(p.ID) != ev.id {
			t.Fatalf("bad log event %+v: %v", ev, err)
		}
		out = append(out, p.Stream+":"+p.Line)
	}
	return out
}

func TestDeployLogStreamsStoredLinesAndEnds(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	deploy := seedDeploy(t, e, "log-done", "failed")
	nasty := `</script><img src=x onerror=alert(1)> "quotes" \backslash`
	addLogs(t, deploy, "system", "Build started")
	addLogs(t, deploy, "build", "Step 1/3 : FROM python", nasty)
	addLogs(t, deploy, "system", "build failed: exit 1")

	resp := e.openLogs(t, srv, e.cookie, "/api/v1/apps/log-done/deploys/1/logs", nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("response: %d %v", resp.StatusCode, resp.Header)
	}
	events := readSSE(t, resp, 10*time.Second)
	got := logLines(t, events)
	want := []string{"system:Build started", "build:Step 1/3 : FROM python", "build:" + nasty, "system:build failed: exit 1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q\nwant %q", got, want)
	}
	if last := events[len(events)-1]; last.event != "end" || last.data != `{"state":"failed"}` {
		t.Fatalf("last event = %+v, want end/failed", last)
	}

	// Reconnecting resumes after the last id seen (header and query forms).
	firstID := events[0].id
	for name, resp := range map[string]*http.Response{
		"Last-Event-ID": e.openLogs(t, srv, e.cookie, "/api/v1/apps/log-done/deploys/1/logs", map[string]string{"Last-Event-ID": firstID}), //nolint:bodyclose // readSSE closes it
		"after":         e.openLogs(t, srv, e.cookie, "/api/v1/apps/log-done/deploys/1/logs?after="+firstID, nil),                          //nolint:bodyclose // readSSE closes it
	} {
		if got := logLines(t, readSSE(t, resp, 10*time.Second)); len(got) != 3 || got[0] != "build:Step 1/3 : FROM python" {
			t.Errorf("resume via %s: %q", name, got)
		}
	}
}

// A build in progress: the stream delivers lines as they are written and the
// final line (written together with the state change) before the end event.
func TestDeployLogFollowsABuildInProgress(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	deploy := seedDeploy(t, e, "log-live", "building")
	addLogs(t, deploy, "system", "Build started")

	resp := e.openLogs(t, srv, e.cookie, "/api/v1/apps/log-live/deploys/1/logs", nil) //nolint:bodyclose // readSSE closes it
	go func() {
		time.Sleep(900 * time.Millisecond)
		addLogs(t, deploy, "build", "Step 1/2", "Step 2/2")
		time.Sleep(700 * time.Millisecond)
		// Last line and state change in one transaction, as the pipeline does.
		_, _ = testPool.Exec(context.Background(), `
			WITH l AS (INSERT INTO log_lines (app_id, deploy_id, stream, line) SELECT app_id, id, 'system', 'Deploy is live' FROM deploys WHERE id = $1)
			UPDATE deploys SET state = 'live' WHERE id = $1`, deploy)
	}()
	events := readSSE(t, resp, 20*time.Second)
	got := logLines(t, events)
	want := "system:Build started\nbuild:Step 1/2\nbuild:Step 2/2\nsystem:Deploy is live"
	if strings.Join(got, "\n") != want {
		t.Fatalf("lines = %q", got)
	}
	if last := events[len(events)-1]; last.event != "end" || last.data != `{"state":"live"}` {
		t.Fatalf("last event = %+v", last)
	}
}

func TestDeployLogIsOwnerOnlyAndValidatesTheDeploy(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	deploy := seedDeploy(t, e, "log-priv", "failed")
	addLogs(t, deploy, "system", "secret build detail")
	other := signup(t, e.handler, "log-other@example.com")

	cases := []struct {
		name   string
		cookie *http.Cookie
		path   string
		want   int
	}{
		{"owner", e.cookie, "/api/v1/apps/log-priv/deploys/1/logs", http.StatusOK},
		{"another user", other, "/api/v1/apps/log-priv/deploys/1/logs", http.StatusNotFound},
		{"anonymous", nil, "/api/v1/apps/log-priv/deploys/1/logs", http.StatusUnauthorized},
		{"no such deploy", e.cookie, "/api/v1/apps/log-priv/deploys/2/logs", http.StatusNotFound},
		{"seq zero", e.cookie, "/api/v1/apps/log-priv/deploys/0/logs", http.StatusNotFound},
		{"seq not a number", e.cookie, "/api/v1/apps/log-priv/deploys/x/logs", http.StatusNotFound},
		{"huge seq", e.cookie, "/api/v1/apps/log-priv/deploys/99999999999/logs", http.StatusNotFound},
		{"no such app", e.cookie, "/api/v1/apps/nope-app/deploys/1/logs", http.StatusNotFound},
	}
	for _, c := range cases {
		resp := e.openLogs(t, srv, c.cookie, c.path, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
		if c.want != http.StatusOK {
			var body strings.Builder
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				body.WriteString(sc.Text())
			}
			if strings.Contains(body.String(), "secret build detail") {
				t.Errorf("%s: the log leaked: %s", c.name, body.String())
			}
		}
	}
}
