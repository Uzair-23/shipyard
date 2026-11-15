package security

// Upload attacks: a malicious source archive must never be accepted if
// unpacking it could write or point outside the upload root (on Linux or a
// Windows host), and must not be able to exhaust the API's CPU or memory.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Uzair-23/shipyard/internal/build"
)

type tarEntry struct {
	hdr  tar.Header
	body string
}

func writeTar(t *testing.T, w io.Writer, entries ...tarEntry) {
	t.Helper()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func evilArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writeTar(t, gz, entries...)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reg(name string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}
}
func link(name, target string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

// assertRejected checks that the archive is refused and that nothing,
// not even a partial archive, is left where it would have been stored.
func assertRejected(t *testing.T, archive io.Reader) {
	t.Helper()
	dir := t.TempDir()
	err := build.NormalizeFile(context.Background(), archive, filepath.Join(dir, "source.tar.gz"), build.DefaultLimits)
	var ue *build.UploadError
	if !errors.As(err, &ue) {
		t.Fatalf("malicious archive accepted (err = %v)", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("rejected archive left files behind: %v", left)
	}
}

func TestUploadPathTraversalRejected(t *testing.T) {
	names := []string{
		"../escape",
		"../../../../etc/cron.d/evil",
		"app/../../escape",
		"./../escape",
		"/etc/passwd",
		"//etc/passwd",
		`..\escape`,
		`app\..\..\escape`,
		`C:\Windows\System32\evil.dll`,
		"C:/Windows/evil",
		"app.py:$DATA",
		"app/..",
		"..",
		"...",
		"app/.. ",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			assertRejected(t, bytes.NewReader(evilArchive(t, reg("ok.txt"), reg(name))))
		})
	}
}

func TestUploadSymlinkEscapeRejected(t *testing.T) {
	tests := map[string][]tarEntry{
		"absolute target":    {reg("ok"), link("passwd", "/etc/passwd")},
		"relative escape":    {reg("ok"), link("up", "../../../etc")},
		"escape from subdir": {reg("ok"), link("a/b/up", "../../../x")},
		"link to parent":     {reg("ok"), link("up", "..")},
		"windows target":     {reg("ok"), link("up", `..\..\x`)},
		// Writing through a symlinked directory, with a contained target, so
		// only the "nothing under a symlink" rule can catch it.
		"write through link dir":  {link("d", "sub"), reg("sub/ok"), reg("d/evil")},
		"write through root link": {link("d", "."), reg("d/x")},
		// Each link looks contained lexically, but a -> . makes a/../y escape.
		"escape via another link": {reg("ok"), link("a", "."), link("b", "a/../y")},
		// Link declared after the file that sits under it.
		"late link over dir": {reg("d/x"), link("d", ".")},
		// Case-insensitive filesystems treat D and d as the same directory.
		"case-folded link dir": {link("D", "."), reg("d/x")},
		"hard link":            {reg("ok"), {hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}}},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			assertRejected(t, bytes.NewReader(evilArchive(t, entries...)))
		})
	}
}

// On a Windows host, "d." and "d " name the same directory as "d", and device
// names and 8.3 aliases resolve to other files. These must not bypass the
// symlink and duplicate checks.
func TestUploadWindowsNameEquivalenceRejected(t *testing.T) {
	tests := map[string][]tarEntry{
		"link dir with trailing dot":   {link("d.", "."), reg("d/x")},
		"link dir with trailing space": {link("d ", "."), reg("d/x")},
		"duplicate via trailing space": {reg("app.py"), reg("APP.PY ")},
		"device name":                  {reg("ok"), reg("NUL")},
		"device name with extension":   {reg("ok"), reg("src/con.py")},
		"short-name alias":             {reg("longname/x"), reg("LONGNA~1/x")},
		"non-ASCII lookalike":          {reg("ok"), reg("\u212a.txt")}, // Kelvin sign folds to k on NTFS
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			assertRejected(t, bytes.NewReader(evilArchive(t, entries...)))
		})
	}
}

// Data the validator does not parse (after the end-of-archive marker, or in a
// second gzip member) could still be extracted by other tools.
func TestUploadHiddenTrailingEntriesRejected(t *testing.T) {
	t.Run("second tar in the same gzip stream", func(t *testing.T) {
		var raw bytes.Buffer
		writeTar(t, &raw, reg("ok"))
		writeTar(t, &raw, reg("../evil"))
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(raw.Bytes())
		_ = gz.Close()
		assertRejected(t, &buf)
	})
	t.Run("second gzip member", func(t *testing.T) {
		archive := append(evilArchive(t, reg("ok")), evilArchive(t, reg("../evil"))...)
		assertRejected(t, bytes.NewReader(archive))
	})
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// streamArchive gzips a tar whose single file has size bytes of zeros,
// generated on the fly so the test never holds it in memory.
func streamArchive(t *testing.T, size int64) io.ReadCloser {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		err := tw.WriteHeader(&tar.Header{Name: "bomb", Typeflag: tar.TypeReg, Mode: 0o644, Size: size})
		if err == nil {
			_, err = io.CopyN(tw, zeros{}, size)
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = gz.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func TestUploadDecompressionBombRejected(t *testing.T) {
	r := streamArchive(t, 1<<30) // 1 GB of zeros, ~1 MB compressed
	defer func() { _ = r.Close() }()
	start := time.Now()
	assertRejected(t, r)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("rejecting the bomb took %s; it must be rejected without decompressing it", d)
	}
}

func TestUploadMetadataFloodRejected(t *testing.T) {
	big := strings.Repeat("a", 512<<10) // compresses to almost nothing
	t.Run("global header flood", func(t *testing.T) {
		entries := make([]tarEntry, 0, 5002)
		for range 5001 {
			entries = append(entries, tarEntry{hdr: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}})
		}
		assertRejected(t, bytes.NewReader(evilArchive(t, append(entries, reg("ok"))...)))
	})
	t.Run("huge PAX path", func(t *testing.T) {
		start := time.Now()
		assertRejected(t, bytes.NewReader(evilArchive(t, reg("ok"), reg(strings.Repeat("a/", len(big)/2)+"f"))))
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("rejecting a huge path took %s", d)
		}
	})
	t.Run("huge symlink target", func(t *testing.T) {
		assertRejected(t, bytes.NewReader(evilArchive(t, reg("ok"), link("l", strings.Repeat("x/", len(big)/2)))))
	})
	t.Run("directory carrying data", func(t *testing.T) {
		assertRejected(t, bytes.NewReader(evilArchive(t, reg("ok"),
			tarEntry{hdr: tar.Header{Name: "d/", Typeflag: tar.TypeDir, Size: 100 << 20}})))
	})
}

// Normalize must strip privilege-relevant metadata, so a setuid binary or a
// file-capability xattr never reaches an image.
func TestUploadPrivilegedMetadataStripped(t *testing.T) {
	archive := evilArchive(t, tarEntry{
		hdr: tar.Header{
			Name: "bin/escalate", Typeflag: tar.TypeReg, Mode: 0o6755, Uid: 0, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02"},
		},
		body: "\x7fELF",
	})
	dst := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := build.NormalizeFile(context.Background(), bytes.NewReader(archive), dst, build.DefaultLimits); err != nil {
		t.Fatalf("NormalizeFile: %v", err)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Mode&0o7000 != 0 || len(h.PAXRecords) != 0 {
		t.Fatalf("privileged metadata survived: mode=%o pax=%v", h.Mode, h.PAXRecords)
	}
}
