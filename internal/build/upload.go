package build

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Limits bound what an uploaded source archive may contain (PRD §7.3).
type Limits struct {
	MaxCompressedBytes int64 // size of the .tar.gz as uploaded
	MaxUnpackedBytes   int64 // total size of all files once decompressed
	MaxEntries         int   // files, directories, symlinks and metadata headers
	MaxOutputBytes     int64 // size of the normalised .tar.gz
}

// DefaultLimits: 50 MB upload and 5,000 entries per the PRD; 250 MB unpacked
// guards against decompression bombs; the repacked output may be up to twice
// the upload (re-compression at BestSpeed can be larger than the original).
var DefaultLimits = Limits{
	MaxCompressedBytes: 50 << 20,
	MaxUnpackedBytes:   250 << 20,
	MaxEntries:         5000,
	MaxOutputBytes:     100 << 20,
}

const (
	maxPathLen      = 1024
	maxComponentLen = 255
	maxDepth        = 64
	// Decompressed bytes allowed per entry beyond file content: tar headers,
	// PAX records and padding.
	headerAllowance = 8 << 10
	// Characters rejected in names: path separators and syntax that Windows
	// treats specially (drive letters, alternate data streams, wildcards).
	forbiddenChars = `\:<>"|?*`
)

// UploadError explains why an upload was rejected. Its message is safe to
// show to the user: attacker-controlled names are quoted and truncated.
type UploadError struct {
	Path   string // archive entry, empty for archive-wide problems
	Reason string
}

func (e *UploadError) Error() string {
	if e.Path == "" {
		return "invalid upload: " + e.Reason
	}
	return fmt.Sprintf("invalid upload: %s: %s", display(e.Path), e.Reason)
}

// display quotes and truncates an attacker-controlled name for messages.
func display(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strconv.Quote(s)
}

func reject(p, format string, args ...any) error {
	return &UploadError{Path: p, Reason: fmt.Sprintf(format, args...)}
}

// NormalizeFile reads a gzip-compressed tar archive from src and, if it is
// safe, writes a normalised gzip-compressed tar to dstPath. The output
// contains only regular files, directories and symlinks, under the validated
// names, with mode 0644 or 0755 (0755 for directories), uid/gid 0, zero
// timestamps and no extended attributes or other metadata. Store and build
// from the output, never from the original upload.
//
// dstPath is only created when the upload is accepted: output goes to a
// temporary file in the same directory and is renamed into place on success,
// so a rejected or failed upload never leaves a partial archive behind.
//
// An unsafe archive yields an *UploadError; any other error is an I/O failure
// or ctx's error. Names must be printable ASCII and are compared
// case-insensitively, so the checks hold on case-insensitive filesystems such
// as NTFS and APFS.
func NormalizeFile(ctx context.Context, src io.Reader, dstPath string, lim Limits) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".upload-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = normalizeTo(ctx, tmp, src, lim); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp.Name(), dstPath); err != nil {
		return fmt.Errorf("move normalized archive into place: %w", err)
	}
	return nil
}

// normalizeTo does the work of NormalizeFile, writing to dst. On error dst
// holds partial output and must be discarded.
func normalizeTo(ctx context.Context, dst io.Writer, src io.Reader, lim Limits) error {
	counter := &countingReader{r: io.LimitReader(src, lim.MaxCompressedBytes+1)}
	out := &limitedWriter{w: dst, max: lim.MaxOutputBytes}
	err := normalize(ctx, out, bufio.NewReader(ctxReader{ctx: ctx, r: counter}), lim)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case counter.n > lim.MaxCompressedBytes:
		return reject("", "archive is larger than %d MB", lim.MaxCompressedBytes>>20)
	case errors.Is(out.err, errOutputTooLarge):
		return reject("", "archive is larger than %d MB once repacked", lim.MaxOutputBytes>>20)
	case out.err != nil:
		return fmt.Errorf("write normalized archive: %w", out.err)
	}
	return err
}

type node struct {
	name string
	typ  byte
	link string
}

