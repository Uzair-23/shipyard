package build

import (
	"reflect"
	"strings"
	"testing"
)

func split(chunks ...string) []string {
	var got []string
	l := &lineSplitter{emit: func(s string) { got = append(got, s) }}
	for _, c := range chunks {
		l.Write(c)
	}
	l.Close()
	return got
}

func TestLineSplitter(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"whole lines", []string{"a\nb\n"}, []string{"a", "b"}},
		{"line split across chunks", []string{"Step 1/", "9 : FROM x\nnext"}, []string{"Step 1/9 : FROM x", "next"}},
		{"unfinished last line is kept", []string{"no newline"}, []string{"no newline"}},
		{"empty and blank lines dropped", []string{"\n\n  \n a \n"}, []string{"a"}},
		{"CRLF", []string{"one\r\ntwo\r\n"}, []string{"one", "two"}},
		{"ANSI escape and control characters removed", []string{"\x1b[31mred\x1b[0m \x07bell\x00nul\n"}, []string{"[31mred[0m bellnul"}},
		{"invalid UTF-8 removed", []string{"ok\xff\xfe!\n"}, []string{"ok!"}},
		{"bidi override removed", []string{"abc\u202edef\n"}, []string{"abcdef"}},
		{"tabs kept", []string{"a\tb\n"}, []string{"a\tb"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := split(tt.chunks...); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLineSplitterBoundsLineLength(t *testing.T) {
	got := split(strings.Repeat("x", 5000) + "\nshort\n")
	if len(got) != 2 || len([]rune(got[0])) != MaxLogLine+1 || !strings.HasSuffix(got[0], "…") || got[1] != "short" {
		t.Fatalf("long line: %d lines, first has %d runes", len(got), len([]rune(got[0])))
	}
	// A stream with no newline at all must not grow without bound.
	var emitted int
	l := &lineSplitter{emit: func(string) { emitted++ }}
	for range 100 {
		l.Write(strings.Repeat("y", 4096))
	}
	if emitted == 0 {
		t.Fatal("a flood without newlines was never flushed")
	}
	if l.partial.Len() > maxPartial {
		t.Fatalf("buffered %d bytes", l.partial.Len())
	}
}
