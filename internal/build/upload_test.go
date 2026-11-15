package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	hdr  tar.Header
	body string
}

func file(name, body string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: body}
}
func dir(name string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}
func symlink(name, to string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: to}}
}
func hardlink(name, to string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: to}}
}
func global() entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "c"}}}
}

func rawTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", hdr.Name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func targz(t *testing.T, entries ...entry) []byte { return gzipBytes(t, rawTar(t, entries...)) }

func runNormalize(t *testing.T, archive []byte, lim Limits) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := normalizeTo(context.Background(), &out, bytes.NewReader(archive), lim)
	return out.Bytes(), err
}

func TestNormalizeAcceptsNormalSource(t *testing.T) {
	archive := targz(t,
		global(), // git archive writes one
		dir("./"),
		file("./app.py", "print('hi')"),
		file("./requirements.txt", "flask\n"),
		dir("./static/"),
		file("./static/style.css", "body{}"),
		symlink("./static/main.css", "style.css"),
		symlink("./assets", "static"),
		dir("./lib/"),
		symlink("./lib/up", "../app.py"),
		file("node_modules/.bin/x.cmd", "@echo"),
		file("a-b_c+d@e (1).txt", "spaces and punctuation are fine"),
	)
	if _, err := runNormalize(t, archive, DefaultLimits); err != nil {
		t.Fatalf("rejected a normal source: %v", err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	long := strings.Repeat("a", 256)
	deep := strings.Repeat("d/", 64) + "f"
	tests := []struct {
		name    string
		entries []entry
		reason  string
	}{
		{"absolute path", []entry{file("/etc/passwd", "x")}, "absolute"},
		{"parent dir", []entry{file("../evil", "x")}, "'..'"},
		{"nested parent dir", []entry{file("a/../../evil", "x")}, "'..'"},
		{"parent dir that cleans inside", []entry{file("a/../b", "x")}, "'..'"},
		{"backslash traversal", []entry{file(`..\..\evil`, "x")}, "may not contain"},
		{"windows drive", []entry{file("C:/evil", "x")}, "may not contain"},
		{"alternate data stream", []entry{file("app.py:hidden", "x")}, "may not contain"},
		{"wildcard", []entry{file("a*b", "x")}, "may not contain"},
		{"control character", []entry{file("a\x01b", "x")}, "printable ASCII"},
		{"non-ASCII", []entry{file("résumé.txt", "x")}, "printable ASCII"},
		{"trailing dot", []entry{file("app.py.", "x")}, "end with a dot or a space"},
		{"trailing space", []entry{file("app.py ", "x")}, "end with a dot or a space"},
		{"dots only", []entry{file("a/.../b", "x")}, "end with a dot or a space"},
		{"reserved name", []entry{file("CON", "x")}, "reserved device name"},
		{"reserved name with extension", []entry{file("lib/nul.txt", "x")}, "reserved device name"},
		{"reserved COM port", []entry{file("com1.py", "x")}, "reserved device name"},
		{"short-name alias", []entry{file("PROGRA~1/x", "x")}, "short-name alias"},
		{"path too long", []entry{file(strings.Repeat("abcdefgh/", 120)+"f", "x")}, "longer than 1024"},
		{"component too long", []entry{file(long, "x")}, "longer than 255"},
		{"too deep", []entry{file(deep, "x")}, "more than 64 levels"},
		{"duplicate path", []entry{file("app.py", "a"), file("app.py", "b")}, "duplicate"},
		{"case-insensitive duplicate", []entry{file("App.py", "a"), file("app.PY", "b")}, "duplicate"},
		{"file used as directory", []entry{file("a", "x"), file("a/b", "y")}, "is a file"},
		{"hard link", []entry{file("a", "x"), hardlink("b", "a")}, "hard links"},
		{"char device", []entry{file("a", "x"), {hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}}, "unsupported entry type"},
		{"fifo", []entry{file("a", "x"), {hdr: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}}}, "unsupported entry type"},
		{"directory with data", []entry{file("a", "x"), {hdr: tar.Header{Name: "d/", Typeflag: tar.TypeDir, Size: 1 << 20}}}, "only regular files"},
		{"symlink absolute", []entry{file("a", "x"), symlink("l", "/etc/passwd")}, "absolute"},
		{"symlink escapes", []entry{file("a", "x"), symlink("l", "../../etc/passwd")}, "outside"},
		{"symlink escapes from subdir", []entry{file("a", "x"), symlink("d/l", "../../x")}, "outside"},
		{"symlink to root parent", []entry{file("a", "x"), symlink("l", "..")}, "outside"},
		{"symlink empty target", []entry{file("a", "x"), symlink("l", "")}, "empty target"},
		{"symlink backslash target", []entry{file("a", "x"), symlink("l", `..\..\x`)}, "may not contain"},
		{"symlink target trailing dot", []entry{file("a", "x"), symlink("l", "a.")}, "end with a dot"},
		{"symlink target too long", []entry{file("a", "x"), symlink("l", strings.Repeat("x/", 600))}, "longer than 1024"},
		{"file under symlinked dir", []entry{symlink("d", "."), file("d/a", "x")}, "goes through symlink"},
		{"file under symlinked dir, link later", []entry{file("d/a", "x"), symlink("d", ".")}, "goes through symlink"},
		{"file under symlink, other case", []entry{symlink("D", "."), file("d/a", "x")}, "goes through symlink"},
		{"chained symlink escape", []entry{file("x", "x"), symlink("a", "."), symlink("b", "a/../../x")}, "outside"},
		{"escape via another link", []entry{file("x", "x"), symlink("a", "."), symlink("b", "a/../y")}, "through another symlink"},
		{"symlink through symlink", []entry{file("x", "x"), symlink("a", "."), symlink("b", "a/x")}, "through another symlink"},
		{"reserved name with space before extension", []entry{file("CON .txt", "x")}, "reserved device name"},
		{"reserved name with trailing dot", []entry{file("con.", "x")}, "end with a dot"},
		{"reserved LPT port with extension", []entry{file("LPT9.x", "x")}, "reserved device name"},
		{"symlink target with dots-only component", []entry{file("a", "x"), symlink("l", "a/.../b")}, "end with a dot"},
		{"symlink target with dot-space component", []entry{file("a", "x"), symlink("l", "x. /y")}, "end with a dot or a space"},
		{"regular file as root", []entry{{hdr: tar.Header{Name: ".", Typeflag: tar.TypeReg}, body: "hidden"}, file("a", "x")}, "root must be a directory"},
		{"continuation entry", []entry{file("a", "x"), {hdr: tar.Header{Name: "c", Typeflag: tar.TypeCont}}}, "unsupported entry type"},
		{"only directories", []entry{dir("a/"), dir("a/b/")}, "no files"},
		{"empty archive", nil, "no files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runNormalize(t, targz(t, tt.entries...), DefaultLimits)
			assertUploadError(t, err, tt.reason)
		})
	}
}