// normalize reads from br, a bufio.Reader so that gzip does not read past the
// first member and trailing data can be detected.
func normalize(ctx context.Context, dst io.Writer, br *bufio.Reader, lim Limits) error {
	gz, err := gzip.NewReader(br)
	if err != nil {
		return reject("", "not a gzip-compressed tar archive")
	}
	gz.Multistream(false)

	maxDecompressed := lim.MaxUnpackedBytes + int64(lim.MaxEntries)*headerAllowance
	dec := &countingReader{r: io.LimitReader(gz, maxDecompressed+1)}
	errTooLarge := reject("", "archive unpacks to more than %d MB", lim.MaxUnpackedBytes>>20)
	readFailed := func(e error) error {
		if dec.n > maxDecompressed {
			return errTooLarge
		}
		return e
	}

	gzw, err := gzip.NewWriterLevel(dst, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	tw := tar.NewWriter(gzw)
	tr := tar.NewReader(dec)
	var (
		entries, files int
		unpacked       int64
		tree           = map[string]node{} // lower-cased path -> entry
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return readFailed(reject("", "corrupt tar archive"))
		}
		entries++
		if entries > lim.MaxEntries {
			return reject("", "archive has more than %d entries", lim.MaxEntries)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue // archive-wide metadata (e.g. from git archive); dropped
		}
		name, skip, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Size != 0 {
			return reject(hdr.Name, "only regular files may contain data")
		}
		if skip {
			if hdr.Typeflag != tar.TypeDir {
				return reject(hdr.Name, "the archive root must be a directory")
			}
			continue // the root directory itself ("." or "./")
		}
		key := strings.ToLower(name)
		if _, dup := tree[key]; dup {
			return reject(name, "duplicate path (paths are compared case-insensitively)")
		}

		h := tar.Header{Name: name, Typeflag: hdr.Typeflag}
		switch hdr.Typeflag {
		case tar.TypeReg:
			if hdr.Size > lim.MaxUnpackedBytes-unpacked {
				return errTooLarge
			}
			files++
			unpacked += hdr.Size
			h.Size, h.Mode = hdr.Size, 0o644
			if hdr.Mode&0o111 != 0 {
				h.Mode = 0o755
			}
			if err := tw.WriteHeader(&h); err != nil {
				return fmt.Errorf("write header: %w", err)
			}
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return readFailed(reject(name, "corrupt file data"))
			}
			tree[key] = node{name: name, typ: tar.TypeReg}
			continue
		case tar.TypeDir:
			h.Name, h.Mode = name+"/", 0o755
		case tar.TypeSymlink:
			if err := checkLinkTarget(name, hdr.Linkname); err != nil {
				return err
			}
			h.Linkname, h.Mode = hdr.Linkname, 0o777
		case tar.TypeLink:
			return reject(name, "hard links are not allowed")
		default:
			return reject(name, "unsupported entry type (only files, directories and symlinks are allowed)")
		}
		if err := tw.WriteHeader(&h); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
		tree[key] = node{name: name, typ: hdr.Typeflag, link: hdr.Linkname}
	}

	if files == 0 {
		return reject("", "archive contains no files")
	}
	if err := checkTrailer(dec, br); err != nil {
		return readFailed(err)
	}
	if err := checkTree(tree); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return nil
}

// checkTrailer rejects anything after the tar end-of-archive marker other
// than zero padding, and any data after the first gzip member. Other tools
// may extract such data, so it must not slip past validation.
func checkTrailer(dec io.Reader, raw *bufio.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := dec.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return reject("", "unexpected data after the end of the tar archive")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return reject("", "corrupt gzip data")
		}
	}
	switch _, err := raw.ReadByte(); {
	case err == nil:
		return reject("", "unexpected data after the archive (only one gzip stream is allowed)")
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("read upload: %w", err)
	}
	return nil
}

// cleanName normalises a tar entry name to a slash-separated path relative to
// the archive root. skip is true for the root directory entry.
func cleanName(raw string) (name string, skip bool, err error) {
	if raw == "" {
		return "", false, reject(raw, "empty path")
	}
	if p := pathProblem(raw); p != "" {
		return "", false, reject(raw, "%s", p)
	}
	if strings.HasPrefix(raw, "/") {
		return "", false, reject(raw, "absolute paths are not allowed")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", false, reject(raw, "path contains '..'")
		}
	}
	name = path.Clean(raw)
	if name == "." {
		return "", true, nil
	}
	if !fs.ValidPath(name) {
		return "", false, reject(raw, "invalid path")
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxDepth {
		return "", false, reject(raw, "path is nested more than %d levels deep", maxDepth)
	}
	for _, part := range parts {
		if p := componentProblem(part); p != "" {
			return "", false, reject(raw, "%s", p)
		}
	}
	return name, false, nil
}

