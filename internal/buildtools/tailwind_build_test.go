package buildtools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// cachedFakeBinary sets up GOTHIC_CLI_CACHE_DIR with a fake cached tailwind
// binary for linux/amd64 so EnsureBinary resolves without a download.
func cachedFakeBinary(t *testing.T) (h TailwindHelper) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("GOTHIC_CLI_CACHE_DIR", tmpDir)
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	fakeBinary := filepath.Join(binDir, "tailwindcss-linux-x64")
	if err := os.WriteFile(fakeBinary, []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}
	return NewTailwindHelper("linux", "amd64")
}

func TestTailwindBuildSuccess(t *testing.T) {
	h := cachedFakeBinary(t)

	fr := &fakeRunner{}
	restore := setRunner(fr)
	defer restore()

	if err := h.Build(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(fr.calls))
	}
	// Assert the full build argv. args[0] is the resolved binary path (a temp
	// dir), so match from the flags onward. --minify is correctness-critical:
	// dropping it would ship unminified CSS to production.
	args := fr.calls[0]
	if len(args) < 1 {
		t.Fatalf("empty argv")
	}
	wantFlags := []string{"-i", "src/css/app.css", "-o", "public/styles.css", "--minify"}
	if !reflect.DeepEqual(args[1:], wantFlags) {
		t.Errorf("build argv flags mismatch:\n got %v\nwant %v", args[1:], wantFlags)
	}
	// The binary must be the cached fake, not a bare command name.
	if !strings.HasSuffix(args[0], "tailwindcss-linux-x64") {
		t.Errorf("expected resolved tailwind binary path, got %q", args[0])
	}
}

func TestTailwindBuildRunnerError(t *testing.T) {
	h := cachedFakeBinary(t)

	fr := &fakeRunner{responses: []fakeResponse{{err: errors.New("css boom")}}}
	restore := setRunner(fr)
	defer restore()

	if err := h.Build(); err == nil {
		t.Fatal("expected error from runner")
	}
}

func TestTailwindBuildBinaryResolveError(t *testing.T) {
	// Override points at a nonexistent file -> EnsureBinary fails before runner.
	h := NewTailwindHelper("linux", "amd64")
	h.ConfigOverride = "/nonexistent/tailwind"
	if err := h.Build(); err == nil {
		t.Fatal("expected error resolving binary")
	}
}

func TestTailwindWatchStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary not portable to windows")
	}
	// Use a real, short-lived executable as the override binary so Start()
	// succeeds and we can Wait() on it without hanging.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tailwind.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	h := NewTailwindHelper("linux", "amd64")
	h.ConfigOverride = script

	cmd, err := h.WatchStart(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected non-nil *exec.Cmd")
	}
	// Reap the process so the test does not leak it.
	_ = cmd.Wait()
}

func TestTailwindWatchStartResolveError(t *testing.T) {
	h := NewTailwindHelper("linux", "amd64")
	h.ConfigOverride = "/nonexistent/tailwind"
	if _, err := h.WatchStart(context.Background()); err == nil {
		t.Fatal("expected error resolving binary")
	}
}

func TestTailwindWatchStartDiesWithContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary not portable to windows")
	}
	// A fake watcher that ignores nothing and just runs forever, standing in
	// for the real `tailwindcss --watch=always`.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tailwind-watch.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0755); err != nil {
		t.Fatal(err)
	}

	h := NewTailwindHelper("linux", "amd64")
	h.ConfigOverride = script

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := h.WatchStart(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pid := cmd.Process.Pid

	// Cancelling the session context must take the watcher down with it.
	cancel()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		t.Fatalf("watcher pid %d outlived its context", pid)
	}
	if cmd.ProcessState == nil {
		t.Error("expected the watcher to have a final process state")
	}
}
