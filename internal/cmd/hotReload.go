/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var hotReloadCmd = &cobra.Command{
	Use:   "hot-reload",
	Short: "Run your Gothic app locally in hot-reload mode.",
	Long: `This command uses Templ and Tailwind to enable real-time reloading for local development.

It allows you to develop and debug your Gothic app more efficiently, with changes instantly reflected in the browser as you save your files.`,
	RunE: newHotReloadCommand(gothic_cli.NewCli()),
}

func init() {
	rootCmd.AddCommand(hotReloadCmd)
	hotReloadCmd.Flags().BoolP("verbose", "v", false, "Show every build phase and log each HTTP request")
}

type HotReloadCommand struct {
	cli            *gothic_cli.GothicCli
	mainBinaryName string
	runCmd         *exec.Cmd
	runCancel      context.CancelFunc
	// runDone is closed by the goroutine that waits on the app process, so
	// shutdown can observe the exit without calling Wait a second time.
	runDone chan struct{}
	// rootCtx is cancelled on Ctrl+C / SIGTERM. Every child process the session
	// starts hangs off it, so none of them can survive the session.
	rootCtx           context.Context
	mutex             sync.Mutex
	excludedDirs      []string
	watchedExtensions []string
	excludeRegex      regexp.Regexp
	debounceTimer     *time.Timer
	debounceMu        sync.Mutex
	// pendingTrigger is the path whose change armed the debounce timer, so the
	// rebuild can say what caused it instead of starting with silent work.
	pendingTrigger string
	// wasmInventoryShown keeps the page/component/topic count to the first build
	// of the session, where it is the only thing describing the work ahead.
	wasmInventoryShown bool
	verbose            bool
	// wasmMu serialises the WASM stage inside the background goroutine so a
	// burst of saves queues GenerateAll calls instead of racing them.
	wasmMu sync.Mutex

	// Injectable seams for tests. Defaults set in newHotReloadCommandCli are
	// exactly equivalent to the previous inline behavior, so production paths
	// are unchanged.
	openBrowserFn func(url string) error      // default: defaultOpenBrowser
	sleeper       func(d time.Duration)       // default: time.Sleep
	proxyRunner   func(target *url.URL) error // default: cli.Proxy.RunProxy("localhost", 3000, target)
	wasmStage     func() (int, error)         // default: buildWasmAll + RebuiltCount
	sseSend       func(eventType, data string) // default: command.cli.Proxy.Sse.Send
	// browserProbeBudget is how long to wait for an already-open tab to
	// reconnect before opening a new one. Unset takes the default; the hermetic
	// tests set a nanosecond so the probe resolves at once.
	browserProbeBudget time.Duration
}

func newHotReloadCommandCli(cli *gothic_cli.GothicCli) HotReloadCommand {
	mainBinary := "tmp/main"
	if cli.Runtime == "windows" {
		mainBinary = "tmp/main.exe"
	}
	return HotReloadCommand{
		cli:               cli,
		mainBinaryName:    mainBinary,
		excludedDirs:      []string{"assets", "tmp", "vendor", "public", "routes"},
		watchedExtensions: []string{".go", ".tpl", ".tmpl", ".templ", ".html"},
		excludeRegex:      *regexp.MustCompile(`.*_templ\.go$|.*_gen\.go$`),
		// Seam fields are left nil here and resolved to their production
		// defaults at the call site (see HotReload). This avoids binding a
		// method value to the about-to-be-copied struct, and keeps the default
		// behavior byte-for-byte identical to the pre-seam code.
	}
}

func newHotReloadCommand(cli gothic_cli.GothicCli) RunEFunc {
	return func(cmd *cobra.Command, args []string) error {
		command := newHotReloadCommandCli(&cli)
		command.verbose, _ = cmd.Flags().GetBool("verbose")

		return command.HotReload()
	}
}