// checkLinkTarget rejects symlink targets that are absolute, malformed, or
// point outside the root when resolved lexically from the link's directory.
func checkLinkTarget(name, target string) error {
	if target == "" {
		return reject(name, "symlink has an empty target")
	}
	if p := pathProblem(target); p != "" {
		return reject(name, "symlink target: %s", p)
	}
	if strings.HasPrefix(target, "/") {
		return reject(name, "symlink target is absolute")
	}
	parts := strings.Split(target, "/")
	if len(parts) > maxDepth {
		return reject(name, "symlink target is nested more than %d levels deep", maxDepth)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		if p := componentProblem(part); p != "" {
			return reject(name, "symlink target: %s", p)
		}
	}
	if !fs.ValidPath(path.Join(path.Dir(name), target)) {
		return reject(name, "symlink points outside the upload")
	}
	return nil
}

func pathProblem(s string) string {
	if len(s) > maxPathLen {
		return fmt.Sprintf("path is longer than %d bytes", maxPathLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return "names must contain only printable ASCII characters"
		}
		if strings.IndexByte(forbiddenChars, c) >= 0 {
			return fmt.Sprintf("names may not contain any of %s", forbiddenChars)
		}
	}
	return ""
}

var reservedNames = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for i := '0'; i <= '9'; i++ {
		m["COM"+string(i)], m["LPT"+string(i)] = true, true
	}
	return m
}()

// componentProblem checks one path component against Windows naming rules,
// because names Windows treats as equal or special could bypass the checks
// when sources are unpacked on a Windows host.
func componentProblem(c string) string {
	switch {
	case len(c) > maxComponentLen:
		return fmt.Sprintf("a name is longer than %d bytes", maxComponentLen)
	case strings.HasSuffix(c, ".") || strings.HasSuffix(c, " "):
		return "names may not end with a dot or a space"
	}
	base, _, _ := strings.Cut(c, ".")
	if reservedNames[strings.ToUpper(strings.TrimRight(base, " "))] {
		return "reserved device name (CON, NUL, COM1, ...)"
	}
	if i := strings.IndexByte(c, '~'); i >= 0 && i+1 < len(c) && c[i+1] >= '0' && c[i+1] <= '9' {
		return "names like NAME~1 are not allowed (Windows short-name alias)"
	}
	return ""
}

// checkTree runs once the whole archive is known. Lexical symlink checks are
// only sound if no path goes through another symlink: with "a -> ." the link
// "a/b -> ../x" looks contained but really escapes. So:
//   - no entry may live under a symlink or under a regular file, and
//   - a symlink target may not pass through another symlink.
func checkTree(tree map[string]node) error {
	for key, n := range tree {
		for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
			switch parent, ok := tree[dir]; {
			case !ok:
			case parent.typ == tar.TypeSymlink:
				return reject(n.name, "path goes through symlink %s", display(parent.name))
			case parent.typ == tar.TypeReg:
				return reject(n.name, "parent %s is a file, not a directory", display(parent.name))
			}
		}
		if n.typ != tar.TypeSymlink {
			continue
		}
		cur := path.Dir(key)
		parts := strings.Split(strings.ToLower(n.link), "/")
		for i, part := range parts {
			switch part {
			case "", ".":
				continue
			case "..":
				if cur == "." {
					return reject(n.name, "symlink points outside the upload")
				}
				cur = path.Dir(cur)
				continue
			}
			cur = path.Join(cur, part)
			if hop, ok := tree[cur]; ok && hop.typ == tar.TypeSymlink && i < len(parts)-1 {
				return reject(n.name, "symlink target goes through another symlink %s", display(hop.name))
			}
		}
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

var errOutputTooLarge = errors.New("normalized archive exceeds the output limit")

// limitedWriter caps the output size and remembers the first write error, so
// output failures are reported as I/O errors, not as problems with the upload.
type limitedWriter struct {
	w      io.Writer
	max, n int64
	err    error
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.n+int64(len(p)) > l.max {
		l.err = errOutputTooLarge
		return 0, l.err
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	if err != nil {
		l.err = err
	}
	return n, err
}

// ctxReader stops reading the upload once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
