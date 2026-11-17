package build

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// zipMagic starts every non-empty zip file (an empty one starts with PK\5\6).
var zipMagic = []byte("PK\x03\x04")

func isZip(br *bufio.Reader) bool {
	head, _ := br.Peek(4)
	return bytes.Equal(head, zipMagic) || bytes.Equal(head, []byte("PK\x05\x06"))
}

// normalizeZip turns an uploaded zip into a gzip-compressed tar and runs that
// through the same validation and normalisation as an uploaded .tar.gz, so
// every rule (names, symlinks, limits) is enforced in one place and the output
// is the same format. The zip is only parsed here; nothing from it is stored.
//
// Differences from tar that matter: a zip needs random access, so it is
// spooled to a temporary file first (bounded by MaxCompressedBytes); sizes in
// zip headers can lie, so data is counted as it is read; Windows tools write
// "\" separators, which are converted to "/" before validation; macOS adds
// __MACOSX/ entries, which are dropped; and a folder zipped through the
// file manager has one wrapper folder around everything, which is removed.
func normalizeZip(ctx context.Context, dst io.Writer, br *bufio.Reader, lim Limits) error {
	spool, err := os.CreateTemp("", "shipyard-zip-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	n, err := io.Copy(spool, io.LimitReader(ctxReader{ctx: ctx, r: br}, lim.MaxCompressedBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read upload: %w", err)
	}
	if n > lim.MaxCompressedBytes {
		return reject("", "archive is larger than %d MB", lim.MaxCompressedBytes>>20)
	}

	zr, err := zip.NewReader(spool, n)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) { // insecure names are rejected by the checks below
		return reject("", "not a valid zip file")
	}

	conv, err := os.CreateTemp("", "shipyard-zip-conv-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = conv.Close(); _ = os.Remove(conv.Name()) }()
	if err := zipToTarGz(ctx, conv, zr, lim); err != nil {
		return err
	}
	if _, err := conv.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp file: %w", err)
	}
	return normalize(ctx, dst, bufio.NewReader(conv), lim)
}

const maxLinkTarget = 1024

// zipToTarGz writes zr as a gzip-compressed tar. It does only what is needed
// to represent the entries; normalize does the actual validation afterwards.
func zipToTarGz(ctx context.Context, dst io.Writer, zr *zip.Reader, lim Limits) error {
	if len(zr.File) > lim.MaxEntries {
		return reject("", "archive has more than %d entries", lim.MaxEntries)
	}
	gzw, err := gzip.NewWriterLevel(dst, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	tw := tar.NewWriter(gzw)
	errTooLarge := reject("", "archive unpacks to more than %d MB", lim.MaxUnpackedBytes>>20)

	wrapper := wrapperFolder(zr.File)
	var unpacked int64
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := zipName(f)
		if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		if wrapper != "" {
			if strings.TrimSuffix(name, "/") == wrapper {
				continue
			}
			name = strings.TrimPrefix(name, wrapper+"/")
		}
		if f.Flags&0x1 != 0 {
			return reject(name, "encrypted zip entries are not supported")
		}
		mode := f.Mode()
		hdr := tar.Header{Name: name, Mode: int64(mode.Perm())}
		var body io.ReadCloser
		var size int64
		switch {
		case mode.IsDir() || strings.HasSuffix(name, "/"):
			hdr.Typeflag = tar.TypeDir
		case mode&fs.ModeSymlink != 0:
			hdr.Typeflag = tar.TypeSymlink
			if f.UncompressedSize64 > maxLinkTarget {
				return reject(name, "symlink target is too long")
			}
		case mode.IsRegular():
			hdr.Typeflag = tar.TypeReg
			if f.UncompressedSize64 > uint64(lim.MaxUnpackedBytes-unpacked) { //nolint:gosec // unpacked <= MaxUnpackedBytes, so the difference is never negative
				return errTooLarge
			}
		default:
			return reject(name, "unsupported entry type (only files, directories and symlinks are allowed)")
		}
		if hdr.Typeflag != tar.TypeDir {
			size = int64(f.UncompressedSize64) //nolint:gosec // symlinks <= 1024 and files <= MaxUnpackedBytes were checked above
			rc, err := f.Open()
			if err != nil {
				if errors.Is(err, zip.ErrAlgorithm) {
					return reject(name, "unsupported zip compression method")
				}
				return reject(name, "corrupt zip entry")
			}
			body = rc
		}
		if hdr.Typeflag == tar.TypeSymlink {
			target, err := io.ReadAll(io.LimitReader(body, maxLinkTarget+1))
			_ = body.Close()
			if err != nil || len(target) > maxLinkTarget {
				return reject(name, "corrupt symlink entry")
			}
			hdr.Linkname = string(target)
			body = nil
		} else if body != nil {
			hdr.Size = size
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			return reject(name, "cannot be represented safely")
		}
		if body == nil {
			continue
		}
		// Count what is really inside, not what the header claims.
		copied, err := io.Copy(tw, io.LimitReader(body, size+1))
		_ = body.Close()
		if err != nil || copied != size {
			return reject(name, "corrupt zip entry (size or checksum does not match)")
		}
		unpacked += copied
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return nil
}

// zipName converts Windows separators so the name checks see what any
// extractor on Windows would see.
func zipName(f *zip.File) string {
	return strings.ReplaceAll(f.Name, `\`, "/")
}

// wrapperFolder returns the name of the single top-level folder that holds
// every entry (what "compress folder" produces), or "" if the archive has
// files or several folders at the top level.
func wrapperFolder(files []*zip.File) string {
	top := ""
	for _, f := range files {
		name := zipName(f)
		if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		first, _, hasSlash := strings.Cut(name, "/")
		if first == "" || first == "." || first == ".." || pathProblem(first) != "" || componentProblem(first) != "" {
			return "" // leave odd names alone so the checks reject them
		}
		if !hasSlash && !f.Mode().IsDir() {
			return "" // a file at the top level
		}
		if top != "" && top != first {
			return ""
		}
		top = first
	}
	return top
}
