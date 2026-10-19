// Package browser wraps the go-rod automation library into the small surface
// the CLI dev session needs: one managed browser process, one long-lived tab,
// an origin allowlist, and a small set of scripted action primitives.
//
// The browser is launched through rod's leakless wrapper (github.com/ysmood/
// leakless): a watcher process babysits the child, so when the CLI dies
// without running its cleanup, leakless reaps the browser anyway — no Chrome
// process is ever left behind. Every call routes through the same kept-alive
// page; the browser is closed after an idle period and relaunched
// transparently on the next use.
//
// An origin allowlist guards every navigation: only loopback hosts on the
// configured dev-server ports may be opened; anything else is refused before
// the browser is even touched.
//
// This package produces no user-facing output by design; callers report
// failures to their own channels.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Defaults for the options a caller may override.
const (
	// DefaultIdleAfter is how long the browser stays up with no activity
	// before it is closed. The next call relaunches it.
	DefaultIdleAfter = 5 * time.Minute

	// DefaultStepTimeout bounds each individual scripted action step.
	DefaultStepTimeout = 15 * time.Second

	// DefaultUserDataDir keeps cookies and localStorage across relaunches so
	// session state survives a headless toggle or an idle shutdown. It is
	// relative to the CLI's working directory, which is the user's project
	// root in a dev session.
	DefaultUserDataDir = ".gothicCli/mcp/state/browser-profile"

	// SettleQuiet is the quiet window WaitStable requires before a navigation
	// counts as settled.
	SettleQuiet = 200 * time.Millisecond
)

// Options configures a Manager. Callers use NewOptions to get the
// production defaults (headless on, download off).
type Options struct {
	// Headless runs the browser without a window. Toggling it relaunches the
	// profile in the other mode; see SetHeadless.
	Headless bool

	// AllowDownload lets the manager download rod's pinned-revision Chromium
	// when launcher.LookPath finds no installed browser. The download lands
	// under the CLI cache dir (see browserDownloadDir). Tests leave this
	// false so a machine without a browser skips instead of downloading.
	AllowDownload bool

	// UserDataDir for the browser profile, keeping cookies across relaunches.
	// Empty takes DefaultUserDataDir (relative to the process working
	// directory, which for the CLI is the user's project root).
	UserDataDir string

	// IdleAfter overrides DefaultIdleAfter when positive.
	IdleAfter time.Duration

	// StepTimeout overrides DefaultStepTimeout when positive.
	StepTimeout time.Duration

	// PinResizePoll overrides the pinned-viewport user-resize watch interval
	// when positive. Tests shorten it; production leaves it zero and takes
	// the package default.
	PinResizePoll time.Duration

	// BinPath forces a specific browser executable, bypassing
	// launcher.LookPath and downloads. A test seam; production leaves it
	// empty.
	BinPath string
}

// NewOptions returns the production defaults: headless on.
func NewOptions() Options {
	return Options{Headless: true}
}

// Manager owns the managed browser lifecycle: launch, the single kept-alive
// tab, the idle close, and shutdown. It is safe for concurrent use; all calls
// serialize on one mutex so scripted actions from concurrent clients run in
// order.
type Manager struct {
	opts Options
	// sessionCtx is the dev session context: cancelling it marks in-flight
	// page operations cancelled.
	sessionCtx context.Context

	allowMu sync.Mutex
	allowed map[string]bool

	mu         sync.Mutex
	browser    *rod.Browser
	page       *rod.Page
	uiLauncher *launcher.Launcher
	launchOpts Options // the options the running browser was launched with
	idleTimer  *time.Timer
	closed     bool

	// pinned is the device-metrics override SetViewport armed on the page
	// (nil = the browser's own window size). It outlives navigations and is
	// re-applied on every relaunch; ClearViewport lifts it; a manual window
	// resize by the maintainer lifts it too (see pinWatch). frameOverhead is
	// the launch-time window-frame cost (outer minus content), measured while
	// no override is armed, so a pin can size the outer window exactly.
	pinned         *proto.EmulationSetDeviceMetricsOverride
	pinnedEmulates bool
	pinWatchStop   chan struct{}
	frameOverhead  frameOverhead

	// console is the retained page-console ring (see console.go); consoleMu
	// guards it.
	consoleMu sync.Mutex
	console   []ConsoleEntry
}