func (command *HotReloadCommand) HotReload() error {
	// Ctrl+C and SIGTERM cancel this context, which tears down the Tailwind
	// watcher and the app process with it. Without it those children are only
	// reached by the terminal's own signal to the foreground process group, so
	// any other kind of exit leaves them running.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	command.rootCtx = rootCtx

	// Resolve injectable seams to their production defaults when unset. Binding
	// the method value here (pointer receiver) is safe and equivalent to the
	// original inline calls.
	if command.openBrowserFn == nil {
		command.openBrowserFn = command.defaultOpenBrowser
	}
	if command.sleeper == nil {
		command.sleeper = time.Sleep
	}
	if command.proxyRunner == nil {
		command.proxyRunner = func(target *url.URL) error {
			return command.cli.Proxy.RunProxy("localhost", 3000, target)
		}
	}
	if command.wasmStage == nil {
		command.wasmStage = command.buildWasmAll
	}
	if command.sseSend == nil {
		command.sseSend = command.cli.Proxy.Sse.Send
	}
	if command.browserProbeBudget == 0 {
		command.browserProbeBudget = defaultBrowserProbeBudget
	}
	godotenv.Load()
	// Load config to pick up binary overrides if present
	command.cli.GetConfig()
	// Ensure tailwind binary is available before starting watch
	if _, err := command.cli.Tailwind.EnsureBinary(); err != nil {
		return fmt.Errorf("error resolving tailwind binary: %w", err)
	}
	// Ensure TinyGo is installed before any goroutines start, avoids a
	// race between the download and the first rebuild() call.
	if err := command.cli.Wasm.EnsureBinary(); err != nil {
		return fmt.Errorf("error resolving tinygo binary: %w", err)
	}
	listenAddr := resolveListenAddr()
	targetURL, err := listenAddrToURL(listenAddr)
	if err != nil {
		return fmt.Errorf("invalid target URL from %q: %w", listenAddr, err)
	}
	go command.watchTailwindChanges()
	// Wait for the first CSS output to appear instead of a fixed sleep, so
	// the browser has styles on the first navigation. The sleeper-based poll
	// keeps the 4s budget but returns as soon as the file is ready.
	command.waitForStyleSheet()
	go command.watchForChanges()

	proxyErrCh := make(chan error, 1)
	go func() {
		proxyErrCh <- command.proxyRunner(targetURL)
	}()

	output.PrintRaw(banner())
	// Open a tab only when nobody is already watching. A tab left open from a
	// previous session reconnects to the reload stream on its own, so opening
	// another one just piles up duplicates across restarts.
	if !command.browserAlreadyOpen(command.browserProbeBudget) {
		command.openBrowserFn("http://127.0.0.1:3000")
	}

	select {
	case err := <-proxyErrCh:
		if err != nil {
			return fmt.Errorf("proxy server error: %w", err)
		}
		return nil
	case <-rootCtx.Done():
		// Cancelling rootCtx already signalled the children. Give them the
		// WaitDelay window to exit on their own before this process goes.
		output.Println("Shutting down...")
		command.awaitChildren(5 * time.Second)
		return nil
	}
}

// defaultBrowserProbeBudget covers the reload stream's own retry interval plus
// the connect, so a tab left over from the previous session has time to come
// back before the session decides nobody is watching.
const defaultBrowserProbeBudget = 1500 * time.Millisecond

