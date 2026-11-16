package api

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		slug string
		ok   bool
	}{
		{"abc", true},
		{"my-app-2", true},
		{"a1b", true},
		{strings.Repeat("a", 30), true},
		{strings.Repeat("a", 31), false},
		{"ab", false},
		{"", false},
		{"-abc", false},
		{"abc-", false},
		{"ABC", false},
		{"my_app", false},
		{"my.app", false},
		{"my app", false},
		{"abc/def", false},
		{"..a", false},
		{"é-app", false},
		{"abc\n", false},
		{"www", false},
		{"api", false},
		{"shipyard", false},
		{"xn--pple-43d", false},
		{"ab--cd", false},
		{"login", false},
		{"a-b--c", true},
	}
	for _, tt := range tests {
		if got := ValidateSlug(tt.slug) == ""; got != tt.ok {
			t.Errorf("ValidateSlug(%q) ok = %v, want %v", tt.slug, got, tt.ok)
		}
	}
}

// internal/api must never talk to Docker: it may not import internal/runtime
// or the Docker SDK directly (CLAUDE.md boundary rules).
func TestAPIDoesNotImportRuntimeOrDocker(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(p, "/internal/runtime") || strings.HasPrefix(p, "github.com/moby/") ||
				strings.HasPrefix(p, "github.com/docker/") {
				t.Errorf("%s imports %s: the API must not talk to Docker", f, p)
			}
		}
	}
}
