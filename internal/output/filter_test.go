package output

import (
	"bytes"
	"testing"
)

func dropHello(line string) bool { return line == "hello" }

func TestLineFilter_DropsMatchingLines(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, dropHello)

	if _, err := f.Write([]byte("hello\nworld\nhello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := sink.String(); got != "world\n" {
		t.Errorf("expected only the surviving line, got %q", got)
	}
}

func TestLineFilter_ReassemblesSplitWrites(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, dropHello)

	// The same line arriving in three chunks must still be recognised.
	f.Write([]byte("hel"))
	f.Write([]byte("lo"))
	f.Write([]byte("\nkeep\n"))

	if got := sink.String(); got != "keep\n" {
		t.Errorf("split line should still be dropped, got %q", got)
	}
}

func TestLineFilter_HoldsPartialLine(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, dropHello)

	f.Write([]byte("no newline yet"))
	if sink.Len() != 0 {
		t.Errorf("partial line must not be forwarded, got %q", sink.String())
	}
	f.Write([]byte("\n"))
	if got := sink.String(); got != "no newline yet\n" {
		t.Errorf("line should flush once terminated, got %q", got)
	}
}

func TestLineFilter_ReportsFullWriteLength(t *testing.T) {
	var sink bytes.Buffer
	f := NewLineFilter(&sink, dropHello)

	p := []byte("hello\n")
	n, err := f.Write(p)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// A dropped line is still fully consumed; a short count reads as an error.
	if n != len(p) {
		t.Errorf("expected %d bytes consumed, got %d", len(p), n)
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[32m(✓)\x1b[0m Complete", "(✓) Complete"},
		{"\x1b[38;5;244m17:25:41\x1b[0m Build app", "17:25:41 Build app"},
		{"plain text", "plain text"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripANSI(c.in); got != c.want {
			t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