// browserAlreadyOpen reports whether a tab is already listening to the reload
// stream, polling until one shows up or the budget expires. The stream sends a
// short retry interval, so a tab left over from the previous session reconnects
// well inside this window.
func (command *HotReloadCommand) browserAlreadyOpen(budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if command.cli.Proxy.Sse.Subscribers() > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// phase reports an internal build step. These are identical on every cycle and
// in the same order, so by default the stream shows only what changed and what
// it cost; --verbose brings the full sequence back for debugging.
func (command *HotReloadCommand) phase(format string, args ...any) {
	if !command.verbose {
		return
	}
	output.Println(format, args...)
}

// awaitChildren waits for the app process to report exit after cancellation, so
// the session does not vanish while a child is still writing to the terminal.
// It watches the channel the run goroutine closes rather than calling Wait: the
// process is already being waited on there, and a second Wait is an error.
func (command *HotReloadCommand) awaitChildren(grace time.Duration) {
	command.mutex.Lock()
	done := command.runDone
	command.mutex.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func (command *HotReloadCommand) isExcludedDir(path string) bool {
	for _, d := range command.excludedDirs {
		if strings.Contains(path, string(os.PathSeparator)+d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (command *HotReloadCommand) watchForChanges() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Returning is mandatory: every watcher call below would deref nil.
		output.Errorln("cannot watch for changes, rebuilds are off: %v", err)
		if strings.Contains(err.Error(), "too many open files") {
			// A running editor can exhaust the per-user inotify instance limit on its own.
			output.Errorln("the inotify instance limit is exhausted, raise fs.inotify.max_user_instances or close other watchers")
		}
		return
	}
	defer watcher.Close()
	// Watch the project root directory for changes to main.go and other root-level files
	if err := watcher.Add("."); err != nil {
		output.Errorln("cannot watch the project root: %v", err)
	}
	walkErr := filepath.Walk("src", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && command.isExcludedDir(path) {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return watcher.Add(path)
		}
		return nil
	})
	// Register the watcher before the first build: saves during the build
	// arm a rebuild instead of being silently dropped.
	command.rebuild()
	if walkErr != nil {
		output.Errorln("cannot walk the project directories: %v", walkErr)
		// The first rebuild may have created files the walk expected; retry
		// once in case the error was a temporary directory-not-found.
		command.rebuild()
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			command.handleWatchEvent(watcher, event)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			output.Errorln("watcher error: %v", err)
		}
	}
}

// handleWatchEvent processes a single fsnotify event: it schedules a rebuild
// when the changed path is relevant and dynamically adds newly created
// directories to the watcher. Extracted from watchForChanges' select loop so
// the per-event logic is unit-testable without a running watcher; the
// production loop calls this unchanged.
func (command *HotReloadCommand) handleWatchEvent(watcher *fsnotify.Watcher, event fsnotify.Event) {
	if command.shouldHandle(event.Name, event.Op) {
		command.scheduleRebuild(event.Name)
	}
	// Dynamically watch new directories
	if event.Op&fsnotify.Create == fsnotify.Create {
		info, err := os.Stat(event.Name)
		if err == nil && info.IsDir() && !command.isExcludedDir(event.Name) {
			err := watcher.Add(event.Name)
			if err == nil {
				output.Println("Watching new directory %s", output.Link(displayPath(event.Name)))
			} else {
				output.Errorln("cannot watch new directory %s: %v", displayPath(event.Name), err)
			}
		}
	}
}

func (command *HotReloadCommand) shouldHandle(path string, op fsnotify.Op) bool {
	if command.isExcludedDir(path) {
		return false
	}

	filename := filepath.Base(path)
	if command.excludeRegex.MatchString(filename) {
		// Ignore templ-generated files unless they are deleted
		if op&(fsnotify.Remove) == 0 {
			return false
		}
	}

	ext := filepath.Ext(path)
	for _, e := range command.watchedExtensions {
		if e == ext {
			return true
		}
	}
	return false
}

// processCtx is the parent for every child process. Tests drive rebuild() and
// the watchers directly without going through HotReload, so fall back to a
// background context rather than panicking on a nil field.
func (command *HotReloadCommand) processCtx() context.Context {
	if command.rootCtx == nil {
		return context.Background()
	}
	return command.rootCtx
}

// maxTailwindRestarts bounds recovery: a watcher that keeps dying is a broken
// config or a broken binary, and a restart loop would bury the real error.
const maxTailwindRestarts = 5

// watchTailwindChanges supervises the Tailwind watcher for the whole session.
// The watcher is a long-lived Node process that can die on its own, it has hit
// the V8 heap limit under heavy rebuild churn, and without a restart the CSS
// silently stops updating for the rest of the session.
func (command *HotReloadCommand) watchTailwindChanges() {
	ctx := command.processCtx()
	output.Println("Starting Tailwind in watch mode...")

	for restarts := 0; ; restarts++ {
		tailWindCmd, err := command.cli.Tailwind.WatchStart(ctx)
		if err != nil {
			output.Errorln("cannot start the Tailwind watcher: %v", err)
			return
		}
		command.phase("Tailwind is watching with PID %s", output.Accent(strconv.Itoa(tailWindCmd.Process.Pid)))

		waitErr := tailWindCmd.Wait()

		// A cancelled context means we asked it to stop, so this is the normal
		// shutdown path, not a failure.
		if ctx.Err() != nil {
			command.phase("Tailwind watcher stopped")
			return
		}
		// An exit code of 0 still counts as a death here: the watcher is only
		// supposed to end when we cancel it, and waitErr is nil in that case.
		cause := "exited on its own"
		if waitErr != nil {
			cause = waitErr.Error()
		}
		if restarts >= maxTailwindRestarts {
			output.Errorln("the Tailwind watcher died %d times (%s), giving up, CSS will not rebuild",
				restarts+1, cause)
			return
		}
		output.Errorln("the Tailwind watcher died (%s), restarting it", cause)
		command.sleepFor(time.Second)
	}
}

// sleepFor honours the injected sleeper so tests do not wait in real time.
func (command *HotReloadCommand) sleepFor(d time.Duration) {
	if command.sleeper != nil {
		command.sleeper(d)
		return
	}
	time.Sleep(d)
}

// waitForStyleSheet polls for Tailwind's CSS output to appear, with a 4s
// budget (40 checks x 100ms sleep). The count-based budget means a stubbed
// sleeper (no-op in tests) completes all 40 iterations instantly rather than
// burning wall-clock time. Returns as soon as the file is ready.
func (command *HotReloadCommand) waitForStyleSheet() {
	stylePath := "public/styles.css"
	for i := 0; i < 40; i++ {
		info, err := os.Stat(stylePath)
		if err == nil && info.Size() > 0 {
			return
		}
		command.sleepFor(100 * time.Millisecond)
	}
}

// scheduleRebuild coalesces rapid fsnotify events (e.g. WRITE+CHMOD from a
// single editor save) into a single rebuild. The timer resets on each event;
// the rebuild fires 150ms after the last event in the burst.
func (command *HotReloadCommand) scheduleRebuild(trigger string) {
	command.debounceMu.Lock()
	defer command.debounceMu.Unlock()
	if trigger != "" {
		// Several files can land inside one debounce window; the most recent
		// one is the useful answer to "why is it rebuilding right now?".
		command.pendingTrigger = trigger
	}
	if command.debounceTimer != nil {
		command.debounceTimer.Stop()
	}
	command.debounceTimer = time.AfterFunc(150*time.Millisecond, command.rebuild)
}

// takeTrigger returns and clears the path that armed the current rebuild.
func (command *HotReloadCommand) takeTrigger() string {
	command.debounceMu.Lock()
	defer command.debounceMu.Unlock()
	t := command.pendingTrigger
	command.pendingTrigger = ""
	return t
}

// displayPath renders a watched path the way a developer would type it: relative
// to the project root, with no leading "./".
func displayPath(p string) string {
	if filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(wd, p); err == nil {
				p = rel
			}
		}
	}
	return strings.TrimPrefix(filepath.Clean(p), "./")
}