// New creates a Manager hung off sessionCtx. Nothing is launched here; the
// first Open, Act, or EnsureStarted call launches.
func New(sessionCtx context.Context, opts Options) *Manager {
	return &Manager{
		opts:       opts,
		sessionCtx: sessionCtx,
		allowed:    map[string]bool{},
	}
}

// SetAllowedHostPorts replaces the origin allowlist with host:port pairs such
// as "localhost:3000". Ports not on the list are refused; non-local hosts are
// always refused regardless of the list.
func (m *Manager) SetAllowedHostPorts(hostPorts []string) {
	m.allowMu.Lock()
	defer m.allowMu.Unlock()
	m.allowed = make(map[string]bool, len(hostPorts))
	for _, hp := range hostPorts {
		m.allowed[hp] = true
	}
}

// AllowHostPort adds one host:port pair to the origin allowlist.
func (m *Manager) AllowHostPort(hostPort string) {
	m.allowMu.Lock()
	defer m.allowMu.Unlock()
	if m.allowed == nil {
		m.allowed = map[string]bool{}
	}
	m.allowed[hostPort] = true
}

// checkURL validates a navigation target against the allowlist: only http(s)
// URLs on a loopback host whose host:port is on the list are allowed. With an
// empty list, any loopback port is allowed; the production session always
// installs its explicit dev-port list.
func (m *Manager) checkURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing to navigate to %q: the managed browser only opens http/https dev origins", rawURL)
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" {
		return fmt.Errorf("refusing to navigate to %q: the managed browser only opens the local dev origin", rawURL)
	}
	m.allowMu.Lock()
	defer m.allowMu.Unlock()
	if len(m.allowed) > 0 && !m.allowed[u.Host] {
		return fmt.Errorf("refusing to navigate to %q: the dev server is not on the managed-browser allowlist", rawURL)
	}
	return nil
}

// Open navigates the kept-alive tab to rawURL, launching the browser first if
// needed. The allowlist gates the URL before anything is launched.
func (m *Manager) Open(rawURL string) error {
	if err := m.checkURL(rawURL); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return err
	}
	if err := m.gotoLocked(page, rawURL); err != nil {
		return err
	}
	m.armIdleLocked()
	return nil
}

// gotoLocked navigates and waits for the page to settle, bounded by the
// step-timeout context so a page that never settles cannot hang a caller.
func (m *Manager) gotoLocked(page *rod.Page, rawURL string) error {
	budget := m.stepTimeout()
	ctx, cancel := context.WithTimeout(m.sessionCtx, budget)
	defer cancel()
	p := page.Context(ctx)
	if err := p.Navigate(rawURL); err != nil {
		return fmt.Errorf("navigate to %s: %w", rawURL, err)
	}
	// WaitStable waits for load plus a quiet window; the context above is the
	// hard bound if the page never settles.
	if err := p.WaitStable(SettleQuiet); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("navigate to %s timed out after %s", rawURL, budget)
		}
		return fmt.Errorf("wait for %s to settle: %w", rawURL, err)
	}
	return nil
}

// stepTimeout returns the configured per-step budget.
func (m *Manager) stepTimeout() time.Duration {
	if m.opts.StepTimeout > 0 {
		return m.opts.StepTimeout
	}
	return DefaultStepTimeout
}

// idleAfter returns the configured idle budget.
func (m *Manager) idleAfter() time.Duration {
	if m.opts.IdleAfter > 0 {
		return m.opts.IdleAfter
	}
	return DefaultIdleAfter
}

