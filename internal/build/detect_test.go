package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  Runtime
		err   error
	}{
		{"requirements.txt", []string{"app.py", "requirements.txt"}, Python, nil},
		{"pyproject.toml", []string{"pyproject.toml", "src/app/main.py"}, Python, nil},
		{"package.json", []string{"package.json", "index.js"}, Node, nil},
		{"index.html", []string{"index.html", "style.css"}, Static, nil},
		{"python wins over index.html", []string{"index.html", "requirements.txt"}, Python, nil},
		{"node wins over index.html (e.g. Vite)", []string{"index.html", "package.json", "src/main.ts"}, Node, nil},
		{"python and node is ambiguous", []string{"package.json", "requirements.txt"}, "", ErrAmbiguousRuntime},
		{"markers in subdirectories are ignored", []string{"docs/requirements.txt", "web/package.json", "site/index.html"}, "", ErrNoRuntime},
		{"subdir marker does not override top-level", []string{"package.json", "scripts/requirements.txt"}, Node, nil},
		{"names are exact", []string{"Requirements.txt", "PACKAGE.JSON", "Index.html"}, "", ErrNoRuntime},
		{"nothing recognisable", []string{"main.go", "go.mod"}, "", ErrNoRuntime},
		{"empty", nil, "", ErrNoRuntime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Detect(tt.paths)
			if !errors.Is(err, tt.err) || got != tt.want {
				t.Fatalf("Detect(%v) = %q, %v; want %q, %v", tt.paths, got, err, tt.want, tt.err)
			}
		})
	}
}

// End to end on real-tool archives: normalise, list paths, detect.
func TestDetectOnNormalizedRealArchives(t *testing.T) {
	for _, name := range []string{"gnu-tar", "bsdtar", "git-archive", "python-tarfile"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "uploads", name+".tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runNormalize(t, data, DefaultLimits)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			gz, err := gzip.NewReader(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			tr := tar.NewReader(gz)
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				paths = append(paths, h.Name)
			}
			if got, err := Detect(paths); err != nil || got != Python {
				t.Fatalf("Detect = %q, %v; want python (paths %v)", got, err, paths)
			}
		})
	}
}
