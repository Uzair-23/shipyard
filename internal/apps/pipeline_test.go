package apps

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestSourceRelPath(t *testing.T) {
	var app pgtype.UUID
	if err := app.Scan("0b5f0b4e-6f1a-4c8e-9d7a-2f3e4d5c6b7a"); err != nil {
		t.Fatal(err)
	}
	hex := strings.Repeat("ab", 32)
	got, err := SourceRelPath(app, "sha256:"+hex)
	if err != nil {
		t.Fatalf("SourceRelPath: %v", err)
	}
	want := filepath.Join("sources", "0b5f0b4e-6f1a-4c8e-9d7a-2f3e4d5c6b7a", hex+".tar.gz")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	for _, ref := range []string{
		"", "sha256:", "sha256:" + hex[:63], "sha256:" + strings.ToUpper(hex),
		"sha256:../../etc/passwd", "md5:" + hex, "sha256:" + hex + "/../x", "../" + hex,
	} {
		if _, err := SourceRelPath(app, ref); err == nil {
			t.Errorf("SourceRelPath accepted %q", ref)
		}
	}
	if _, err := SourceRelPath(pgtype.UUID{}, "sha256:"+hex); err == nil {
		t.Error("SourceRelPath accepted an invalid app id")
	}
}