// pageLocked returns the running page, launching the browser (and tab) if
// needed. Callers hold m.mu. A previously failed launch leaves the fields
// nil, so this retries cleanly — unless the manager itself was closed by a
// previous shutdown, which is terminal.
func (m *Manager) pageLocked() (*rod.Page, error) {
	if m.closed {
		return nil, fmt.Errorf("browser manager is closed")
	}
	if m.page != nil {
		return m.page, nil
	}
	page, err := m.launchLocked()
	if err != nil {
		return nil, err
	}
	m.page = page
	return page, nil
}

// launchLocked starts the browser process, connects rod to it, and opens the
// single tab that every later call reuses. Callers hold m.mu.
func (m *Manager) launchLocked() (*rod.Page, error) {
	bin, err := m.resolveBin()
	if err != nil {
		return nil, err
	}

	userDataDir := m.opts.UserDataDir
	if userDataDir == "" {
		userDataDir = DefaultUserDataDir
	}
	// Ensure the profile dir exists before Chrome maps it: Chrome fails to
	// start when its profile directory is inside a missing parent.
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create browser profile dir %s: %w", userDataDir, err)
	}

	l := launcher.New().
		Bin(bin).
		Headless(m.opts.Headless).
		UserDataDir(userDataDir).
		Context(m.sessionCtx)

	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch managed browser: %w", err)
	}

	b := rod.New().ControlURL(controlURL).Context(m.sessionCtx)
	if err := b.Connect(); err != nil {
		l.Kill()
		return nil, fmt.Errorf("connect to managed browser: %w", err)
	}

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = b.Close()
		l.Kill()
		return nil, fmt.Errorf("open managed tab: %w", err)
	}

	// A pinned viewport survives relaunches: re-apply the agent's override to
	// the fresh target so the pin holds across idle closes and headless
	// toggles. Best effort — a CDP flake on re-apply must not take the whole
	// browser down; the pin stays armed for the next relaunch.
	if m.pinned != nil {
		if m.pinnedEmulates {
			// Same order as SetViewport: the relaunched headful window first
			// gets its real size (honest feedback, no emulation armed), then
			// the override re-arms on top of the matching content area.
			if !m.opts.Headless {
				_ = m.pinWindowTo(page, m.pinned.Width, m.pinned.Height)
			}
			_ = m.pinned.Call(page)
		} else if !m.opts.Headless {
			// Window pin: the relaunched window goes back to the pinned
			// real size; the emulation-free path keeps the app mirroring.
			_ = m.pinWindowTo(page, m.pinned.Width, m.pinned.Height)
		}
		// And the maintainer keeps control of the relaunch too.
		m.startPinWatchLocked()
	} else {
		// Measure the window-frame overhead (outer size minus the content
		// area) on a clean, unpinned launch: once a device-metrics override
		// is armed, innerHeight reports the EMULATED height, not the real
		// content area, so a pin-time measurement can never converge. The
		// overhead is what lets a later pin size the outer window exactly.
		m.frameOverhead = measureFrameOverhead(page)
		// The session's first window is the maintainer's working canvas:
		// give it the whole screen (headful), no dead space anywhere.
		if !m.opts.Headless {
			if info, err := (proto.BrowserGetWindowForTarget{}).Call(page); err == nil {
				_ = (proto.BrowserSetWindowBounds{
					WindowID: info.WindowID,
					Bounds:   &proto.BrowserBounds{WindowState: proto.BrowserWindowStateMaximized},
				}).Call(page)
				// The renderer can lag a fresh maximize (a Linux-Chrome
				// native-window/layout desync); the page is still blank here,
				// so the relayout kick and its reload cost nothing and the
				// first paint already fills the screen.
				time.Sleep(150 * time.Millisecond)
				_ = forceRelayout(page)
			}
		}
	}

	// Retain the page's console errors/warnings for the audit and logs tools.
	m.subscribeConsole(page)

	m.browser = b
	m.uiLauncher = l
	m.launchOpts = m.opts
	return page, nil
}

