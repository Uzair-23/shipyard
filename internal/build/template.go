package build

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

//go:embed templates/*.Dockerfile.tmpl
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.Dockerfile.tmpl"))

// SourceDir is where user files sit inside the build context. The generated
// Dockerfile is at the context root, so no upload can replace it (ADR 004).
const SourceDir = "src"

// Source is what the templates need to know about a normalised upload.
type Source struct {
	Paths        []string // every path in the archive
	Requirements []byte   // top-level requirements.txt, if present
	PyProject    []byte   // top-level pyproject.toml, if present
	PackageJSON  []byte   // top-level package.json, if present
}

// maxDepsFileBytes bounds how much of requirements.txt / pyproject.toml the
// start-command heuristics parse; larger files are ignored.
const maxDepsFileBytes = 64 << 10

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (s Source) has(name string) bool {
	for _, p := range s.Paths {
		if p == name {
			return true
		}
	}
	return false
}

// validStart reports whether a start command is safe to embed and display:
// non-blank valid UTF-8 without control characters (C0, DEL or C1).
func validStart(start string) bool {
	if strings.TrimSpace(start) == "" || !utf8.ValidString(start) {
		return false
	}
	for _, r := range start {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ErrNoStart means no start command was configured and none could be inferred.
var ErrNoStart = errors.New(`could not work out how to start the app: set start in shipyard.toml ` +
	`(for example start = "python app.py"); the app must listen on $PORT`)

// ErrFrontendProject means a Node project that is built into static files
// (it has a build script but nothing to run), which Shipyard does not build.
var ErrFrontendProject = errors.New(`this looks like a front-end project: package.json has a "build" script but no "start" script, ` +
	`and Shipyard runs servers and static files; it does not run builds. Run "npm run build" on your machine, then upload ` +
	`the contents of the build folder (dist/ or build/) as a .zip: it is served as a static site`)

// ErrNoNodeStart means a Node project with nothing to run.
var ErrNoNodeStart = errors.New(`package.json has no "start" script and there is no server.js: add a "start" script ` +
	`(the app must listen on $PORT) or set start in shipyard.toml`)

// nodeStartProblem explains why "npm start" would fail, or returns nil. npm
// runs "node server.js" when there is no start script but a server.js exists.
// A package.json that cannot be read is not judged: the build proceeds.
func nodeStartProblem(src Source) error {
	if len(src.PackageJSON) == 0 || len(src.PackageJSON) > maxDepsFileBytes {
		return nil
	}
	var pkg struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal(src.PackageJSON, &pkg) != nil {
		return nil
	}
	if _, ok := pkg.Scripts["start"]; ok || src.has("server.js") {
		return nil
	}
	if _, ok := pkg.Scripts["build"]; ok {
		return ErrFrontendProject
	}
	return ErrNoNodeStart
}

// DefaultStart infers a start command when shipyard.toml does not set one.
// Static sites have no start command.
func DefaultStart(rt Runtime, src Source) (string, error) {
	switch rt {
	case Static:
		return "", nil
	case Node:
		if err := nodeStartProblem(src); err != nil {
			return "", err
		}
		return "npm start", nil
	case Python:
	default:
		return "", fmt.Errorf("unknown runtime %q", rt)
	}

	deps := pythonDeps(src)
	pick := func(candidates ...string) string {
		for _, c := range candidates {
			if src.has(c) {
				return c
			}
		}
		return ""
	}
	module := func(file string) string { return strings.TrimSuffix(file, ".py") }

	switch {
	case deps["streamlit"]:
		if e := pick("streamlit_app.py", "app.py", "main.py"); e != "" {
			return "streamlit run " + e + " --server.port $PORT --server.address 0.0.0.0 --server.headless true", nil
		}
	case deps["fastapi"]:
		if e := pick("main.py", "app.py"); e != "" {
			return "uvicorn " + module(e) + ":app --host 0.0.0.0 --port $PORT", nil
		}
	case deps["flask"]:
		if e := pick("app.py", "main.py"); e != "" {
			// Never the debugger: with FLASK_DEBUG set it allows remote code execution.
			return "flask --app " + module(e) + " run --no-debugger --no-reload --host 0.0.0.0 --port $PORT", nil
		}
	default:
		if e := pick("app.py", "main.py"); e != "" {
			return "python " + e, nil
		}
	}
	return "", ErrNoStart
}

// pythonDeps returns normalised package names from requirements.txt and
// pyproject.toml's [project].dependencies. Files over maxDepsFileBytes are
// ignored.
func pythonDeps(src Source) map[string]bool {
	deps := map[string]bool{}
	add := func(spec string) {
		spec = strings.TrimSpace(spec)
		if spec == "" || strings.HasPrefix(spec, "#") || strings.HasPrefix(spec, "-") {
			return
		}
		if i := strings.IndexAny(spec, " <>=!~;[@#"); i >= 0 {
			spec = spec[:i]
		}
		name := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(spec))
		deps[name] = true
	}
	if len(src.Requirements) <= maxDepsFileBytes {
		sc := bufio.NewScanner(bytes.NewReader(src.Requirements))
		for sc.Scan() {
			add(sc.Text())
		}
	}
	if len(src.PyProject) > maxDepsFileBytes {
		return deps
	}
	var pp struct {
		Project struct {
			Dependencies []string `toml:"dependencies"`
		} `toml:"project"`
	}
	if _, err := toml.Decode(string(src.PyProject), &pp); err == nil {
		for _, d := range pp.Project.Dependencies {
			add(d)
		}
	}
	return deps
}

// Dockerfile renders the fixed template for rt. start is the shell command
// that runs the app (from shipyard.toml or DefaultStart); it becomes the
// third element of an exec-form CMD, JSON-encoded, so it cannot inject
// Dockerfile instructions. appID (a platform UUID, never user input) keys
// the build cache per app.
func Dockerfile(rt Runtime, start string, src Source, appID string) ([]byte, error) {
	if !uuidRE.MatchString(appID) {
		return nil, fmt.Errorf("app id %q is not a lower-case UUID", appID)
	}
	data := struct {
		AppID        string
		Cmd          string
		Requirements bool
		Lockfile     bool
	}{AppID: appID}

	switch rt {
	case Static:
		if start != "" {
			return nil, errors.New("static sites cannot have a start command")
		}
	case Python, Node:
		if strings.TrimSpace(start) == "" {
			return nil, ErrNoStart
		}
		if !validStart(start) {
			return nil, errors.New("start command must be valid UTF-8 without control characters")
		}
		cmd, err := json.Marshal([]string{"/bin/sh", "-c", "exec " + start})
		if err != nil {
			return nil, fmt.Errorf("encode CMD: %w", err)
		}
		data.Cmd = string(cmd)
	default:
		return nil, fmt.Errorf("unknown runtime %q", rt)
	}

	switch rt {
	case Python:
		data.Requirements = src.has("requirements.txt")
		if !data.Requirements && !src.has("pyproject.toml") {
			return nil, errors.New("python apps need requirements.txt or pyproject.toml at the top level")
		}
	case Node:
		if !src.has("package.json") {
			return nil, errors.New("node apps need package.json at the top level")
		}
		data.Lockfile = src.has("package-lock.json")
	}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, string(rt)+".Dockerfile.tmpl", data); err != nil {
		return nil, fmt.Errorf("render %s template: %w", rt, err)
	}
	return buf.Bytes(), nil
}
