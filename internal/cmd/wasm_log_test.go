package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

func withTermColor(t *testing.T, fn func()) {
	t.Helper()
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()
	fn()
}

func TestWasmTimestampFormat(t *testing.T) {
	withTermColor(t, func() {
		ts := wasmTimestamp()
		// Strip ANSI wrappers; the core is wall-clock only, no date.
		core := strings.TrimPrefix(ts, ansiGray())
		core = strings.TrimSuffix(core, ansiReset())
		matched, err := regexp.MatchString(`^\d{2}:\d{2}:\d{2}$`, core)
		if err != nil {
			t.Fatalf("regexp: %v", err)
		}
		if !matched {
			t.Errorf("wasmTimestamp core %q does not match timestamp layout", core)
		}
		// Dim, so it recedes behind the message it prefixes.
		if !strings.HasPrefix(ts, ansiGray()) || !strings.HasSuffix(ts, ansiReset()) {
			t.Errorf("wasmTimestamp %q missing ANSI wrappers", ts)
		}
	})
}

func TestWasmCount(t *testing.T) {
	withTermColor(t, func() {
		got := wasmCount(3, "page(s)")
		if !strings.Contains(got, "3") {
			t.Errorf("wasmCount missing number: %q", got)
		}
		if !strings.Contains(got, "page(s)") {
			t.Errorf("wasmCount missing label: %q", got)
		}
		// Ends in the message colour so surrounding wasmLogf text is unaffected.
		if !strings.HasSuffix(got, "page(s)") {
			t.Errorf("wasmCount %q should end with the label", got)
		}
		if !strings.Contains(got, ansiLightGreen()) || !strings.Contains(got, ansiWhite()) {
			t.Errorf("wasmCount %q missing expected color codes", got)
		}
	})
}

func TestWasmCountZero(t *testing.T) {
	got := wasmCount(0, "topic manager(s)")
	if !strings.Contains(got, "0") {
		t.Errorf("wasmCount(0) missing zero: %q", got)
	}
}

func TestWasmLogf(t *testing.T) {
	withTermColor(t, func() {
		out := captureStdout(t, func() {
			wasmLogf("building %s", wasmCount(2, "page(s)"))
		})
		if !strings.Contains(out, "building") {
			t.Errorf("wasmLogf output missing message: %q", out)
		}
		if !strings.Contains(out, "WASM") {
			t.Errorf("wasmLogf output missing WASM tag: %q", out)
		}
		if !strings.Contains(out, "2") || !strings.Contains(out, "page(s)") {
			t.Errorf("wasmLogf output missing formatted args: %q", out)
		}
		if !strings.HasSuffix(out, "\n") {
			t.Errorf("wasmLogf output should end with newline: %q", out)
		}
	})
}

func TestWasmErrorf(t *testing.T) {
	withTermColor(t, func() {
		out := captureStdout(t, func() {
			wasmErrorf("scan failed: %v", "boom")
		})
		if !strings.Contains(out, "scan failed: boom") {
			t.Errorf("wasmErrorf output missing message: %q", out)
		}
		if !strings.Contains(out, "WASM") {
			t.Errorf("wasmErrorf output missing WASM tag: %q", out)
		}
		if !strings.Contains(out, ansiRed()) {
			t.Errorf("wasmErrorf output missing red color: %q", out)
		}
	})
}

func TestWasmTagConstant(t *testing.T) {
	if !strings.Contains(wasmTag(), "WASM") {
		t.Errorf("wasmTag %q should contain WASM", wasmTag())
	}
}