func TestNormalizeLimits(t *testing.T) {
	small := Limits{MaxCompressedBytes: 1 << 20, MaxUnpackedBytes: 1 << 20, MaxEntries: 3, MaxOutputBytes: 2 << 20}

	t.Run("too many entries", func(t *testing.T) {
		archive := targz(t, file("a", "1"), file("b", "2"), file("c", "3"), file("d", "4"))
		_, err := runNormalize(t, archive, small)
		assertUploadError(t, err, "more than 3 entries")
	})
	t.Run("global headers count as entries", func(t *testing.T) {
		archive := targz(t, global(), global(), global(), file("a", "1"))
		_, err := runNormalize(t, archive, small)
		assertUploadError(t, err, "more than 3 entries")
	})
	t.Run("one file too large", func(t *testing.T) {
		archive := targz(t, file("bomb", strings.Repeat("0", 2<<20))) // compresses to a few KB
		_, err := runNormalize(t, archive, small)
		assertUploadError(t, err, "unpacks to more than 1 MB")
	})
	t.Run("files together too large", func(t *testing.T) {
		half := strings.Repeat("0", 600<<10)
		archive := targz(t, file("a", half), file("b", half))
		_, err := runNormalize(t, archive, small)
		assertUploadError(t, err, "unpacks to more than 1 MB")
	})
	t.Run("compressed too large", func(t *testing.T) {
		noise := make([]byte, 1536<<10) // random data does not compress
		if _, err := rand.Read(noise); err != nil {
			t.Fatal(err)
		}
		lim := small
		lim.MaxUnpackedBytes = 4 << 20
		_, err := runNormalize(t, targz(t, file("noise", string(noise))), lim)
		assertUploadError(t, err, "larger than 1 MB")
	})
	t.Run("exactly at entry limit is fine", func(t *testing.T) {
		if _, err := runNormalize(t, targz(t, file("a", "1"), file("b", "2"), file("c", "3")), small); err != nil {
			t.Fatalf("rejected archive at the limit: %v", err)
		}
	})
}

