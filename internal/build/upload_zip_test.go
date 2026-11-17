package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

type zent struct {
	name string
	body string
	mode fs.FileMode // 0 = regular 0644
}

func zipOf(t *testing.T, ents ...zent) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		switch {
		case e.mode != 0:
			h.SetMode(e.mode)
		case strings.HasSuffix(e.name, "/"):
			h.SetMode(fs.ModeDir | 0o755)
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("create %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// names returns the sorted entry names (with type marks) of a normalised archive.
func names(t *testing.T, out []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name)
	}
	sort.Strings(got)
	return got
}

func equalNames(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func TestZipProducesTheSameOutputAsTar(t *testing.T) {
	z := zipOf(t,
		zent{name: "app.py", body: "print('hi')"},
		zent{name: "static/"},
		zent{name: "static/style.css", body: "body{}"},
		zent{name: "static/main.css", body: "style.css", mode: fs.ModeSymlink | 0o777},
	)
	fromZip, err := runNormalize(t, z, DefaultLimits)
	if err != nil {
		t.Fatalf("zip rejected: %v", err)
	}
	fromTar, err := runNormalize(t, targz(t,
		file("app.py", "print('hi')"),
		dir("static/"),
		file("static/style.css", "body{}"),
		symlink("static/main.css", "style.css"),
	), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromZip, fromTar) {
		t.Fatalf("zip and tar of the same content normalise differently:\nzip %v\ntar %v", names(t, fromZip), names(t, fromTar))
	}
}

func TestZipFolderWrapperIsRemoved(t *testing.T) {
	tests := []struct {
		name string
		ents []zent
		want []string
	}{
		{"one wrapper folder", []zent{{name: "myapp/"}, {name: "myapp/app.py", body: "x"}, {name: "myapp/lib/"}, {name: "myapp/lib/a.py", body: "y"}},
			[]string{"app.py", "lib/", "lib/a.py"}},
		{"wrapper without its own entry", []zent{{name: "myapp/app.py", body: "x"}},
			[]string{"app.py"}},
		{"file at the top level: keep the folder", []zent{{name: "app.py", body: "x"}, {name: "data/x.txt", body: "y"}},
			[]string{"app.py", "data/x.txt"}},
		{"two folders: keep both", []zent{{name: "a/x", body: "1"}, {name: "b/y", body: "2"}},
			[]string{"a/x", "b/y"}},
		{"macOS junk is dropped, wrapper still removed", []zent{{name: "myapp/app.py", body: "x"}, {name: "__MACOSX/"}, {name: "__MACOSX/myapp/._app.py", body: "junk"}},
			[]string{"app.py"}},
		{"Windows separators", []zent{{name: `myapp\`}, {name: `myapp\sub\a.txt`, body: "x"}},
			[]string{"sub/a.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runNormalize(t, zipOf(t, tt.ents...), DefaultLimits)
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if got := names(t, out); !equalNames(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZipRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name   string
		ents   []zent
		reason string
	}{
		{"parent traversal", []zent{{name: "ok.txt", body: "x"}, {name: "../evil", body: "x"}}, "'..'"},
		{"traversal with backslashes", []zent{{name: "ok.txt", body: "x"}, {name: `..\evil`, body: "x"}}, "'..'"},
		{"deep traversal", []zent{{name: "ok.txt", body: "x"}, {name: "a/../../evil", body: "x"}}, "'..'"},
		{"absolute path", []zent{{name: "/etc/cron.d/x", body: "x"}}, "absolute"},
		{"windows drive path", []zent{{name: `C:\Windows\x`, body: "x"}}, "may not contain"},
		{"symlink out of the upload", []zent{{name: "ok.txt", body: "x"}, {name: "l", body: "../../etc/passwd", mode: fs.ModeSymlink | 0o777}}, "outside"},
		{"absolute symlink", []zent{{name: "ok.txt", body: "x"}, {name: "l", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777}}, "absolute"},
		{"write through a symlink", []zent{{name: "ok.txt", body: "x"}, {name: "l", body: ".", mode: fs.ModeSymlink | 0o777}, {name: "l/x", body: "x"}}, "symlink"},
		{"named pipe", []zent{{name: "ok.txt", body: "x"}, {name: "p", mode: fs.ModeNamedPipe | 0o644}}, "unsupported entry type"},
		{"device", []zent{{name: "ok.txt", body: "x"}, {name: "d", mode: fs.ModeDevice | 0o644}}, "unsupported entry type"},
		{"case-insensitive duplicate", []zent{{name: "App.py", body: "1"}, {name: "app.PY", body: "2"}}, "duplicate"},
		{"non-ASCII name", []zent{{name: "résumé.txt", body: "x"}}, "printable ASCII"},
		{"Windows reserved name", []zent{{name: "ok.txt", body: "x"}, {name: "CON.txt", body: "x"}}, "reserved"},
		{"only directories", []zent{{name: "a/"}, {name: "a/b/"}}, "no files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runNormalize(t, zipOf(t, tt.ents...), DefaultLimits)
			assertUploadError(t, err, tt.reason)
		})
	}
}

func TestZipLimits(t *testing.T) {
	small := DefaultLimits
	small.MaxUnpackedBytes = 1 << 20
	small.MaxEntries = 3

	t.Run("decompression bomb", func(t *testing.T) {
		z := zipOf(t, zent{name: "zeros", body: strings.Repeat("\x00", 8<<20)})
		if len(z) > 100<<10 {
			t.Fatalf("test archive should be tiny, is %d bytes", len(z))
		}
		_, err := runNormalize(t, z, small)
		assertUploadError(t, err, "unpacks to more than")
	})
	t.Run("many entries", func(t *testing.T) {
		_, err := runNormalize(t, zipOf(t, zent{name: "a", body: "1"}, zent{name: "b", body: "2"}, zent{name: "c", body: "3"}, zent{name: "d", body: "4"}), small)
		assertUploadError(t, err, "more than 3 entries")
	})
	t.Run("upload larger than the limit", func(t *testing.T) {
		lim := DefaultLimits
		lim.MaxCompressedBytes = 100
		_, err := runNormalize(t, zipOf(t, zent{name: "a", body: strings.Repeat("abc", 1000)}), lim)
		assertUploadError(t, err, "larger than")
	})
}

// rawZip builds a one-file zip whose header sizes and CRC are chosen by the
// caller, to model archives that lie about their content.
func rawZip(t *testing.T, name string, data []byte, claimed uint64, crc uint32) []byte {
	t.Helper()
	var comp bytes.Buffer
	fw, err := flate.NewWriter(&comp, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = fw.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: name, Method: zip.Deflate, CRC32: crc,
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: claimed,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(comp.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestZipHeaderThatUnderstatesTheSizeIsRejected(t *testing.T) {
	small := DefaultLimits
	small.MaxUnpackedBytes = 1 << 20
	data := bytes.Repeat([]byte{0}, 8<<20)
	z := rawZip(t, "lie", data, 10, crc32.ChecksumIEEE(data))
	_, err := runNormalize(t, z, small)
	if err == nil {
		t.Fatal("accepted a zip entry that is larger than its header says")
	}
	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want *UploadError", err)
	}
}

func TestZipWrongChecksumIsRejected(t *testing.T) {
	data := []byte("hello")
	_, err := runNormalize(t, rawZip(t, "a.txt", data, uint64(len(data)), 0xdeadbeef), DefaultLimits)
	assertUploadError(t, err, "corrupt zip entry")
}

func TestZipEncryptedEntryIsRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "secret.txt", Method: zip.Store, Flags: 0x1})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_, err = runNormalize(t, buf.Bytes(), DefaultLimits)
	assertUploadError(t, err, "encrypted")
}

func TestZipTruncatedIsRejected(t *testing.T) {
	z := zipOf(t, zent{name: "a.txt", body: strings.Repeat("x", 4096)})
	_, err := runNormalize(t, z[:len(z)/2], DefaultLimits)
	assertUploadError(t, err, "not a valid zip")
}

func TestZipOutputIsDetectableAndIdempotent(t *testing.T) {
	out, err := runNormalize(t, zipOf(t, zent{name: "myapp/app.py", body: "x"}), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	again, err := runNormalize(t, out, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, again) {
		t.Fatal("normalising the output again changed it")
	}
}
