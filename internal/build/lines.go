package build

import "strings"

const (
	// MaxLogLine is the longest line kept; longer ones are cut.
	MaxLogLine = 2000
	// maxPartial bounds a line that never ends, so a flood without newlines cannot grow memory.
	maxPartial = 8 << 10
)

// lineSplitter turns a stream of build output into sanitised lines: valid
// UTF-8, no control characters, no empty lines, at most MaxLogLine characters.
type lineSplitter struct {
	emit    func(string)
	partial strings.Builder
}

// Write feeds more output; every completed line is emitted.
func (l *lineSplitter) Write(s string) {
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			l.partial.WriteString(s)
			if l.partial.Len() > maxPartial {
				l.flushPartial()
			}
			return
		}
		l.partial.WriteString(s[:i])
		l.flushPartial()
		s = s[i+1:]
	}
}

// Close emits an unfinished last line.
func (l *lineSplitter) Close() { l.flushPartial() }

func (l *lineSplitter) flushPartial() {
	line := Sanitize(l.partial.String())
	l.partial.Reset()
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if r := []rune(line); len(r) > MaxLogLine {
		line = string(r[:MaxLogLine]) + "…"
	}
	l.emit(line)
}