// resolveBin finds a browser executable: an explicit BinPath override, then
// launcher.LookPath over the OS-installed browsers, then — only when
// downloads are allowed — rod's pinned-revision Chromium placed under the CLI
// cache dir.
func (m *Manager) resolveBin() (string, error) {
	if m.opts.BinPath != "" {
		return m.opts.BinPath, nil
	}
	if bin, ok := launcher.LookPath(); ok {
		return bin, nil
	}
	if !m.opts.AllowDownload {
		return "", errors.New("no browser binary found (launcher.LookPath); install Chrome, Chromium, or Edge, or enable the pinned-browser download")
	}
	b := launcher.NewBrowser()
	b.RootDir = browserDownloadDir()
	b.Logger = nil // download progress goes nowhere; only the error surfaces
	bin, err := b.Get()
	if err != nil {
		return "", fmt.Errorf("download pinned browser into %s: %w", b.RootDir, err)
	}
	return bin, nil
}

// browserDownloadDir mirrors the CLI's other binary caches: GOTHIC_CLI_CACHE_DIR
// when set, else the OS user cache dir, always under a gothic-cli/browser
// subtree so the pinned-revision Chromium lives beside the Tailwind/TinyGo
// tool binaries. Rod's Browser.RootDir accepts any writable directory.
func browserDownloadDir() string {
	if dir := os.Getenv("GOTHIC_CLI_CACHE_DIR"); dir != "" {
		return filepath.Join(dir, "gothic-cli", "browser")
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "gothic-cli", "browser")
	}
	// No cache dir available; fall back to rod's own default location.
	return launcher.DefaultBrowserDir
}

// armIdleLocked schedules the idle close. Callers hold m.mu.
func (m *Manager) armIdleLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleAfter(), m.idleClose)
}

// idleClose runs from the timer goroutine: it tears the browser down and
// leaves the fields nil so the next call relaunches.
func (m *Manager) idleClose() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A Close (shutdown) or a fresh call in between wins over this stale fire.
	if m.closed || m.page == nil {
		return
	}
	m.closeBrowserLocked()
}

// closeBrowserLocked tears down the tab and the browser process. Rod is
// leakless, so even if a teardown step errors nothing is left running.
// Callers hold m.mu. On return the fields are nil and a later call relaunches.
func (m *Manager) closeBrowserLocked() {
	m.stopPinWatcherLocked()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	// Teardown uses a fresh short context: the session context may already be
	// cancelling when Close runs on the shutdown path.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if m.page != nil {
		_ = m.page.Context(ctx).Close()
	}
	if m.browser != nil {
		if err := m.browser.Context(ctx).Close(); err != nil {
			// The CDP connection may already be gone; make sure the process
			// itself is gone before giving up.
			if m.uiLauncher != nil {
				m.uiLauncher.Kill()
			}
		}
	}
	m.page = nil
	m.browser = nil
	m.uiLauncher = nil
}

// Close tears the browser down and marks the manager unusable. Safe to call
// more than once; concurrent calls serialize on the same mutex.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.closeBrowserLocked()
}

// EnsureStarted launches the browser and tab without navigating anywhere.
// The dev command calls it at session start so the first tool call never
// pays the launch latency.
func (m *Manager) EnsureStarted() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.pageLocked()
	if err != nil {
		return err
	}
	m.armIdleLocked()
	return nil
}

// Running reports whether the browser is currently up.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.page != nil
}

