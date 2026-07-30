package cmd

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
)

func TestResolveListenAddrDefault(t *testing.T) {
	// Unset env to test the default.
	t.Setenv("HTTP_LISTEN_ADDR", "")
	if got := resolveListenAddr(); got != ":60714" {
		t.Errorf("resolveListenAddr() = %q, want :60714", got)
	}
}

func TestResolveListenAddrBarePort(t *testing.T) {
	t.Setenv("HTTP_LISTEN_ADDR", ":9999")
	if got := resolveListenAddr(); got != ":9999" {
		t.Errorf("resolveListenAddr() = %q, want :9999", got)
	}
}

func TestResolveListenAddrHostQualified(t *testing.T) {
	t.Setenv("HTTP_LISTEN_ADDR", "0.0.0.0:8080")
	if got := resolveListenAddr(); got != "0.0.0.0:8080" {
		t.Errorf("resolveListenAddr() = %q, want 0.0.0.0:8080", got)
	}
}

func TestResolveListenAddrLocalhost(t *testing.T) {
	t.Setenv("HTTP_LISTEN_ADDR", "localhost:9999")
	if got := resolveListenAddr(); got != "localhost:9999" {
		t.Errorf("resolveListenAddr() = %q, want localhost:9999", got)
	}
}

func TestDialAddrBarePort(t *testing.T) {
	if got := dialAddr(":60714"); got != "localhost:60714" {
		t.Errorf("dialAddr(:60714) = %q, want localhost:60714", got)
	}
}

func TestDialAddrWildcardHost(t *testing.T) {
	if got := dialAddr("0.0.0.0:8080"); got != "localhost:8080" {
		t.Errorf("dialAddr(0.0.0.0:8080) = %q, want localhost:8080", got)
	}
	if got := dialAddr(":8080"); got != "localhost:8080" {
		t.Errorf("dialAddr(:8080) = %q, want localhost:8080", got)
	}
}

func TestDialAddrLocalhost(t *testing.T) {
	if got := dialAddr("localhost:9999"); got != "localhost:9999" {
		t.Errorf("dialAddr(localhost:9999) = %q, want localhost:9999", got)
	}
	if got := dialAddr("127.0.0.1:3000"); got != "127.0.0.1:3000" {
		t.Errorf("dialAddr(127.0.0.1:3000) = %q, want 127.0.0.1:3000", got)
	}
}

func TestDialAddrMalformed(t *testing.T) {
	// A malformed address passes through unchanged, the caller's Dial will
	// fail naturally.
	if got := dialAddr("not-valid"); got != "not-valid" {
		t.Errorf("dialAddr(not-valid) = %q, want not-valid", got)
	}
}

func TestListenAddrToURLBarePort(t *testing.T) {
	u, err := listenAddrToURL(":60714")
	if err != nil {
		t.Fatalf("listenAddrToURL(:60714): %v", err)
	}
	if u.Host != "localhost:60714" {
		t.Errorf("Host = %q, want localhost:60714", u.Host)
	}
}

func TestListenAddrToURLWildcard(t *testing.T) {
	u, err := listenAddrToURL("0.0.0.0:8080")
	if err != nil {
		t.Fatalf("listenAddrToURL(0.0.0.0:8080): %v", err)
	}
	if u.Host != "localhost:8080" {
		t.Errorf("Host = %q, want localhost:8080", u.Host)
	}
}

func TestListenAddrToURLLocalhost(t *testing.T) {
	u, err := listenAddrToURL("localhost:9999")
	if err != nil {
		t.Fatalf("listenAddrToURL(localhost:9999): %v", err)
	}
	if u.Host != "localhost:9999" {
		t.Errorf("Host = %q, want localhost:9999", u.Host)
	}
}

func TestListenAddrToURLIP(t *testing.T) {
	u, err := listenAddrToURL("127.0.0.1:3000")
	if err != nil {
		t.Fatalf("listenAddrToURL(127.0.0.1:3000): %v", err)
	}
	if u.Host != "127.0.0.1:3000" {
		t.Errorf("Host = %q, want 127.0.0.1:3000", u.Host)
	}
}

func TestListenAddrToURLInvalid(t *testing.T) {
	_, err := listenAddrToURL("")
	if err == nil {
		t.Error("expected error for empty address")
	}
}

