package build

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"time"
)

// maxInspectedFileBytes bounds how much of the small top-level files the
// build plan reads (shipyard.toml, requirements.txt, pyproject.toml).
const maxInspectedFileBytes = 64 << 10

// ReadSource scans a normalised archive (NormalizeFile output) and returns the
// paths and the few top-level files the build plan needs. config is the raw
// shipyard.toml, or nil if absent. Files larger than 64 KB are returned
// truncated to 64 KB + 1 byte, so ParseConfig rejects an oversized
// shipyard.toml and the start heuristics ignore oversized dependency files.
func ReadSource(r io.Reader) (src Source, config []byte, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Source{}, nil, fmt.Errorf("open source archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return src, config, nil
		}
		if err != nil {
			return Source{}, nil, fmt.Errorf("read source archive: %w", err)
		}
		src.Paths = append(src.Paths, hdr.Name)
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		var dst *[]byte
		switch hdr.Name {
		case ConfigFile:
			dst = &config
		case "requirements.txt":
			dst = &src.Requirements
		case "pyproject.toml":
			dst = &src.PyProject
		default:
			continue
		}
		if *dst, err = io.ReadAll(io.LimitReader(tr, maxInspectedFileBytes+1)); err != nil {
			return Source{}, nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
	}
}

// WriteContext writes an uncompressed tar build context to w: the generated
// Dockerfile at the root and every entry of the normalised archive under
// SourceDir/, so nothing the user uploaded can replace the Dockerfile or a
// root .dockerignore (ADR 004). archive must be NormalizeFile output (the
// pipeline verifies its hash first). As a cheap extra guard, entry names,
// types and symlink targets are re-checked; the whole-archive checks of
// Normalize (paths through symlinks, duplicates, limits) are not repeated.
func WriteContext(w io.Writer, archive io.Reader, dockerfile []byte) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open source archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tw := tar.NewWriter(w)
	write := func(h *tar.Header, body io.Reader) error {
		h.ModTime = time.Unix(0, 0)
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("write context entry %q: %w", h.Name, err)
		}
		if body != nil {
			if _, err := io.CopyN(tw, body, h.Size); err != nil {
				return fmt.Errorf("copy context entry %q: %w", h.Name, err)
			}
		}
		return nil
	}
	if err := write(&tar.Header{Name: "Dockerfile", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(dockerfile))},
		bytesReader(dockerfile)); err != nil {
		return err
	}
	if err := write(&tar.Header{Name: SourceDir + "/", Typeflag: tar.TypeDir, Mode: 0o755}, nil); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		name, skip, err := cleanName(hdr.Name)
		if err != nil {
			return fmt.Errorf("source archive is not normalised: %w", err)
		}
		if skip {
			continue
		}
		h := &tar.Header{Name: SourceDir + "/" + name, Typeflag: hdr.Typeflag, Mode: hdr.Mode & 0o755}
		switch hdr.Typeflag {
		case tar.TypeReg:
			h.Size = hdr.Size
			err = write(h, tr)
		case tar.TypeDir:
			h.Name += "/"
			err = write(h, nil)
		case tar.TypeSymlink:
			// Targets are relative and were validated against the upload
			// root, which is now SourceDir, so they stay inside it.
			if err := checkLinkTarget(name, hdr.Linkname); err != nil {
				return fmt.Errorf("source archive is not normalised: %w", err)
			}
			h.Linkname, h.Mode = hdr.Linkname, 0o777
			err = write(h, nil)
		default:
			return fmt.Errorf("source archive is not normalised: entry %q has type %q", hdr.Name, hdr.Typeflag)
		}
		if err != nil {
			return err
		}
	}
	return tw.Close()
}
