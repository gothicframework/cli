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
	// re-applied on every relaunch; ClearViewport lifts it. restoreBounds
	// holds the headful window's pre-pin geometry so the clear path hands
	// the window back the way the maintainer had it. frameOverhead is the
	// launch-time window-frame cost (outer minus content), measured while
	// no override is armed, so a pin can size the outer window exactly.
	pinned        *proto.EmulationSetDeviceMetricsOverride
	restoreBounds *proto.BrowserBounds
	frameOverhead [2]int

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
		_ = m.pinned.Call(page)
		// The headful window that just relaunched also gets the pin's
		// geometry, so the emulation fills the window again.
		m.matchWindowToViewport(page, m.pinned.Width, m.pinned.Height)
	} else {
		// Measure the window-frame overhead (outer size minus the content
		// area) on a clean, unpinned launch: once a device-metrics override
		// is armed, innerHeight reports the EMULATED height, not the real
		// content area, so a pin-time measurement can never converge. The
		// overhead is what lets a later pin size the outer window exactly.
		m.frameOverhead = measureFrameOverhead(page)
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

// SetViewport pins a device-metrics override on the managed page until
// ClearViewport: the agent keeps the browser in one configuration (say a
// 375px mobile viewport) across navigations, probes, and captures. CDP's
// override outlives navigations within the target; the manager re-applies it
// after any relaunch. A non-positive height keeps the page's current height
// (CDP replaces both dimensions at once, so the pin must carry an explicit
// pair).
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
	if err := ovr.Call(page); err != nil {
		return fmt.Errorf("pin viewport %dx%d: %w", width, height, err)
	}
	// Capture the pre-pin window geometry so ClearViewport can put the
	// maintainer's window back, then make the window fill the emulation.
	if m.pinned == nil && !m.opts.Headless {
		if info, err := (proto.BrowserGetWindowForTarget{}).Call(page); err == nil {
			m.restoreBounds = info.Bounds
		}
	}
	m.matchWindowToViewport(page, width, height)
	m.pinned = &ovr
	m.armIdleLocked()
	return nil
}

// matchWindowToViewport resizes a HEADFUL window so the emulated viewport
// fills it. CDP's device-metrics override renders the page at the pinned
// size inside whatever window exists; without this the maintainer sees the
// page canvas shrunk into a corner of a much larger window and empty space
// everywhere else — it reads as a broken layout but is emulation geometry.
// Outer sizing needs the window-frame overhead (title/URL bars sit outside
// the viewport); the launch path measures it while no override is armed,
// because with a pin active innerHeight reports the EMULATED height and can
// never drive a correction. Without a measurement the estimator keeps the
// pre-fix two-pass shape (best effort, can overshoot height).
func (m *Manager) matchWindowToViewport(page *rod.Page, width, height int) {
	if m.opts.Headless {
		return
	}
	var outerW, outerH int
	if m.frameOverhead[0] > 0 || m.frameOverhead[1] > 0 {
		outerW = width + m.frameOverhead[0]
		outerH = height + m.frameOverhead[1]
	} else {
		const initialFrameAllowance = 160
		outerW, outerH = width, height+initialFrameAllowance
	}
	info, err := (proto.BrowserGetWindowForTarget{}).Call(page)
	if err != nil {
		return
	}
	bounds := &proto.BrowserBounds{Width: &outerW, Height: &outerH, WindowState: proto.BrowserWindowStateNormal}
	_ = (proto.BrowserSetWindowBounds{WindowID: info.WindowID, Bounds: bounds}).Call(page)
}

// measureFrameOverhead reads the live window's outer size minus its content
// area — honest numbers only while no device-metrics override is armed (inner
// dims then report the real content area). nil when any read fails or the
// numbers are implausible; the caller then falls back to the rough estimator.
func measureFrameOverhead(page *rod.Page) [2]int {
	res, err := page.Eval(`() => JSON.stringify({ow: outerWidth, iw: innerWidth, oh: outerHeight, ih: innerHeight})`)
	if err != nil {
		return [2]int{}
	}
	var raw struct{ OW, IW, OH, IH float64 }
	if json.Unmarshal([]byte(res.Value.Str()), &raw) != nil {
		return [2]int{}
	}
	ow, iw := int(raw.OW), int(raw.IW)
	oh, ih := int(raw.OH), int(raw.IH)
	if ow <= iw || oh <= ih || iw <= 0 || ih <= 0 {
		return [2]int{}
	}
	return [2]int{ow - iw, oh - ih}
}

// ClearViewport lifts a pinned override and restores the browser's own window
// size. With no pin and no running page it is a clean no-op; with a pin but
// no running page the pin is dropped so a later launch starts unpinned.
func (m *Manager) ClearViewport() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.page != nil {
		// SetViewport(nil) issues Emulation.clearDeviceMetricsOverride.
		if err := m.page.SetViewport(nil); err != nil {
			return fmt.Errorf("clear viewport: %w", err)
		}
		// Headful sessions gave the window to the emulation (the pin
		// resized it); hand the maintainer's window geometry back.
		if !m.opts.Headless && m.restoreBounds != nil {
			if info, err := (proto.BrowserGetWindowForTarget{}).Call(m.page); err == nil {
				_ = (proto.BrowserSetWindowBounds{WindowID: info.WindowID, Bounds: m.restoreBounds}).Call(m.page)
			}
		}
		m.armIdleLocked()
	}
	m.pinned = nil
	m.restoreBounds = nil
	return nil
}