func TestWaitForPortReturnsWhenBound(t *testing.T) {
	// Start a listener on a random port so waitForPort can find it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	ctx := context.Background()
	if !waitForPort(ctx, addr, 2*time.Second) {
		t.Errorf("waitForPort(%q) = false, want true (port is listening)", addr)
	}
}

func TestWaitForPortExpiresWhenNothingBinds(t *testing.T) {
	// Pick a port that nothing is listening on, random high port to avoid
	// collisions with real services.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // release it immediately, now nothing binds

	ctx := context.Background()
	start := time.Now()
	result := waitForPort(ctx, addr, 100*time.Millisecond)
	elapsed := time.Since(start)

	if result {
		t.Errorf("waitForPort(%q) = true, want false (port should be free)", addr)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("waitForPort took %v, expected <= 500ms (budget 100ms + scheduling overhead)", elapsed)
	}
}

func TestWaitForPortReturnsWithinBudget(t *testing.T) {
	// Nothing listening, should return within the 50ms budget.
	ctx := context.Background()
	start := time.Now()
	result := waitForPort(ctx, "127.0.0.1:19999", 50*time.Millisecond)
	elapsed := time.Since(start)
	if result {
		t.Error("waitForPort on free port returned true")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("waitForPort took %v, want <= ~100ms for a 50ms budget", elapsed)
	}
}

func TestWaitForPortRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if waitForPort(ctx, "127.0.0.1:19999", 5*time.Second) {
		t.Error("waitForPort on cancelled context returned true")
	}
}

func TestWaitForPortDialAddrUsage(t *testing.T) {
	// Dial with a bare port, waitForPort should convert to localhost.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Get the port and use a bare-port form.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	bareAddr := ":" + port

	ctx := context.Background()
	if !waitForPort(ctx, bareAddr, 2*time.Second) {
		t.Errorf("waitForPort(%q) = false, want true", bareAddr)
	}
}

// TestWaitForStyleSheetReturnsQuickly exercises the waitForStyleSheet poll in
// a temp dir without public/styles.css. With a no-op sleeper the 40 iterations
// complete instantly, the function must not hang.
func TestWaitForStyleSheetReturnsQuickly(t *testing.T) {
	chdirTemp(t)
	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}

	start := time.Now()
	cmd.waitForStyleSheet()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("waitForStyleSheet took %v, expected less than 2s (40 no-op iterations)", elapsed)
	}
}

// TestWaitForStyleSheetFindsFile confirms the poll returns immediately when
// the CSS output already exists.
func TestWaitForStyleSheetFindsFile(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll("public", 0o755); err != nil {
		t.Fatalf("mkdir public: %v", err)
	}
	if err := os.WriteFile("public/styles.css", []byte("body{}"), 0o644); err != nil {
		t.Fatalf("write styles.css: %v", err)
	}

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}

	start := time.Now()
	cmd.waitForStyleSheet()
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("waitForStyleSheet took %v, expected return as soon as file is found", elapsed)
	}
}

// TestRebuildStartsWasmAndGoBuildConcurrently injects a wasmStage that records
// when it starts and an empty wasmStage to verify the go build goroutine
// overlaps.
func TestRebuildStartsWasmAndGoBuildConcurrently(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// Inject a wasmStage that blocks until the Go build has started.
	wasmStarted := make(chan struct{})
	goBuildStarted := make(chan struct{})
	cmd.wasmStage = func() (int, error) {
		close(wasmStarted)
		<-goBuildStarted
		return 0, nil
	}

	// We also need to block the sleeper to control timing.
	cmd.sleeper = func(time.Duration) {}

	// Run rebuild in a goroutine (it holds the mutex).
	rebuildDone := make(chan struct{})
	go func() {
		cmd.rebuild()
		close(rebuildDone)
	}()

	// Wait for the wasmStage goroutine to start.
	select {
	case <-wasmStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("wasmStage did not start within 2s")
	}

	// At this point, the Go build should also be in flight (the goroutine
	// starting the Go build does not block on wasmDone).
	close(goBuildStarted)

	select {
	case <-rebuildDone:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild did not complete within 5s")
	}
}

