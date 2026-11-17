package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packDir returns a .tar.gz of dir's files, as a user would upload.
func packDir(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipDir returns a .zip of dir as a file manager makes it: everything inside
// one wrapper folder named after the directory.
func zipDir(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	wrapper := filepath.Base(dir) + "/"
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		w, err := zw.Create(wrapper + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The sample apps in testdata/apps must pass the whole upload-to-Dockerfile path.
func TestSampleApps(t *testing.T) {
	tests := []struct {
		dir       string
		runtime   Runtime
		wantStart string
	}{
		{"hello-python", Python, "flask --app app run"},
		{"hello-node", Node, "npm start"},
		{"whoami", Node, "npm start"},
		{"static-site", Static, ""},
	}
	for _, tt := range tests {
		for _, format := range []string{"tar.gz", "zip"} {
			t.Run(tt.dir+"/"+format, func(t *testing.T) {
				dst := filepath.Join(t.TempDir(), "out.tar.gz")
				appDir := filepath.Join("..", "..", "testdata", "apps", tt.dir)
				upload := packDir(t, appDir)
				if format == "zip" {
					upload = zipDir(t, appDir)
				}
				err := NormalizeFile(context.Background(), bytes.NewReader(upload), dst, DefaultLimits)
				if err != nil {
					t.Fatalf("NormalizeFile: %v", err)
				}
				f, err := os.Open(dst)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				src, cfg, err := ReadSource(f)
				if err != nil {
					t.Fatalf("ReadSource: %v", err)
				}
				plan, err := Prepare(src, cfg, "11111111-1111-1111-1111-111111111111")
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				if plan.Runtime != tt.runtime {
					t.Errorf("runtime = %s, want %s", plan.Runtime, tt.runtime)
				}
				if !strings.HasPrefix(plan.Start, tt.wantStart) {
					t.Errorf("start = %q, want prefix %q", plan.Start, tt.wantStart)
				}
				if len(plan.Dockerfile) == 0 {
					t.Error("no Dockerfile generated")
				}
			})
		}
	}
}
