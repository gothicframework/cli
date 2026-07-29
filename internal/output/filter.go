package output

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
)

// ansiSequence matches the CSI escapes a colourising producer emits. A filter
// that inspects line text has to strip these first: a line whose visible text
// starts with "(" can start with an escape byte on the wire.
var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI returns s without its ANSI escape sequences.
func StripANSI(s string) string {
	return ansiSequence.ReplaceAllString(s, "")
}

// LineFilter wraps a writer and drops whole lines for which drop reports true.
// Writes arrive in arbitrary chunks, so partial lines are buffered until their
// newline shows up and the decision can be made on the complete line.
type LineFilter struct {
	w    io.Writer
	drop func(string) bool

	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLineFilter returns a writer that forwards to w every line drop rejects.
func NewLineFilter(w io.Writer, drop func(string) bool) *LineFilter {
	return &LineFilter{w: w, drop: drop}
}

// Write buffers p and forwards the complete lines that survive the filter. It
// always reports len(p) consumed: a dropped line is still fully handled, and
// reporting a short write would look like an I/O error to the producer.
func (f *LineFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.buf.Write(p)
	for {
		line, err := f.buf.ReadString('\n')
		if err != nil {
			// No newline yet: keep the partial line for the next Write.
			f.buf.Reset()
			f.buf.WriteString(line)
			return len(p), nil
		}
		if f.drop(strings.TrimRight(line, "\r\n")) {
			continue
		}
		if _, werr := io.WriteString(f.w, line); werr != nil {
			return len(p), werr
		}
	}
}