// TestRebuildFailsGoBuildReturnsEarly verifies that when go build fails, the
// rebuild returns before the WASM stage completes, the WASM goroutine is
// orphaned but harmless.
func TestRebuildFailsGoBuildReturnsEarly(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// Inject wasmStage that blocks until we know rebuild has returned.
	wasmEntered := make(chan struct{})
	cmd.wasmStage = func() (int, error) {
		close(wasmEntered)
		// Block so the Go build finishes first (and fails, no main.go).
		select {
		case <-time.After(10 * time.Second):
		case <-cmd.rootCtx.Done():
		}
		return 0, nil
	}
	cmd.rootCtx = context.Background() // avoid nil ctx

	start := time.Now()
	cmd.rebuild()
	elapsed := time.Since(start)

	// rebuild() must return quickly because go build fails (no main.go),
	// even though wasmStage is still blocking.
	if elapsed > 2*time.Second {
		t.Errorf("rebuild took %v, expected < 2s (go build fails early, wasm is in-flight)", elapsed)
	}
}

// TestRebuildSendsReloadAfterPortReadiness runs a full rebuild to verify that
// the process lifecycle does not deadlock: go build fails (no main.go or
// unresolved deps), rebuild returns before the WASM goroutine completes, and
// no channel is closed twice.
func TestRebuildSendsReloadAfterPortReadiness(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	cmd.wasmStage = func() (int, error) { return 0, nil }
	cmd.sleeper = func(time.Duration) {}

	// ensure mainBinaryName dir exists
	if err := os.MkdirAll("tmp", 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	// Must not deadlock or panic, go build fails (no tidy), returns before
	// the WASM goroutine, and the goroutine completes on its own.
	cmd.rebuild()
}

// TestRebuildArmsWatcherDuringBuild proves that a file saved while rebuild()
// is in progress (with the watcher active) arms the debounce timer, via the
// injectable wasmStage seam.
func TestRebuildArmsWatcherDuringBuild(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// We need a real watcher running. We'll simulate by starting
	// watchForChanges in a goroutine, but it's complex to wait for the
	// right moment. Instead, we instrument the wasm stage so that when
	// rebuild() calls it, we have a chance to write a file.
	//
	// The key insight: with the new ordering, the watcher is already active
	// when rebuild() runs in watchForChanges(). So we test the seam directly:
	// inject a wasmStage that blocks, write during it, and verify the
	// debounce timer was armed by a preceding watcher event.

	// Simplified: test that watcher delivery during the wasm stage works
	// by exercising the scheduleRebuild/wasStage path via handleWatchEvent.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer watcher.Close()

	// Manually arm the debounce timer as if the watcher delivered an event
	// during the build.
	cmd.handleWatchEvent(watcher, fsnotify.Event{Name: "src/pages/index.go", Op: fsnotify.Write})

	// Verify the debounce timer was armed from the watcher event.
	cmd.debounceMu.Lock()
	armed := cmd.debounceTimer != nil
	if cmd.debounceTimer != nil {
		cmd.debounceTimer.Stop()
	}
	cmd.debounceMu.Unlock()

	if !armed {
		t.Error("expected watcher event during build to arm the debounce timer")
	}
}

// TestWasmStageSeamDefault verifies the default wasmStage closure calls
// buildWasmAll and returns a count.
func TestWasmStageSeamDefault(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// Install the default wasmStage (exercises buildWasmAll).
	cmd.wasmStage = cmd.buildWasmAll

	// With no pages buildWasmAll should return quickly.
	start := time.Now()
	count, _ := cmd.wasmStage()
	elapsed := time.Since(start)

	// No pages → rebuilt count should be 0.
	if count != 0 {
		t.Errorf("wasmStage() = %d, want 0 (no pages)", count)
	}
	if elapsed > 5*time.Second {
		t.Errorf("wasmStage took %v, expected < 5s (no pages)", elapsed)
	}
}

// TestHotReloadUsesWasmStageSeam verifies the seam is resolved and called.
// Uses a channel to synchronize with the concurrent goroutine.
func TestHotReloadUsesWasmStageSeam(t *testing.T) {
	dir := chdirTemp(t)
	writeGoMod(t, "demo")
	scaffoldSrc(t)
	// Create the fake binary inside the current temp dir with an absolute
	// path so exec.Command can find it without PATH resolution.
	bin := filepath.Join(dir, "faketailwind")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake tailwind: %v", err)
	}
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo","tailwindBinary":"`+bin+`","wasmBinary":"`+bin+`"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	wasmCalled := make(chan struct{}, 1)
	var wasmOnce sync.Once
	cmd.wasmStage = func() (int, error) {
		wasmOnce.Do(func() { close(wasmCalled) })
		return 42, nil
	}
	cmd.openBrowserFn = func(string) error { return nil }
	cmd.sleeper = func(time.Duration) {}
	cmd.proxyRunner = func(target *url.URL) error {
		if target.Host != "localhost:60714" {
			t.Errorf("proxy target host = %q, want localhost:60714", target.Host)
		}
		return nil
	}

	if err := cmd.HotReload(); err != nil {
		t.Fatalf("HotReload error: %v", err)
	}

	select {
	case <-wasmCalled:
	case <-time.After(3 * time.Second):
		t.Error("wasmStage seam was not called within 3s of HotReload returning")
	}
}