func TestNormalizeRejectsMalformedArchives(t *testing.T) {
	good := rawTar(t, file("a", "x"))
	tests := []struct {
		name   string
		data   []byte
		reason string
	}{
		{"not gzip", []byte("PK\x03\x04 this is a zip"), "not a gzip"},
		{"gzip of garbage", gzipBytes(t, bytes.Repeat([]byte("not a tar header "), 64)), "corrupt tar"},
		{"truncated", truncated(targz(t, file("a", strings.Repeat("x", 4096)))), "corrupt"},
		{"second tar after end-of-archive", gzipBytes(t, append(good, rawTar(t, file("../evil", "x"))...)), "after the end of the tar archive"},
		{"second gzip member", append(targz(t, file("a", "x")), targz(t, file("../evil", "x"))...), "only one gzip stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runNormalize(t, tt.data, DefaultLimits)
			assertUploadError(t, err, tt.reason)
		})
	}
}

func TestNormalizeOutput(t *testing.T) {
	input := targz(t,
		global(),
		dir("./src"),
		entry{hdr: tar.Header{
			Name: "./src/run.sh", Typeflag: tar.TypeReg, Mode: 0o4777, Uid: 1000, Gid: 1000,
			Uname: "riya", Gname: "staff", Format: tar.FormatPAX,
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "cap"},
		}, body: "#!/bin/sh\n"},
		entry{hdr: tar.Header{Name: "src/data.txt", Typeflag: tar.TypeReg, Mode: 0o666}, body: "data"},
		symlink("./link", "src/data.txt"),
	)
	out, err := runNormalize(t, input, DefaultLimits)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	type got struct {
		typ  byte
		mode int64
		link string
		body string
	}
	want := map[string]got{
		"src/":         {tar.TypeDir, 0o755, "", ""},
		"src/run.sh":   {tar.TypeReg, 0o755, "", "#!/bin/sh\n"},
		"src/data.txt": {tar.TypeReg, 0o644, "", "data"},
		"link":         {tar.TypeSymlink, 0o777, "src/data.txt", ""},
	}
	gz, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	seen := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		w, ok := want[h.Name]
		if !ok {
			t.Fatalf("unexpected entry %q (type %c) in output", h.Name, h.Typeflag)
		}
		seen++
		if h.Typeflag != w.typ || h.Mode != w.mode || h.Linkname != w.link || string(body) != w.body {
			t.Errorf("%s: got type=%c mode=%o link=%q body=%q, want %+v", h.Name, h.Typeflag, h.Mode, h.Linkname, body, w)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || !h.ModTime.IsZero() && h.ModTime.Unix() != 0 {
			t.Errorf("%s: ownership/time not stripped: uid=%d gid=%d uname=%q gname=%q mtime=%v", h.Name, h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime)
		}
		if len(h.PAXRecords) != 0 {
			t.Errorf("%s: PAX records not stripped: %v", h.Name, h.PAXRecords)
		}
	}
	if seen != len(want) {
		t.Fatalf("output has %d entries, want %d", seen, len(want))
	}

	// Normalising the output again is a no-op, byte for byte.
	again, err := runNormalize(t, out, DefaultLimits)
	if err != nil {
		t.Fatalf("Normalize(output): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("Normalize is not idempotent")
	}
}