func (command *HotReloadCommand) rebuild() {
	command.mutex.Lock()
	defer command.mutex.Unlock()

	start := time.Now()
	// Resolve the sseSend seam to its default when rebuild() is called without
	// going through HotReload (e.g. in unit tests that don't set up the Proxy).
	if command.sseSend == nil {
		command.sseSend = command.cli.Proxy.Sse.Send
	}

	// Name the file that caused this cycle before the slow work starts, so the
	// wait between the save and the first build result is never unexplained.
	if trigger := command.takeTrigger(); trigger != "" {
		output.Println("%s %s", output.Tag("Rebuilding"), output.Link(displayPath(trigger)))
		command.sseSend("building", displayPath(trigger))
	}

	command.phase("Build routes...")
	config, err := command.cli.GetConfig()
	if err != nil {
		output.Errorln("cannot read the config: %v", err)
		command.sseSend("builderror", "config: "+err.Error())
		return
	}
	if err := command.cli.FileBasedRouter.Render(config.GoModName); err != nil {
		output.Errorln("cannot build routes: %v", err)
		command.sseSend("builderror", "routes: "+err.Error())
		return
	}
	if err := syncEmbeddedPublicFile(&config); err != nil {
		output.Errorln("cannot sync the embedded public file: %v", err)
		command.sseSend("builderror", "embedded: "+err.Error())
		return
	}

	command.phase("Build templ...")
	if err := command.cli.Templ.Render(); err != nil {
		output.Errorln("templ failed: %v", err)
		command.sseSend("builderror", "templ: "+err.Error())
		return
	}

	// Use cheaper compression and skip the second optimizer pass in dev.
	command.cli.Wasm.DevShaping = true
	// The background stage prints the aggregate itself, with the duration of
	// the whole stage, so GenerateAll must not print its own copy.
	command.cli.Wasm.QuietSummary = true

	// Start WASM compile in background. WASM artifacts are static files under
	// public/wasm/ served from disk per request, so first paint never needs to
	// wait for a compile. The background stage is serialized by its own mutex
	// so a burst of saves queues GenerateAll calls instead of racing them.
	if command.sseSend == nil {
		command.sseSend = command.cli.Proxy.Sse.Send
	}
	command.startWasmBuild()

	listenAddr := resolveListenAddr()
	command.phase("Build app...")
	// Build the whole package ("."), not just main.go: the server config now lives in
	// gothic.config.go (var Config, referenced from main.go as Config.Runtime), so a
	// single-file build fails with "undefined: Config". "." compiles every .go file in
	// the package directory.
	buildCmd := exec.Command("go", "build", "-o", command.mainBinaryName, ".")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		output.Errorln("cannot build the app: %v", err)
		command.sseSend("builderror", "go build: "+err.Error())
		return
	}

	if command.runCancel != nil {
		command.phase("Stopping previous go run process...")
		command.runCancel()
		command.runCancel = nil
		// Wait for the old process to actually exit before binding again.
		// Cancelling only delivers SIGTERM, so without this the new server races
		// the old listener and loses with "address already in use", dies, and the
		// session serves nothing until the next save. The grace matches
		// runCmd.WaitDelay, which force-kills anything that hangs.
		if command.runDone != nil {
			select {
			case <-command.runDone:
			case <-time.After(5 * time.Second):
			}
			command.runDone = nil
		}
	}
	command.phase("Running app...")
	ctx, cancel := context.WithCancel(command.processCtx())
	command.runCancel = cancel

	runCmd := exec.CommandContext(ctx, command.mainBinaryName)
	// Let the server close its listener before it dies, both on a rebuild and
	// on session shutdown. WaitDelay force-kills anything that hangs.
	runCmd.Cancel = func() error {
		if err := runCmd.Process.Signal(syscall.SIGTERM); err != nil {
			return runCmd.Process.Kill()
		}
		return nil
	}
	runCmd.WaitDelay = 5 * time.Second
	// GOTHIC_MODE=dev enables dev behavior. The wasm_exec shim needs no signal:
	// the runtime serves the single embedded shim regardless of how the server
	// was started.
	runCmd.Env = append(os.Environ(), "GOTHIC_MODE=dev")
	if command.verbose {
		runCmd.Env = append(runCmd.Env, "GOTHIC_VERBOSE=true")
	}
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	command.runCmd = runCmd
	done := make(chan struct{})
	command.runDone = done
	go func() {
		defer close(done)
		if err := runCmd.Run(); err != nil {
			if ctx.Err() == nil {
				output.Errorln("the app exited: %v", err)
			}
		}
	}()

	command.notifyReload(listenAddr, 2*time.Second)

	// Closing line of the cycle: says it finished and what it cost. "Rebuilt"
	// rather than "Ready" because the app is only starting here, so claiming
	// readiness would be a lie whenever it fails to boot.
	output.Println("Rebuilt in %s", output.Accent(time.Since(start).Round(time.Millisecond).String()))
}