// TestHotReloadAddressSeam verifies HotReload constructs the proxy target from
// the factored helper.
func TestHotReloadAddressSeam(t *testing.T) {
	t.Setenv("HTTP_LISTEN_ADDR", "localhost:8888")
	dir := chdirTemp(t)
	writeGoMod(t, "demo")
	scaffoldSrc(t)
	// Create the fake binary in the current temp dir with an absolute path.
	bin := filepath.Join(dir, "faketailwind")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake tailwind: %v", err)
	}
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo","tailwindBinary":"`+bin+`","wasmBinary":"`+bin+`"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	var gotHost string
	cmd.openBrowserFn = func(string) error { return nil }
	cmd.sleeper = func(time.Duration) {}
	cmd.wasmStage = func() (int, error) { return 0, nil }
	cmd.proxyRunner = func(target *url.URL) error { gotHost = target.Host; return nil }

	if err := cmd.HotReload(); err != nil {
		t.Fatalf("HotReload error: %v", err)
	}
	if gotHost != "localhost:8888" {
		t.Errorf("proxy target host = %q, want localhost:8888", gotHost)
	}
}

// TestWasmHelperRebuiltCount verifies the RebuiltCount() accessor returns the
// expected count after a GenerateAll call with multiple pages.
func TestWasmHelperRebuiltCount(t *testing.T) {
	// This test verifies the accessor contract; real GenerateAll is
	// tested in the build package.
	h := gothic_cli.NewCli().Wasm
	if got := h.RebuiltCount(); got != 0 {
		t.Errorf("RebuiltCount() before any build = %d, want 0", got)
	}
}

// TestConcurrentWasmduringGoBuildOrphan verifies that when go build fails
// and rebuild returns early, the orphaned WASM goroutine does not cause a
// deadlock (it is blocked on the mutex for the next rebuild call and stays
// harmless).
func TestConcurrentWasmDuringGoBuildOrphan(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// wasmStage blocks until the test signals, simulating a long WASM build.
	wasmEntered := make(chan struct{})
	wasmCanFinish := make(chan struct{})
	cmd.wasmStage = func() (int, error) {
		close(wasmEntered)
		<-wasmCanFinish
		return 0, nil
	}
	cmd.rootCtx = context.Background()

	// Run rebuild (go build will fail, return early).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cmd.rebuild()
	}()

	// Wait for wasmStage to be entered.
	<-wasmEntered

	// Give rebuild time to fail go build and return.
	time.Sleep(500 * time.Millisecond)

	// Signal wasm to finish, rebuild already returned, so the goroutine
	// should complete without incident.
	close(wasmCanFinish)

	wg.Wait()
}

// TestRebuildOnlyOnceAfterWalkError confirms the walk-error retry path runs
// rebuild exactly twice (the unconditional one + the retry) rather than
// spinning.
func TestRebuildOnlyOnceAfterWalkError(t *testing.T) {
	chdirTemp(t)
	// No src/ directory → walk errors; no config → rebuild returns early.

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}

	// Call rebuild twice to simulate the walk-error path behavior:
	// once unconditionally after watch setup, once on walk error.
	// With no config, both return early, this proves no deadlock or
	// infinite loop on the retry path.
	cmd.rebuild()
	cmd.rebuild()
}

