package output

import (
	"bytes"
	"errors"
	"testing"
)

// errWriter fails every write with errWrite. failAfter lets a test let the
// first N writes through so the failure lands on a chosen line.
type errWriter struct {
	failAfter int
	writes    int
	written   bytes.Buffer
}

var errWrite = errors.New("write failed")

func (e *errWriter) Write(p []byte) (int, error) {
	e.writes++
	if e.writes > e.failAfter {
		return 0, errWrite
	}
	return e.written.Write(p)
}

// LineFilter forwards surviving lines with io.WriteString; the error it returns
// had no coverage, so a mutation that inverted the check went unnoticed.
func TestLineFilterReturnsWriteError(t *testing.T) {
	w := &errWriter{failAfter: 0}
	f := NewLineFilter(w, func(string) bool { return false })

	n, err := f.Write([]byte("first\n"))
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want %v", err, errWrite)
	}
	if n != len("first\n") {
		t.Errorf("n = %d, want %d — a failed write still reports the input consumed", n, len("first\n"))
	}
}

// The error must surface on the line that actually fails, not the first one.
func TestLineFilterReturnsWriteErrorOnLaterLine(t *testing.T) {
	w := &errWriter{failAfter: 1}
	f := NewLineFilter(w, func(string) bool { return false })

	_, err := f.Write([]byte("one\ntwo\nthree\n"))
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want %v", err, errWrite)
	}
	if got := w.written.String(); got != "one\n" {
		t.Errorf("forwarded %q, want %q — the filter must stop at the failing line", got, "one\n")
	}
}

// The success path must keep going: every surviving line in a multi-line write
// reaches the writer, and no error is reported.
func TestLineFilterForwardsAllLinesWhenWritesSucceed(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, func(string) bool { return false })

	n, err := f.Write([]byte("one\ntwo\nthree\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len("one\ntwo\nthree\n") {
		t.Errorf("n = %d, want %d", n, len("one\ntwo\nthree\n"))
	}
	if got := sink.String(); got != "one\ntwo\nthree\n" {
		t.Errorf("forwarded %q, want all three lines", got)
	}
}

// A dropped line must not consume the write error path, and the lines after it
// must still be forwarded.
func TestLineFilterKeepsWritingAfterDroppedLine(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, func(line string) bool { return line == "skip" })

	if _, err := f.Write([]byte("keep\nskip\nkeep2\n")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sink.String(); got != "keep\nkeep2\n" {
		t.Errorf("forwarded %q, want %q", got, "keep\nkeep2\n")
	}
}