// notifyReload waits for the new server to own the port and then tells the
// browser to reload. The wait is an optimisation: it stops the refetch from
// landing on a socket nobody is listening on yet, which the proxy would
// otherwise absorb as a retry. Expiring the budget is NOT a reason to withhold
// the event. The proxy's transport retries a refused connection on its own,
// while a withheld event leaves the page stale until a manual refresh, which
// reads as "hot reload is broken" on any machine slow enough to miss the
// budget.
func (command *HotReloadCommand) notifyReload(addr string, budget time.Duration) {
	waitForPort(command.processCtx(), addr, budget)
	command.sseSend("message", "reload")
}

func (command *HotReloadCommand) buildWasmAll() (int, error) {
	// Gate: skip the whole stage when no input file has changed. The snapshot is
	// taken here, before anything is read, and is what gets recorded on success,
	// so a save landing mid-build is not mistaken for content this build compiled.
	snap := takeWasmInputSnapshot()
	if !snap.changed {
		return 0, nil
	}

	command.cli.Wasm.PregenerateTopicStubs()
	pages, err := command.cli.Wasm.ScanPages("src/pages", "src/components")
	if err != nil {
		if strings.Contains(err.Error(), "go mod tidy") || strings.Contains(err.Error(), "updates to go.mod needed") {
			wasmLogf("go.mod out of date, running go mod tidy...")
			tidy := exec.Command("go", "mod", "tidy")
			tidy.Stderr = os.Stderr
			if tidyErr := tidy.Run(); tidyErr != nil {
				wasmErrorf("go mod tidy failed: %v", tidyErr)
				return 0, tidyErr
			}
			pages, err = command.cli.Wasm.ScanPages("src/pages", "src/components")
		}
		if err != nil {
			wasmErrorf("scan failed: %v", err)
			return 0, err
		}
	}
	if len(pages) == 0 {
		recordWasmDigestFor(snap, nil)
		return 0, nil
	}
	var nPages, nComponents int
	for _, p := range pages {
		if p.IsComponent {
			nComponents++
		} else {
			nPages++
		}
	}
	topics := command.cli.Wasm.CountTopics()
	// Only the first build of the session gets the inventory line. On a rebuild
	// almost everything is cached, so announcing the full count would suggest
	// work that is not happening.
	if !command.wasmInventoryShown {
		wasmLogf("building %s, %s, %s...",
			wasmCount(nPages, "page(s)"),
			wasmCount(nComponents, "component(s)"),
			wasmCount(topics, "topic(s)"))
		command.wasmInventoryShown = true
	}
	if err := command.cli.Wasm.GenerateAll(pages, "public/wasm"); err != nil {
		wasmErrorf("build failed (continuing with stale binaries): %v", err)
		return 0, err
	}

	// Persist only after a successful build, and persist the pre-build snapshot:
	// see recordWasmDigestFor for why a fresh reading here would strand a stale
	// binary. The local package dirs come from the scan that just completed.
	recordWasmDigestFor(snap, collectWasmLocalDirs(pages))
	return int(command.cli.Wasm.RebuiltCount()), nil
}