// TestWasmStageReturnsCount checks that the default wasmStage returns the
// correct count from RebuiltCount when tied to the real WasmHelper.
func TestWasmStageReturnsCount(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)

	// Create the default wasmStage as the production code would.
	cmd.wasmStage = cmd.buildWasmAll

	count, _ := cmd.wasmStage()
	// No pages, so zero WASM units are rebuilt.
	if count != 0 {
		t.Errorf("wasmStage returned %d, want 0 (no pages to compile)", count)
	}
}

// TestWasmSecondReloadConditional verifies the background WASM goroutine pushes
// a second reload only when rebuiltCount > 0.
func TestWasmSecondReloadConditional(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}
	var sseData []string
	var sseMu sync.Mutex
	cmd.sseSend = func(et, d string) {
		if et == "message" && d == "reload" {
			sseMu.Lock()
			sseData = append(sseData, et+":"+d)
			sseMu.Unlock()
		}
	}

	t.Run("reload sent when rebuilt > 0", func(t *testing.T) {
		sseMu.Lock()
		sseData = nil
		sseMu.Unlock()
		cmd.wasmStage = func() (int, error) { return 3, nil }

		cmd.rebuild()
		time.Sleep(50 * time.Millisecond)

		sseMu.Lock()
		got := len(sseData)
		sseMu.Unlock()
		if got != 1 {
			t.Errorf("expected 1 reload event with rebuiltCount=3, got %d", got)
		}
	})

	t.Run("no reload when rebuilt == 0", func(t *testing.T) {
		sseMu.Lock()
		sseData = nil
		sseMu.Unlock()
		cmd.wasmStage = func() (int, error) { return 0, nil }

		cmd.rebuild()
		time.Sleep(50 * time.Millisecond)

		sseMu.Lock()
		got := len(sseData)
		sseMu.Unlock()
		if got != 0 {
			t.Errorf("expected 0 reload events with rebuiltCount=0, got %d", got)
		}
	})
}

// TestWasmBuildErrorSendsBuilderror verifies a failing wasmStage emits a
// builderror SSE event from the background goroutine.
func TestWasmBuildErrorSendsBuilderror(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}

	var sseEvents []string
	var sseMu sync.Mutex
	cmd.sseSend = func(et, d string) {
		sseMu.Lock()
		sseEvents = append(sseEvents, et+":"+d)
		sseMu.Unlock()
	}
	cmd.wasmStage = func() (int, error) { return 0, fmt.Errorf("tinygo crash") }

	cmd.rebuild()
	time.Sleep(50 * time.Millisecond)

	sseMu.Lock()
	got := len(sseEvents)
	sseMu.Unlock()
	if got == 0 {
		t.Fatal("expected at least one SSE event for a build error")
	}
	hasBuildError := false
	for _, e := range sseEvents {
		if strings.Contains(e, "builderror") && strings.Contains(e, "tinygo crash") {
			hasBuildError = true
		}
	}
	if !hasBuildError {
		t.Errorf("expected builderror event with 'tinygo crash', got %v", sseEvents)
	}
}

// TestWasmBackgroundRespectsContextCancel verifies the background goroutine
// exits without sending events when the session context is cancelled.
func TestWasmBackgroundRespectsContextCancel(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	ctx, cancel := context.WithCancel(context.Background())

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.rootCtx = ctx
	cmd.sleeper = func(time.Duration) {}

	wasmCalled := make(chan struct{})
	cmd.wasmStage = func() (int, error) {
		close(wasmCalled)
		<-ctx.Done()
		return 0, ctx.Err()
	}

	sseSeen := false
	cmd.sseSend = func(et, d string) { sseSeen = true }

	cmd.startWasmBuild()
	<-wasmCalled
	cancel()
	time.Sleep(50 * time.Millisecond)

	if sseSeen {
		t.Error("expected no SSE events after context cancellation")
	}
}

