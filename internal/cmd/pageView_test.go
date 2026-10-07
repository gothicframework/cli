package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// captureStdout pipes os.Stdout around fn and returns what was printed.
func capturePageViewStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return buf.String(), runErr
}

// resetPageViewFlags clears the package-level flags so tests never see each
// other's leftovers.
func resetPageViewFlags(t *testing.T) {
	t.Helper()
	pageViewPNG, pageViewManifest, pageViewEngine, pageViewGrid = "", "", "", "48x24"
	pageViewWidth = 0
	t.Cleanup(func() {
		pageViewPNG, pageViewManifest, pageViewEngine, pageViewGrid = "", "", "", "48x24"
		pageViewWidth = 0
	})
}

// runPageView executes the real page-view command under a fresh root, the same
// seam the other command tests use, returning its stdout and error.
func runPageView(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "gothic"}
	root.AddCommand(pageViewCmd)
	root.SetOut(&bytes.Buffer{}) // silence usage/help from the expected-error tests
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"page-view"}, args...))
	t.Cleanup(func() { root.SetArgs(nil) })

	return capturePageViewStdout(t, root.Execute)
}

// fixturePath points at the pagemap package's committed fixtures; the command
// must be able to encode exactly those bytes.
func fixturePath(t *testing.T, name, ext string) string {
	t.Helper()
	return ".." + string(os.PathSeparator) + "pagemap" + string(os.PathSeparator) + "testdata" + string(os.PathSeparator) + "pageview" + string(os.PathSeparator) + name + ext
}

// TestPageViewCommand_EncodesFixturePNG: the `--png`/`--manifest` path runs
// the encoder end to end and prints the payload verbatim.
func TestPageViewCommand_EncodesFixturePNG(t *testing.T) {
	resetPageViewFlags(t)
	pageViewPNG = fixturePath(t, "dark", ".png")
	pageViewManifest = fixturePath(t, "dark", ".manifest.json")
	pageViewEngine = "chromium 131"

	out, err := runPageView(t, "http://localhost:3000/docs/routing", "--png", pageViewPNG, "--manifest", pageViewManifest, "--engine", pageViewEngine)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(out, "=== GOTHIC PAGE VIEW ===") {
		t.Errorf("payload header missing:\n%s", out)
	}
	if !strings.Contains(out, "URL       http://localhost:3000/docs/routing") {
		t.Errorf("URL line missing:\n%s", out)
	}
	if !strings.Contains(out, "GRID      48x24") {
		t.Errorf("GRID line missing:\n%s", out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("the dark fixture has no alerts, payload should say so:\n%s", out)
	}
}

func TestPageViewCommand_LiveCaptureRefusedWithoutServer(t *testing.T) {
	// Without --png the command renders the URL live in the managed browser
	// (loopback only). With no dev server listening, the error names the
	// navigation failure — never a hang and never a silent payload.
	resetPageViewFlags(t)
	_, err := runPageView(t, "http://localhost:3000/")
	if err == nil {
		t.Fatal("expected an error capturing a page nobody serves")
	}
	if !strings.Contains(err.Error(), "live capture") || !strings.Contains(err.Error(), "localhost:3000") {
		t.Errorf("error should name the live capture and the URL, got: %v", err)
	}
}

func TestPageViewCommand_BadGridFlag(t *testing.T) {
	resetPageViewFlags(t)
	pageViewPNG = "whatever.png"
	_, err := runPageView(t, "http://localhost:3000/", "--png", pageViewPNG, "--grid", "48")
	if err == nil {
		t.Fatal("expected an error for a malformed --grid")
	}
	if !strings.Contains(err.Error(), "invalid --grid") {
		t.Errorf("error should mention the flag, got: %v", err)
	}
}

func TestPageViewParseGrid(t *testing.T) {
	w, h, err := parseGrid("48x24")
	if err != nil || w != 48 || h != 24 {
		t.Errorf("parseGrid(48x24) = %d,%d,%v", w, h, err)
	}
	if _, _, err := parseGrid("160x160"); err != nil {
		t.Errorf("parseGrid(160x160) must pass, got %v", err)
	}
	for _, bad := range []string{"", "48", "48xx24", "0x24", "3x24", "200x24", "axb"} {
		if _, _, err := parseGrid(bad); err == nil {
			t.Errorf("parseGrid(%q) must fail", bad)
		}
	}
}