// startWasmBuild spawns the WASM compile in a background goroutine hung off
// the session context. It is serialised by wasmMu so a burst of saves queues
// GenerateAll calls instead of racing them. A second reload is pushed only
// when at least one unit actually rebuilt; a cache-hit pass skips it.
func (command *HotReloadCommand) startWasmBuild() {
	if command.wasmStage == nil {
		command.wasmStage = command.buildWasmAll
	}

	go func() {
		command.wasmMu.Lock()
		defer command.wasmMu.Unlock()

		wasmStart := time.Now()
		rebuiltCount, buildErr := command.wasmStage()

		// If the session was cancelled during the build, discard the result
		// and return without sending events, the browser is gone.
		if command.processCtx().Err() != nil {
			return
		}

		if buildErr != nil {
			command.sseSend("builderror", buildErr.Error())
			return
		}

		// Clear the badge before any reload, so the document the browser swaps
		// in starts with the build already settled. A reload cannot carry this
		// signal on its own: the paint reload fires while the compile is still
		// running, and the two are indistinguishable on the client.
		command.sseSend("builddone", "")

		// Push a second reload only when at least one unit actually rebuilt.
		if rebuiltCount > 0 {
			command.sseSend("message", "reload")
		}

		// The one aggregate line of the cycle. GenerateAll stays quiet (see
		// QuietSummary) so this can carry the whole stage's duration, scan
		// included, rather than just the generate phase.
		elapsed := output.Accent(time.Since(wasmStart).Round(time.Millisecond).String())
		if line := wasmSummaryLine(command.cli.Wasm.UpToDateCount(), int32(rebuiltCount), elapsed); line != "" {
			wasmLogf("%s", line)
		}
	}()
}

func (command *HotReloadCommand) defaultOpenBrowser(url string) error {
	var cmd *exec.Cmd

	switch command.cli.Runtime {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return nil
	}

	return cmd.Start()
}