// SetViewport pins the agent's configuration until ClearViewport or the
// maintainer's own window resize (see pinWatch), with two mechanisms:
//
//   - headful + non-mobile: the REAL WINDOW is resized until its real content
//     area equals the requested pair — converging feedback, correct on any
//     scale factor; no emulation geometry is involved, so the page IS the
//     window and dead space is impossible.
//   - headless or mobile: CDP's device-metrics override emulates the pair
//     (headless has no window; mobile needs the DPR/UA simulation), and the
//     headful mobile window is sized to host the canvas.
//
// A non-positive height keeps the current pair; CDP emulation resets both
// dimensions at once, so the pin resolves an explicit height.
func (m *Manager) SetViewport(width, height int, mobile bool) error {
	if width <= 0 {
		return errors.New("set viewport: width must be a positive pixel count")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return err
	}

	// Headful desktop pin: the ONLY mechanism is a real window resize.
	if !m.opts.Headless && !mobile {
		if height <= 0 {
			inner, err := realDims(page)
			if err != nil {
				return err
			}
			height = int(inner[1])
		}
		m.pinned = &proto.EmulationSetDeviceMetricsOverride{Width: width, Height: height}
		m.pinnedEmulates = false
		if err := m.pinWindowTo(page, width, height); err != nil {
			m.pinned = nil
			return err
		}
		m.stopPinWatcherLocked()
		m.startPinWatchLocked()
		m.armIdleLocked()
		return nil
	}

	// Emulated pin (headless, or headful mobile): CDP device-metrics override.
	if height <= 0 {
		dims, err := viewportDims(page)
		if err != nil {
			return err
		}
		height = int(dims[1])
	}
	ovr := proto.EmulationSetDeviceMetricsOverride{
		Width:             width,
		Height:            height,
		DeviceScaleFactor: 1,
		Mobile:            mobile,
	}
	// Headful mobile: resize the REAL window FIRST, with no emulation armed —
	// the feedback loop reads the honest content area and lands the window as
	// close to (width, height) as the window manager allows (WMs enforce a
	// minimum width, so a 390px canvas may leave a strip). Only then arm the
	// override, and paint the webview background with the app's own body
	// colour — the leftover strip then blends into the app instead of reading
	// as a broken gray area.
	if !m.opts.Headless {
		m.frameOverhead = measureFrameOverhead(page)
		if err := m.pinWindowTo(page, width, height); err != nil {
			return err
		}
	}
	if err := ovr.Call(page); err != nil {
		return fmt.Errorf("pin viewport %dx%d: %w", width, height, err)
	}
	m.pinned = &ovr
	m.pinnedEmulates = true
	if !m.opts.Headless {
		m.matchWebviewBackground(page)
	}
	// Restart the user-resize watcher with a fresh baseline, so the window
	// moves it just made are not mistaken for the maintainer's hands.
	m.stopPinWatcherLocked()
	m.startPinWatchLocked()
	m.armIdleLocked()
	return nil
}

// ClearViewport lifts a pinned override and returns the browser window to its
// maximized state — the headful default. With no pin and no running page it is
// a clean no-op; with a pin but no running page the pin is dropped so a later
// launch starts unpinned.
func (m *Manager) ClearViewport() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopPinWatcherLocked()
	if m.page != nil {
		// SetViewport(nil) issues Emulation.clearDeviceMetricsOverride.
		if err := m.page.SetViewport(nil); err != nil {
			return fmt.Errorf("clear viewport: %w", err)
		}
		// Headful default: the window goes back to maximized (its own real
		// viewport), so manual resizing by the maintainer is live again.
		if !m.opts.Headless {
			if info, err := (proto.BrowserGetWindowForTarget{}).Call(m.page); err == nil {
				_ = (proto.BrowserSetWindowBounds{
					WindowID: info.WindowID,
					Bounds:   &proto.BrowserBounds{WindowState: proto.BrowserWindowStateMaximized},
				}).Call(m.page)
			}
		}
		m.armIdleLocked()
	}
	m.pinned = nil
	return nil
}

// pinResizePollInterval is how often the pinned-viewport watcher reads the
// window's outer size to detect a maintainer's manual resize. Package-level
// so tests can shorten it.
var pinResizePollInterval = 400 * time.Millisecond

// pinResizePollLocked returns the watcher tick, honoring the test override.
func (m *Manager) pinResizePoll() time.Duration {
	if m.opts.PinResizePoll > 0 {
		return m.opts.PinResizePoll
	}
	return pinResizePollInterval
}

