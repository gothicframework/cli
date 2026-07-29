package output

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestPrintln_DistinctMessages(t *testing.T) {
	ResetDedup()

	out := captureStdout(t, func() {
		Println("hello")
		Println("world")
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
}

func TestPrintln_TwoIdentical(t *testing.T) {
	ResetDedup()

	out := captureStdout(t, func() {
		Println("build %s", "app")
		Println("build %s", "app")
	})
	if !strings.Contains(out, "(x2)") {
		t.Errorf("expected (x2) suffix, got: %s", out)
	}
}

func TestPrintln_ThreeIdentical(t *testing.T) {
	ResetDedup()

	out := captureStdout(t, func() {
		Println("building")
		Println("building")
		Println("building")
	})
	if !strings.Contains(out, "(x3)") {
		t.Errorf("expected (x3) suffix, got: %s", out)
	}
}

func TestPrintln_IdenticalThenDifferentThenIdentical(t *testing.T) {
	ResetDedup()

	out := captureStdout(t, func() {
		Println("same")
		Println("same")
		Println("different")
		Println("same")
	})
	// Should have: "same", "same (x2)", "different", "same" (reset)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %s", len(lines), out)
	}
}

func TestPrint_KeyIgnoresDecoration(t *testing.T) {
	ResetDedup()

	// Same key, different rendered line: still counts as a repeat.
	out := captureStdout(t, func() {
		Print("build app", "12:00:00 build app")
		Print("build app", "12:00:09 build app")
	})
	if !strings.Contains(out, "(x2)") {
		t.Errorf("same key should dedup regardless of the rendered line, got: %s", out)
	}
}

func TestPrintRaw_BreaksTheRun(t *testing.T) {
	ResetDedup()

	out := captureStdout(t, func() {
		Println("same")
		Println("same")
		PrintRaw("an unrelated line")
		Println("same")
	})
	if strings.Count(out, "(x") != 1 {
		t.Errorf("expected exactly one (xN) suffix, got: %s", out)
	}
	if strings.Contains(out, "(x3)") {
		t.Errorf("PrintRaw must reset the run, not carry it: %s", out)
	}
}

func TestAccent_RestoresTheSurroundingColour(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()

	got := Accent("src/pages/index.templ")
	if !strings.HasPrefix(got, termcolor.Yellow) {
		t.Errorf("accent should open in the value colour: %q", got)
	}
	// Without the trailing dim, everything after the accent would fall back to
	// the terminal default instead of the line's own colour.
	if !strings.HasSuffix(got, termcolor.Gray) {
		t.Errorf("accent should resume the dim line colour: %q", got)
	}
}

func TestAccent_PlainWhenColourIsOff(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = false
	defer func() { termcolor.Enabled = orig }()

	if got := Accent("src/x.templ"); got != "src/x.templ" {
		t.Errorf("no escapes may appear with colour off, got %q", got)
	}
}

func TestErrorln_IsRedAndNotDeduped(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()
	ResetDedup()

	out := captureStdout(t, func() {
		Errorln("boom")
		Errorln("boom")
	})
	if !strings.Contains(out, termcolor.Red) {
		t.Errorf("a failure line must be red: %q", out)
	}
	// Two identical failures are two events; collapsing them hides one.
	if strings.Contains(out, "(x2)") {
		t.Errorf("errors must not be deduped: %q", out)
	}
	if n := strings.Count(out, "boom"); n != 2 {
		t.Errorf("expected both failures printed, got %d", n)
	}
}

func TestLink_IsBlueAndDistinctFromValue(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()

	link := Link("http://localhost:3000")
	if !strings.HasPrefix(link, termcolor.Blue) {
		t.Errorf("an address should open in the link colour: %q", link)
	}
	if !strings.HasSuffix(link, termcolor.Gray) {
		t.Errorf("link should resume the dim line colour: %q", link)
	}
	// A location and a value must not look alike; that is the whole reason Link
	// exists next to Accent.
	if strings.HasPrefix(Accent("1.043s"), termcolor.Blue) {
		t.Error("Accent must not use the link colour")
	}
}

func TestLink_PlainWhenColourIsOff(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = false
	defer func() { termcolor.Enabled = orig }()

	if got := Link("http://localhost:3000"); got != "http://localhost:3000" {
		t.Errorf("no escapes may appear with colour off, got %q", got)
	}
}

func TestTag_IsTheBrandAccentWithoutBold(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()

	got := Tag("Proxy")
	if !strings.HasPrefix(got, termcolor.Purple) {
		t.Errorf("a subsystem tag should open in the brand accent: %q", got)
	}
	// Plain, not bold: bold on a saturated magenta reads as shouting.
	if strings.Contains(got, termcolor.Bold) {
		t.Errorf("a subsystem tag must not be bold: %q", got)
	}
	if !strings.HasSuffix(got, termcolor.Gray) {
		t.Errorf("tag should resume the dim line colour: %q", got)
	}
	// A tag marks where a line comes from; a link marks where something lives.
	// They must not render alike.
	if Tag("Proxy") == Link("Proxy") {
		t.Error("tag and link must render differently")
	}
}

func TestTag_PlainWhenColourIsOff(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = false
	defer func() { termcolor.Enabled = orig }()

	if got := Tag("Proxy"); got != "Proxy" {
		t.Errorf("no escapes may appear with colour off, got %q", got)
	}
}