func TestUploadErrorQuotesAndTruncatesNames(t *testing.T) {
	name := "evil\x1b[31m" + strings.Repeat("x", 2000)
	_, err := runNormalize(t, targz(t, file(name, "x")), DefaultLimits)
	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want *UploadError", err)
	}
	msg := ue.Error()
	if strings.ContainsRune(msg, '\x1b') || len(msg) > 400 {
		t.Fatalf("message not sanitised (len %d): %q", len(msg), msg)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestNormalizeReportsOutputErrorsAsIO(t *testing.T) {
	err := normalizeTo(context.Background(), failingWriter{}, bytes.NewReader(targz(t, file("a", "x"))), DefaultLimits)
	var ue *UploadError
	if err == nil || errors.As(err, &ue) {
		t.Fatalf("got %v, want a plain I/O error", err)
	}
}

func TestNormalizeOutputLimit(t *testing.T) {
	noise := make([]byte, 64<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	lim := DefaultLimits
	lim.MaxOutputBytes = 16 << 10
	_, err := runNormalize(t, targz(t, file("noise", string(noise))), lim)
	assertUploadError(t, err, "once repacked")
}

func TestNormalizeStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := normalizeTo(ctx, io.Discard, bytes.NewReader(targz(t, file("a", "x"))), DefaultLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// Names and targets over 100 bytes use PAX/ustar-prefix encoding in the
// output; normalising again must still be a byte-for-byte no-op.
func TestNormalizeIdempotentWithLongNames(t *testing.T) {
	long := strings.Repeat("directory-name/", 12) + "file.txt" // ~190 bytes
	out, err := runNormalize(t, targz(t,
		file(long, "x"),
		symlink("link", long),
		symlink(strings.Repeat("l", 120), "link"),
	), DefaultLimits)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	again, err := runNormalize(t, out, DefaultLimits)
	if err != nil {
		t.Fatalf("Normalize(output): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("Normalize is not idempotent for long names")
	}
}

func TestNormalizeFileCommitsOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	dirPath := t.TempDir()
	dst := filepath.Join(dirPath, "source.tar.gz")

	// Rejected only by the whole-archive check that runs after every entry
	// was written: nothing may be left behind.
	evil := targz(t, symlink("d", "."), file("d/x", "x"))
	err := NormalizeFile(ctx, bytes.NewReader(evil), dst, DefaultLimits)
	assertUploadError(t, err, "goes through symlink")
	if left, _ := os.ReadDir(dirPath); len(left) != 0 {
		t.Fatalf("rejected upload left files behind: %v", left)
	}

	if err := NormalizeFile(ctx, bytes.NewReader(targz(t, file("a", "x"))), dst, DefaultLimits); err != nil {
		t.Fatalf("NormalizeFile: %v", err)
	}
	if left, _ := os.ReadDir(dirPath); len(left) != 1 || left[0].Name() != "source.tar.gz" {
		t.Fatalf("after success the directory holds %v, want only source.tar.gz", left)
	}
}

func truncated(b []byte) []byte { return b[:len(b)/2] }

func assertUploadError(t *testing.T, err error, reason string) {
	t.Helper()
	var ue *UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want *UploadError containing %q", err, reason)
	}
	if !strings.Contains(ue.Error(), reason) {
		t.Fatalf("got %q, want it to contain %q", ue.Error(), reason)
	}
}

// Archives produced by real tools must pass: GNU tar 1.35, bsdtar 3.8
// (Windows tar.exe), git archive (adds a PAX global header) and Python
// tarfile. Each packs app.py, requirements.txt, run.sh (+x), static/style.css.
func TestNormalizeAcceptsRealToolArchives(t *testing.T) {
	for _, name := range []string{"gnu-tar", "bsdtar", "git-archive", "python-tarfile"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "uploads", name+".tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runNormalize(t, data, DefaultLimits)
			if err != nil {
				t.Fatalf("rejected %s archive: %v", name, err)
			}
			gz, err := gzip.NewReader(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]int64{}
			tr := tar.NewReader(gz)
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Typeflag == tar.TypeReg {
					files[h.Name] = h.Mode
				}
			}
			for _, f := range []string{"app.py", "requirements.txt", "run.sh", "static/style.css"} {
				if _, ok := files[f]; !ok {
					t.Errorf("%s missing from output (got %v)", f, files)
				}
			}
		})
	}
}