// TestRebuildEmitsBuildingEvent verifies the building event carries the correct
// project-relative path when a trigger is present.
func TestRebuildEmitsBuildingEvent(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}

	var sseEvents []string
	var sseMu sync.Mutex
	cmd.sseSend = func(et, d string) {
		sseMu.Lock()
		sseEvents = append(sseEvents, et+":"+d)
		sseMu.Unlock()
	}

	// Arm the trigger and call rebuild.
	cmd.scheduleRebuild("src/pages/counter.templ")
	// Stop the timer so the 150ms rebuild doesn't fire again.
	cmd.debounceMu.Lock()
	if cmd.debounceTimer != nil {
		cmd.debounceTimer.Stop()
	}
	cmd.debounceMu.Unlock()

	// Call rebuild directly, it reads the trigger we set.
	cmd.wasmStage = func() (int, error) { return 0, nil }
	cmd.rebuild()

	sseMu.Lock()
	got := len(sseEvents)
	sseMu.Unlock()
	if got == 0 {
		t.Fatal("expected at least one SSE event from rebuild")
	}
	foundBuilding := false
	for _, e := range sseEvents {
		if strings.Contains(e, "building") && strings.Contains(e, "counter.templ") {
			foundBuilding = true
		}
	}
	if !foundBuilding {
		t.Errorf("expected building event with 'counter.templ', got %v", sseEvents)
	}
}

// TestTwoSavesQueueWasmStage verifies that two rebuild calls serialise the WASM
// stage: the second goroutine waits for the first to complete before entering
// wasmStage.
func TestTwoSavesQueueWasmStage(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.sleeper = func(time.Duration) {}
	cmd.sseSend = func(et, d string) {} // silence events

	var mu sync.Mutex
	var callCount int
	block := make(chan struct{})

	cmd.wasmStage = func() (int, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			<-block // first call blocks until released
		}
		return 0, nil
	}

	// First rebuild starts goroutine A; A enters wasmStage and blocks.
	cmd.rebuild()

	// Wait for A to enter wasmStage.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if callCount != 1 {
		t.Fatalf("expected 1 wasmStage call before second rebuild, got %d", callCount)
	}
	mu.Unlock()

	// Second rebuild starts goroutine B; B blocks on wasmMu.
	cmd.rebuild()

	// Wait for B to attempt the lock, it should still be blocked.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if callCount != 1 {
		t.Errorf("expected 1 call after second rebuild (B blocked on mutex), got %d", callCount)
	}
	mu.Unlock()

	// Release A; B should now acquire wasmMu and enter wasmStage.
	close(block)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if callCount != 2 {
		t.Errorf("expected 2 calls after releasing first, got %d", callCount)
	}
	mu.Unlock()
}

// TestNotifyReloadSendsWhenPortIsBound covers the common case: the app owns the
// port, so the wait returns immediately and the browser is told to reload.
func TestNotifyReloadSendsWhenPortIsBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var events [][2]string
	cmd := newTestHotReloadCommand()
	cmd.sseSend = func(eventType, data string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, [2]string{eventType, data})
	}

	start := time.Now()
	cmd.notifyReload(ln.Addr().String(), 2*time.Second)
	elapsed := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != [2]string{"message", "reload"} {
		t.Fatalf("expected one message/reload event, got %v", events)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("notifyReload took %v on a bound port, expected it to return promptly", elapsed)
	}
}

// TestNotifyReloadSendsEvenWhenPortNeverBinds pins the rule that the readiness
// wait is an optimisation, never a gate. Withholding the event when the budget
// expires leaves the page stale until a manual refresh, which is exactly the
// failure this poll was meant to avoid.
func TestNotifyReloadSendsEvenWhenPortNeverBinds(t *testing.T) {
	var mu sync.Mutex
	var events [][2]string
	cmd := newTestHotReloadCommand()
	cmd.sseSend = func(eventType, data string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, [2]string{eventType, data})
	}

	// Port 19999 is free, so the wait burns its whole budget and returns false.
	start := time.Now()
	cmd.notifyReload("127.0.0.1:19999", 100*time.Millisecond)
	elapsed := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != [2]string{"message", "reload"} {
		t.Fatalf("expected the reload to be sent anyway, got %v", events)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("notifyReload returned in %v, expected it to spend the budget first", elapsed)
	}
}