// startPinWatchLocked, called after the pin is armed (headful only), starts
// the watcher that keeps the MAINTAINER in control: whenever the real window
// is resized by hand (drag handle, snap, restore, maximize), the emulation is
// lifted so the page mirrors the window again. The device-metrics canvas is
// otherwise frozen at the pinned size and reads as a broken, non-responsive
// app. The window's own size is never touched by the watcher — the
// maintainer's geometry stands. The watcher also clears an UNTRACKED override
// (a capture error path or a foreign tool can leave one armed while
// m.pinned is nil): clearing an override that isn't armed is a harmless no-op.
// Callers hold m.mu. When the pin is armed and nothing changed, the watcher
// exits silently at pin clear, teardown, or manager close.
func (m *Manager) startPinWatchLocked() {
	if m.opts.Headless || m.pinWatchStop != nil || m.page == nil {
		return
	}
	stop := make(chan struct{})
	m.pinWatchStop = stop
	page := m.page
	poll := m.pinResizePoll()
	go func() {
		// Settle first: after a programmatic unmaximize the WM (e.g. GNOME
		// restore-tiling, snap decisions) keeps adjusting the frame for a few
		// hundred ms. A baseline taken during that dance would read as the
		// maintainer's hands and lift the pin spuriously — adopt the baseline
		// only once the outer size is stable across the settle tick, bounded
		// by the budget.
		// settle tick AND budget scale with the poll interval so tests
		// (60ms poll) settle quickly while production (400ms) waits out the
		// window-manager's restore-tiling dance.
		settle := time.NewTicker(poll)
		defer settle.Stop()
		budget := time.Now().Add(8 * poll)
		last, ok := m.watcherOuterDims(page)
		if !ok {
			return
		}
		for time.Now().Before(budget) {
			select {
			case <-stop:
				return
			case <-settle.C:
			}
			cur, ok := m.watcherOuterDims(page)
			if !ok {
				return
			}
			if cur[0] == last[0] && cur[1] == last[1] {
				break
			}
			last = cur
		}
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			m.mu.Lock()
			if m.pinned == nil || m.page != page || m.closed {
				m.mu.Unlock()
				return
			}
			dims, err := outerDims(page)
			if err == nil && (dims[0] != last[0] || dims[1] != last[1]) {
				// The maintainer moved the window: lift the emulation —
				// even the untracked-armed case, since clearing a clear is
				// a no-op. The window keeps its new geometry. A fresh
				// measurement of the frame overhead is honest from here:
				// the override for our pin is now (or just) gone.
				last = dims
				_ = page.SetViewport(nil)
				m.pinned = nil
				m.frameOverhead = measureFrameOverhead(page)
				m.stopPinWatcherLocked()
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
		}
	}()
}

// watcherOuterDims reads the window's outer size for the watcher. ok=false
// ends the watcher contract (bad read, different page, pin gone, closed).
func (m *Manager) watcherOuterDims(page *rod.Page) ([2]int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pinned == nil || m.page != page || m.closed {
		return [2]int{}, false
	}
	dims, err := outerDims(page)
	if err != nil {
		return [2]int{}, false
	}
	return dims, true
}

// stopPinWatcherLocked stops the user-resize watcher. Callers hold m.mu.
func (m *Manager) stopPinWatcherLocked() {
	if m.pinWatchStop != nil {
		close(m.pinWatchStop)
		m.pinWatchStop = nil
	}
}

