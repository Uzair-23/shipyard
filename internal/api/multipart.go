package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
)

var (
	errNoSource = errors.New(`no "source" part`)
	errBadForm  = errors.New("malformed multipart body")
)

// multipartPartReader streams a multipart body without buffering it in
// memory or in temp files (unlike http.Request.ParseMultipartForm).
type multipartPartReader struct{ mr *multipart.Reader }

func newPartReader(body io.Reader, boundary string) *multipartPartReader {
	return &multipartPartReader{mr: multipart.NewReader(body, boundary)}
}

// source returns the file part named "source". Other parts are skipped.
func (p *multipartPartReader) source() (io.Reader, error) {
	for {
		part, err := p.mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errNoSource
		}
		if err != nil {
			// Body-size and I/O errors must keep their type; only framing errors are "bad form".
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, err
			}
			return nil, errBadForm
		}
		if part.FormName() == "source" && part.FileName() != "" {
			return part, nil
		}
	}
}