// outerDims reads the window's REAL outer size — width and height only, so a
// window move without a resize does not count as maintainer action.
func outerDims(page *rod.Page) ([2]int, error) {
	res, err := page.Eval(`() => JSON.stringify({ow: outerWidth, oh: outerHeight})`)
	if err != nil {
		return [2]int{}, fmt.Errorf("read outer dims: %w", err)
	}
	var raw struct{ OW, OH float64 }
	if json.Unmarshal([]byte(res.Value.Str()), &raw) != nil {
		return [2]int{}, fmt.Errorf("parse outer dims")
	}
	return [2]int{int(raw.OW), int(raw.OH)}, nil
}

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// realDims reads the page's real innerWidth/innerHeight — honest only while
// no CDP device-metrics override is armed.
func realDims(page *rod.Page) ([2]float64, error) {
	res, err := page.Eval(`() => JSON.stringify({w: innerWidth, h: innerHeight})`)
	if err != nil {
		return [2]float64{}, fmt.Errorf("read real viewport: %w", err)
	}
	var raw struct{ W, H float64 }
	if json.Unmarshal([]byte(res.Value.Str()), &raw) != nil {
		return [2]float64{}, fmt.Errorf("parse real viewport")
	}
	return [2]float64{raw.W, raw.H}, nil
}

func (m *Manager) pinWindowTo(page *rod.Page, width, height int) error {
	ok := m.pinWindowFeedback(page, width, height)
	if ok {
		return nil
	}
	// The renderer sometimes desyncs from the native window on Linux WMs: the
	// window reports the requested bounds while the page layout (innerWidth/
	// innerHeight) stays frozen at an older size, which reads as "huge window,
	// tiny canvas". A forced relayout — a brief emulation cycle plus a reload
	// (the page re-inits at the current window) — resyncs it; then converge.
	_ = forceRelayout(page)
	_ = m.pinWindowFeedback(page, width, height)
	return nil
}

// pinWindowFeedback resizes the real window until its real content area equals
// (width, height) a pixel. A converging feedback loop, deliberately NOT
// arithmetic on measured frames: report units differ between displays
// (physical px outside, CSS px inside), scale factors differ, and window
// managers choreograph; each pass reads the POST-set state and corrects the
// remaining error, so any fixed gap (scale factor, frame units, WM margins)
// cancels algebraically. At most pinWindowMaxPasses passes; reported false
// when the passes end with the pair still off (the caller then relayouts).
// The window ends normal: maximize and an exact size contradict.
func (m *Manager) pinWindowFeedback(page *rod.Page, width, height int) bool {
	const maxPasses = 3
	for pass := 0; pass < maxPasses; pass++ {
		// Give the window manager a beat before each read so the POST-set
		// state is what this pass measures (the source of convergence).
		if pass > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		inner, err := realDims(page)
		if err != nil {
			return false
		}
		dw := width - int(inner[0])
		dh := height - int(inner[1])
		if pass > 0 && absInt(dw) <= 2 && absInt(dh) <= 2 {
			// Converged — or the WM refuses a finer step; stand pat.
			return true
		}
		info, err := (proto.BrowserGetWindowForTarget{}).Call(page)
		if err != nil {
			return false
		}
		newW := width
		newH := height
		if info.Bounds.Width != nil {
			newW = *info.Bounds.Width + dw
		}
		if info.Bounds.Height != nil {
			newH = *info.Bounds.Height + dh
		}
		// A negative correction (inner reads larger than the window bounds —
		// a report-unit mismatch mid-transition) degrades to the absolute
		// request; the next pass's feedback pass converges from there.
		if newW <= 0 {
			newW = width
		}
		if newH <= 0 {
			newH = height
		}
		// Restore from maximized first: a combined restore+size call is
		// ignored by some Chrome builds; the plain state set is not.
		if info.Bounds.WindowState != proto.BrowserWindowStateNormal {
			_ = (proto.BrowserSetWindowBounds{
				WindowID: info.WindowID,
				Bounds:   &proto.BrowserBounds{WindowState: proto.BrowserWindowStateNormal},
			}).Call(page)
			time.Sleep(150 * time.Millisecond)
		}
		bounds := &proto.BrowserBounds{Width: &newW, Height: &newH, WindowState: proto.BrowserWindowStateNormal}
		if err := (proto.BrowserSetWindowBounds{WindowID: info.WindowID, Bounds: bounds}).Call(page); err != nil {
			return false
		}
	}
	inner, err := realDims(page)
	if err != nil {
		return false
	}
	return absInt(width-int(inner[0])) <= 2 && absInt(height-int(inner[1])) <= 2
}

// matchWebviewBackground paints the webview's leftover area with the page's
// own body background colour: a mobile-emulation canvas narrower than the
// window's enforced minimum reads its remainder as Chrome's default gray —
// with the app's own colour there the strip disappears into the layout.
// Best effort: any failure just leaves Chrome's default.
func (m *Manager) matchWebviewBackground(page *rod.Page) {
	res, err := page.Eval(`() => getComputedStyle(document.body).backgroundColor`)
	if err != nil {
		return
	}
	var css [4]float64
	if _, err := fmt.Sscanf(res.Value.Str(), "rgb(%f, %f, %f)", &css[0], &css[1], &css[2]); err != nil {
		if _, err := fmt.Sscanf(res.Value.Str(), "rgba(%f, %f, %f, %f)", &css[0], &css[1], &css[2], &css[3]); err != nil {
			return
		}
	}
	a := 255.0
	color := &proto.DOMRGBA{
		R: int(css[0]), G: int(css[1]), B: int(css[2]), A: &a,
	}
	_ = (proto.EmulationSetDefaultBackgroundColorOverride{Color: color}).Call(page)
}

// forceRelayout kicks the renderer into re-laying out at the window's current
// native size: a brief device-metrics cycle plus a reload — both cheap, and
// the reload re-initializes the page at the CURRENT window (a Linux-Chrome
// desync leaves the layout frozen at an older window otherwise; see
// pinWindowTo's rationale).
func forceRelayout(page *rod.Page) error {
	ovr := proto.EmulationSetDeviceMetricsOverride{DeviceScaleFactor: 1}
	if err := ovr.Call(page); err != nil {
		return err
	}
	_ = page.SetViewport(nil)
	return page.Reload()
}

// frameOverhead is the window chrome cost measured at launch (or right before
// a pin): title/URL/tab bars measured in CSS px, plus the window's
// devicePixelRatio so the pin math can land DEVICE-px outer bounds (Chrome
// reports outerWidth in physical px and innerWidth in CSS px on scaled
// displays — the mismatch that made unscaled math fail on 2× setups). DPR 0
// means the measurement failed.
type frameOverhead struct {
	W, H int
	DPR  float64
}

func (f frameOverhead) valid() bool { return f.DPR > 0 && (f.W > 0 || f.H > 0) }

// measureFrameOverhead reads the live window's frame cost (outer minus
// content) in CSS px and the window's devicePixelRatio — honest numbers only
// while no device-metrics override is armed (inner dims then report the real
// content area). Zero frameOverhead when any read fails or the numbers are
// implausible; the caller then falls back to the rough estimator.
func measureFrameOverhead(page *rod.Page) frameOverhead {
	res, err := page.Eval(`() => JSON.stringify({ow: outerWidth, iw: innerWidth, oh: outerHeight, ih: innerHeight, dpr: window.devicePixelRatio})`)
	if err != nil {
		return frameOverhead{}
	}
	var raw struct{ OW, IW, OH, IH, DPR float64 }
	if json.Unmarshal([]byte(res.Value.Str()), &raw) != nil {
		return frameOverhead{}
	}
	if raw.DPR <= 0 {
		raw.DPR = 1
	}
	// Normalize the outer (physical) dims to CSS px for the frame math.
	ow := int(raw.OW / raw.DPR)
	oh := int(raw.OH / raw.DPR)
	iw, ih := int(raw.IW), int(raw.IH)
	if ow <= iw || oh <= ih || iw <= 0 || ih <= 0 {
		return frameOverhead{}
	}
	return frameOverhead{W: ow - iw, H: oh - ih, DPR: raw.DPR}
}
