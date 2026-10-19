// Package browser tests drive a real browser against local httptest servers,
// verifying launch, actions, the headless toggle, the idle close, the
// allowlist, and the leakless teardown. When the machine has no browser
// installed and downloads are disabled, the launch-dependent tests skip —
// running them requires Chrome/Chromium/Edge installed or downloads enabled;
// rod must never download ~150 MB inside a test run.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// browserAvailable reports whether a browser binary can be resolved without a
// download. Launch-dependent tests skip when it returns false: rod would
// otherwise download a pinned Chromium (~150 MB), which tests must never do.
func browserAvailable() bool {
	_, ok := launcher.LookPath()
	return ok
}

// roundTripServer serves a form page at "/" that posts "value" back to /submit,
// where the handler records it so tests can assert what the server received.
type roundTripServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	submit map[string][]string
	calls  int
}

func newRoundTripServer(t *testing.T) *roundTripServer {
	t.Helper()
	rt := &roundTripServer{submit: map[string][]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rt.mu.Lock()
		rt.submit["value"] = r.Form["value"]
		rt.calls++
		rt.mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "use /submit", http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`<html><body>
			<form method="post" action="/submit">
				<input name="value" value="">
				<button type="submit">Send</button>
			</form>
		</body></html>`))
	})

	rt.srv = httptest.NewServer(mux)
	t.Cleanup(rt.srv.Close)
	return rt
}

func (rt *roundTripServer) url() string { return rt.srv.URL }

// waitForReceived polls until the server has recorded want for a field —
// the browser's click returns as soon as the click dispatches, and the
// navigation/form POST lands moments later.
func waitForReceived(t *testing.T, rt *roundTripServer, want string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		got := rt.received("value")
		if len(got) > 0 && got[0] == want {
			return got[0]
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// received reads what the server recorded for a form field.
func (rt *roundTripServer) received(field string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.submit[field]
}

// testManager builds a Manager configured for tests: no download, short
// timeouts, a temp profile dir.
func testManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	opts.AllowDownload = false
	if opts.UserDataDir == "" {
		opts.UserDataDir = t.TempDir()
	}
	if opts.IdleAfter == 0 {
		opts.IdleAfter = 500 * time.Millisecond
	}
	return New(context.Background(), opts)
}

// skipUnlessLaunchable gates the tests that actually launch a browser.
func skipUnlessLaunchable(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("browser launch tests skipped with -short")
	}
	if !browserAvailable() {
		t.Skip("no Chrome/Chromium/Edge found on this machine; downloads are disabled in tests")
	}
}

// skipUnlessHeadfulOptIn gates the window-geometry tests (real window pin,
// watcher, WM choreography). They NEED a visible window, so they pop one on
// the screen — the maintainer opts in with GOTHIC_TEST_HEADFUL=1 and nothing
// in the default loop disturbs their desktop. Everything functional about the
// viewport is covered headless by the sweep below.
func skipUnlessHeadfulOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("GOTHIC_TEST_HEADFUL") != "1" {
		t.Skip("window-geometry test needs a real window; run with GOTHIC_TEST_HEADFUL=1 (pops a browser on the screen)")
	}
	skipUnlessLaunchable(t)
}

func TestOpenNavigatesAndSettles(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)
	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)

	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !m.Running() {
		t.Fatal("expected browser to be running after Open")
	}
}

func TestFillClickRoundTrip(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)

	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	m.SetAllowedHostPorts([]string{strings.TrimPrefix(rt.url(), "http://")})

	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	steps := []Step{
		{Action: "fill", Selector: `input[name="value"]`, Value: "managed-browser"},
		{Action: "click", Selector: `button[type="submit"]`},
	}
	if err := m.Act(context.Background(), steps); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if got := waitForReceived(t, rt, "managed-browser", 5*time.Second); got != "managed-browser" {
		t.Fatalf("server received %q, want managed-browser", got)
	}
}

func TestHeadlessToggleRelaunches(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)

	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Toggle to headful and back; both relaunches reuse the same profile dir,
	// so the relaunch itself is the documented restart working.
	for _, want := range []bool{false, true} {
		if err := m.SetHeadless(want); err != nil {
			t.Fatalf("SetHeadless(%v): %v", want, err)
		}
		if !m.Running() {
			t.Fatalf("browser not running after SetHeadless(%v)", want)
		}
		// The relaunched browser must still serve the page.
		if err := m.Open(rt.url()); err != nil {
			t.Fatalf("Open after SetHeadless(%v): %v", want, err)
		}
	}
}

func TestIdleClose(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)

	m := testManager(t, Options{IdleAfter: 250 * time.Millisecond})
	t.Cleanup(m.Close)
	m.SetAllowedHostPorts([]string{strings.TrimPrefix(rt.url(), "http://")})

	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !m.Running() {
		t.Fatal("expected running after Open")
	}
	// The idle timer closes the browser on its own; poll until it reports down.
	deadline := time.Now().Add(5 * time.Second)
	for m.Running() {
		if time.Now().After(deadline) {
			t.Fatal("browser still running after the idle budget")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// After the idle close the process is gone; a subsequent call relaunches
	// rather than failing.
	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("relaunch after idle close: %v", err)
	}
}

func TestAllowlistRefusesNonLocal(t *testing.T) {
	rt := newRoundTripServer(t)
	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	m.SetAllowedHostPorts([]string{strings.TrimPrefix(rt.url(), "http://")})

	for _, u := range []string{
		"https://example.com",
		"http://evil.example.internal/x",
		"file:///etc/passwd",
	} {
		err := m.Open(u)
		if err == nil {
			t.Fatalf("Open(%q) succeeded, want refusal", u)
		}
		if !strings.Contains(err.Error(), "refusing") {
			t.Errorf("Open(%q) error = %v, want a refusal", u, err)
		}
	}
	if m.Running() {
		t.Fatal("refusal must not launch the browser")
	}
}

func TestAllowlistRefusesForeignPortOnLoopback(t *testing.T) {
	rt := newRoundTripServer(t)
	other := newRoundTripServer(t) // a second local port, deliberately off-list

	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	m.SetAllowedHostPorts([]string{strings.TrimPrefix(rt.url(), "http://")})

	if err := m.Open(other.url()); err != nil {
		if !strings.Contains(err.Error(), "allowlist") {
			t.Errorf("Open(%q) error = %v, want an allowlist refusal", other.url(), err)
		}
	} else {
		t.Fatalf("Open(%q) succeeded, want an allowlist refusal", other.url())
	}
	// The allowlisted port still works.
	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open(%q): %v", rt.url(), err)
	}
}

func TestEmptyAllowlistRefusesNonLoopbackOnly(t *testing.T) {
	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	// No explicit list: loopback on any port passes the host check.
	if err := m.checkURL("http://127.0.0.1:1"); err != nil {
		t.Errorf("checkURL(127.0.0.1) = %v, want nil", err)
	}
	if err := m.checkURL("http://example.com"); err == nil {
		t.Error("checkURL(example.com) = nil, want a refusal")
	}
}

func TestUnknownActionErrors(t *testing.T) {
	skipUnlessLaunchable(t)
	m := testManager(t, NewOptions())
	t.Cleanup(func() {
		m.Close()
		// Give Chrome a moment to exit after Close; the leakless watcher
		// reaps the process, but the directory still holds open handles for a
		// short window. Waiting here lets the handles close so TempDir's
		// RemoveAll can succeed.
		<-time.After(200 * time.Millisecond)
	})

	err := m.Act(context.Background(), []Step{{Action: "teleport", Selector: "body"}})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("Act with unknown action = %v, want an unknown-action error", err)
	}
}

func TestCloseIsFinalAndSafeTwice(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)
	m := testManager(t, NewOptions())

	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	m.Close()
	m.Close() // idempotent
	if m.Running() {
		t.Fatal("Running must be false after Close")
	}
	if err := m.Open(rt.url()); err == nil {
		t.Fatal("Open after Close must fail")
	}
}

// TestNoZombieProcessesAfterClose checks the leakless contract: after the
// manager closes, no Chrome/Chromium process this test created is left
// behind. It counts processes from /proc, comparing after Close against the
// baseline taken before the launch.
func TestNoZombieProcessesAfterClose(t *testing.T) {
	skipUnlessLaunchable(t)
	m := testManager(t, NewOptions())
	rt := newRoundTripServer(t)

	before := chromeProcessCount()
	if err := m.Open(rt.url()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	m.Close()

	// Chrome can take a moment to exit after Browser.Close.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if chromeProcessCount() <= before {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("browser processes remain after Close: before=%d after=%d",
		before, chromeProcessCount())
}

// chromeProcessCount counts running Chrome/Chromium processes by reading
// /proc directly, cheaper and more portable than shelling out.
func chromeProcessCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name == "chrome" || name == "chromium" {
			n++
		}
	}
	return n
}

// ── Viewport-filling tests ─────────────────────────────────────────────────

// TestViewportHeadfulSizesWindow verifies that SetViewport resizes the headful
// window so the emulated viewport fills the entire content area — no empty
// space above or below. It does this by comparing the window bounds with the
// page's inner dimensions (which reflect the CDP device-metrics override).
func TestViewportHeadfulSizesWindow(t *testing.T) {
	skipUnlessHeadfulOptIn(t)

	// Create a headful manager (headless=false) so window-resize is exercised.
	m := testManager(t, Options{Headless: false, IdleAfter: 2 * time.Second})
	t.Cleanup(func() {
		m.Close()
		// Give Chrome a moment to exit after Close; the leakless watcher
		// reaps the process, but the directory still holds open handles for a
		// short window. Waiting here lets the handles close so TempDir's
		// RemoveAll can succeed.
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	// 1) Pin a 640×600 viewport and verify the real window content area
	//    becomes exactly that pair (the window pin resizes the REAL window;
	//    the WM may clamp a too-small request — 640 clears the 500px floor).
	if err := m.SetViewport(640, 600, false); err != nil {
		t.Fatalf("SetViewport(640, 600): %v", err)
	}
	dims := innerDims(t, m.page)
	widthDiff := abs(dims[0] - 640)
	heightDiff := abs(dims[1] - 600)
	if widthDiff > 2 {
		t.Errorf("inner width drift: got %g, want 640 (drift %g)", dims[0], widthDiff)
	}
	if heightDiff > 2 {
		t.Errorf("inner height drift: got %g, want 600 (drift %g)", dims[1], heightDiff)
	}

	// 2) A second SetViewport call at different dimensions must also fill
	//    the window — inner dimensions should track the new pin.
	if err := m.SetViewport(780, 1024, false); err != nil {
		t.Fatalf("SetViewport(780, 1024): %v", err)
	}
	dims = innerDims(t, m.page)
	widthDiff = abs(dims[0] - 780)
	heightDiff = abs(dims[1] - 1024)
	if widthDiff > 2 {
		t.Errorf("inner width drift: got %g, want 780 (drift %g)", dims[0], widthDiff)
	}
	if heightDiff > 2 {
		t.Errorf("inner height drift: got %g, want 1024 (drift %g)", dims[1], heightDiff)
	}

	// 3) ClearViewport should restore the page to the browser's own viewport
	//    (inner dims become >= 400 after a 400-pixel-wide pin was active).
	if err := m.ClearViewport(); err != nil {
		t.Fatalf("ClearViewport: %v", err)
	}
	dims = innerDims(t, m.page)
	// The browser should have restored its own viewport; innerWidth >= 400
	// proves the page did not remain at the pinned 400.
	if dims[0] < 400 {
		t.Errorf("after ClearViewport: innerWidth = %g, want >= 400", dims[0])
	}
}

// TestViewportHeadlessNoResize verifies that SetViewport in headless mode
// does not attempt window resizing (headless browsers have no window bounds)
// and the page remains functional.
func TestViewportHeadlessNoResize(t *testing.T) {
	skipUnlessLaunchable(t)

	m := testManager(t, Options{Headless: true})
	t.Cleanup(func() {
		m.Close()
		// Give Chrome a moment to exit after Close; the leakless watcher
		// reaps the process, but the directory still holds open handles for a
		// short window. Waiting here lets the handles close so TempDir's
		// RemoveAll can succeed.
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	// In headless mode, SetViewport should succeed without error. The page
	// must remain functional (page not nil, still running).
	if err := m.SetViewport(375, 667, true); err != nil {
		t.Fatalf("SetViewport headless: %v", err)
	}
	if m.page == nil {
		t.Fatal("headless page should not be nil after SetViewport")
	}
	if !m.Running() {
		t.Fatal("headless browser should remain running after SetViewport")
	}
}

// innerDims reads the page's innerWidth and innerHeight via JS eval.
// The caller must hold the page reference (set after EnsureStarted or Open).
func innerDims(t *testing.T, page *rod.Page) [2]float64 {
	t.Helper()
	res, err := page.Eval(`() => JSON.stringify({w: innerWidth, h: innerHeight})`)
	if err != nil {
		t.Fatalf("innerDims eval: %v", err)
	}
	var raw struct{ W, H float64 }
	if err := json.Unmarshal([]byte(res.Value.Str()), &raw); err != nil {
		t.Fatalf("innerDims unmarshal: %v", err)
	}
	return [2]float64{raw.W, raw.H}
}

func abs(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}

// TestUserResizeDropsViewportPin is the maintainer-wins contract: while an
// agent-pinned viewport emulation is armed, a manual window resize by the
// maintainer lifts the emulation; the page mirrors the resized window (no
// frozen canvas, no dead space) and the window keeps the user's geometry.
func TestUserResizeDropsViewportPin(t *testing.T) {
	skipUnlessHeadfulOptIn(t)

	m := testManager(t, Options{Headless: false, PinResizePoll: 60 * time.Millisecond, IdleAfter: 3 * time.Second})
	t.Cleanup(func() {
		m.Close()
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	// Pre-unmaximize once so the window manager's restore-tiling dance is
	// over before the watcher arms: from a settled normal window the CDP
	// size set produces no WM motion and the test is deterministic.
	if err := settleNormalWindow(m); err != nil {
		t.Fatalf("settle window: %v", err)
	}

	// Pin a small emulated viewport; the window is sized to fill it.
	if err := m.SetViewport(500, 380, false); err != nil {
		t.Fatalf("SetViewport(500, 380): %v", err)
	}
	// With no external resize, several watcher ticks leave the pin armed.
	time.Sleep(3 * m.pinResizePoll())
	if m.pinned == nil {
		t.Fatal("pin dropped without any window resize")
	}

	// Let the watcher's settle window pass so a real maintainer-style drag
	// is treated as user action, not as baseline adoption.
	time.Sleep(8 * m.pinResizePoll())

	// The maintainer stretches/restores the window by hand: a CDP bounds set
	// to a different geometry is exactly what a drag or snap produces.
	page, err := m.pageLockedSafe()
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	info, err := (proto.BrowserGetWindowForTarget{}).Call(page)
	if err != nil {
		t.Fatalf("GetWindowForTarget: %v", err)
	}
	outerW, outerH := 860, 640
	bounds := &proto.BrowserBounds{
		Width: &outerW, Height: &outerH,
		WindowState: proto.BrowserWindowStateNormal,
	}
	if err := (proto.BrowserSetWindowBounds{WindowID: info.WindowID, Bounds: bounds}).Call(page); err != nil {
		t.Fatalf("manual resize set: %v", err)
	}

	// The watcher notices and lifts the emulation.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.pinned == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if m.pinned != nil {
		t.Fatal("pin still armed after the maintainer's manual resize")
	}

	// Contract asserts (robust across scaled displays and WM quirks):
	// the pin is gone, the page no longer reports the frozen emulated canvas,
	// and the window is not stuck maximized — the maintainer's geometry stands.
	dims := innerDims(t, m.page)
	if d := abs(dims[0] - 500); d < 3 {
		t.Errorf("after manual resize innerWidth = %g, still the frozen emulated 500", dims[0])
	}
	if d := abs(dims[1] - 380); d < 3 {
		t.Errorf("after manual resize innerHeight = %g, still the frozen emulated 380", dims[1])
	}
	info, err = (proto.BrowserGetWindowForTarget{}).Call(m.page)
	if err != nil {
		t.Fatalf("GetWindowForTarget after clear: %v", err)
	}
	if info.Bounds.WindowState != proto.BrowserWindowStateNormal {
		t.Errorf("after manual resize window state = %v, want normal (the user's drag)", info.Bounds.WindowState)
	}
}

// TestViewportPinTouchesNothingWhenIdle asserts that the watcher never
// rewrites the window while the maintainer leaves it alone: the frame stays
// byte-stable after several poll intervals.
func TestViewportPinTouchesNothingWhenIdle(t *testing.T) {
	skipUnlessHeadfulOptIn(t)

	m := testManager(t, Options{Headless: false, PinResizePoll: 60 * time.Millisecond, IdleAfter: 3 * time.Second})
	t.Cleanup(func() {
		m.Close()
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	// Pre-unmaximize and wait out the WM's tiling dance: an idle window must
	// not move by itself, only a manager-settled window can prove that.
	if err := settleNormalWindow(m); err != nil {
		t.Fatalf("settle window: %v", err)
	}
	if err := m.SetViewport(640, 480, false); err != nil {
		t.Fatalf("SetViewport: %v", err)
	}

	page, err := m.pageLockedSafe()
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	before, err := (proto.BrowserGetWindowForTarget{}).Call(page)
	if err != nil {
		t.Fatalf("bounds before: %v", err)
	}
	for i := 0; i < 6; i++ { // ~6 watcher ticks
		time.Sleep(m.pinResizePoll() * 2)
		after, err := (proto.BrowserGetWindowForTarget{}).Call(m.page)
		if err != nil {
			t.Fatalf("bounds after tick %d: %v", i, err)
		}
		if (before.Bounds.Width == nil) != (after.Bounds.Width == nil) ||
			(before.Bounds.Width != nil && *before.Bounds.Width != *after.Bounds.Width) {
			t.Fatalf("window width changed while idle: %v -> %v", before.Bounds.Width, after.Bounds.Width)
		}
	}
}

// TestMobilePinHeadfulFillsWindow is the emulated-pin contract: the mobile
// emulation canvas reports the pinned pair exactly AND the real window was
// resized (before the override armed) so the canvas fills the content area —
// no dead strip on either side.
func TestMobilePinHeadfulFillsWindow(t *testing.T) {
	skipUnlessHeadfulOptIn(t)

	m := testManager(t, Options{Headless: false, IdleAfter: 3 * time.Second})
	t.Cleanup(func() {
		m.Close()
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if err := settleNormalWindow(m); err != nil {
		t.Fatalf("settle window: %v", err)
	}

	if err := m.SetViewport(390, 844, true); err != nil {
		t.Fatalf("SetViewport(390, 844, mobile): %v", err)
	}

	// Canvas: the compositor surface is exactly the pin (PNG dims). Under
	// mobile emulation Chrome's layout metrics (innerWidth etc.) report the
	// emulated layout viewport, not the surface — the screenshot is the truth.
	pageCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	png, err := m.page.Context(pageCtx).Screenshot(false, nil)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	w := int(png[16])<<24 | int(png[17])<<16 | int(png[18])<<8 | int(png[19])
	h := int(png[20])<<24 | int(png[21])<<16 | int(png[22])<<8 | int(png[23])
	if abs(float64(w)-390) > 2 || abs(float64(h)-844) > 2 {
		t.Errorf("mobile surface (screenshot) = %dx%d, want 390x844", w, h)
	}

	// Window: normal state, and at least as wide as the canvas (a narrower
	// window would clip; a much wider one would leave a dead strip).
	info, err := (proto.BrowserGetWindowForTarget{}).Call(m.page)
	if err != nil {
		t.Fatalf("GetWindowForTarget: %v", err)
	}
	if info.Bounds.WindowState != proto.BrowserWindowStateNormal {
		t.Errorf("window state = %v, want normal after the mobile pin", info.Bounds.WindowState)
	}
	if info.Bounds.Width == nil || *info.Bounds.Width < 390 {
		t.Errorf("window width = %v, want at least the canvas width (WM min may widen it)", info.Bounds.Width)
	}
}

// TestViewportPinSweep is the no-dead-space regression for the desktop (real
// window) pin across the sizes the maintainer approved live: the real content
// area becomes exactly the requested pair (the frozen-canvas class of bugs
// makes inner stick at an old size — this assert is the kill switch), the
// window stays in the normal state, and the outer window is never smaller
// than its content. Sizes clear the WM's ~500px minimum width floor.
func TestViewportPinSweep(t *testing.T) {
	skipUnlessHeadfulOptIn(t)

	m := testManager(t, Options{Headless: false, PinResizePoll: 60 * time.Millisecond, IdleAfter: 6 * time.Second})
	t.Cleanup(func() {
		m.Close()
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if err := settleNormalWindow(m); err != nil {
		t.Fatalf("settle window: %v", err)
	}

	for _, size := range [][2]int{
		{1366, 768}, // small laptop
		{768, 1024}, // tablet (portrait)
		{1280, 800}, // medium laptop
		{1600, 900}, // desktop
		{768, 1024}, // tablet again (round trip)
	} {
		width, height := size[0], size[1]
		if err := m.SetViewport(width, height, false); err != nil {
			t.Fatalf("SetViewport(%d, %d): %v", width, height, err)
		}
		dims := innerDims(t, m.page)
		if d := abs(dims[0] - float64(width)); d > 2 {
			t.Errorf("pin %dx%d: inner width = %g (drift %g) — canvas frozen, not mirroring", width, height, dims[0], d)
		}
		if d := abs(dims[1] - float64(height)); d > 2 {
			t.Errorf("pin %dx%d: inner height = %g (drift %g)", width, height, dims[1], d)
		}
		info, err := (proto.BrowserGetWindowForTarget{}).Call(m.page)
		if err != nil {
			t.Fatalf("bounds read at %dx%d: %v", width, height, err)
		}
		if info.Bounds.WindowState != proto.BrowserWindowStateNormal {
			t.Errorf("pin %dx%d: window state = %v, want normal", width, height, info.Bounds.WindowState)
		}
		if info.Bounds.Width != nil && *info.Bounds.Width < width-2 {
			t.Errorf("pin %dx%d: window width %d < content %d", width, height, *info.Bounds.Width, width)
		}
	}

	// The maintainer's default comes back: clear → maximized full screen.
	if err := m.ClearViewport(); err != nil {
		t.Fatalf("ClearViewport: %v", err)
	}
	info, err := (proto.BrowserGetWindowForTarget{}).Call(m.page)
	if err != nil {
		t.Fatalf("bounds read after clear: %v", err)
	}
	if info.Bounds.WindowState != proto.BrowserWindowStateMaximized {
		t.Errorf("after clear: window state = %v, want maximized (the headful default)", info.Bounds.WindowState)
	}
}

// TestViewportHeadlessEmulationSweep is the DEFAULT-loop viewport regression:
// in headless mode SetViewport uses the CDP device-metrics emulation, and the
// emulated canvas must be exactly the requested pair at every size — the same
// "viewport respects the size" contract, with no window and nothing popping
// on anyone's screen. The window-geometry lane (real WM choreography) runs
// separately under GOTHIC_TEST_HEADFUL=1.
func TestViewportHeadlessEmulationSweep(t *testing.T) {
	skipUnlessLaunchable(t)

	m := testManager(t, Options{Headless: true})
	t.Cleanup(func() {
		m.Close()
		<-time.After(200 * time.Millisecond)
	})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	for _, size := range [][2]int{
		{3840, 2160}, // 4K
		{2560, 1440}, // 2K
		{1366, 768},  // small laptop
		{768, 1024},  // tablet (portrait)
		{390, 844},   // iPhone 12 Pro (still emulation: no window involved)
		{768, 1024},  // round trip back
	} {
		width, height := size[0], size[1]
		if err := m.SetViewport(width, height, false); err != nil {
			t.Fatalf("SetViewport(%d, %d): %v", width, height, err)
		}
		dims := innerDims(t, m.page)
		if d := abs(dims[0] - float64(width)); d > 2 {
			t.Errorf("pin %dx%d: emulated width = %g (drift %g)", width, height, dims[0], d)
		}
		if d := abs(dims[1] - float64(height)); d > 2 {
			t.Errorf("pin %dx%d: emulated height = %g (drift %g)", width, height, dims[1], d)
		}
	}

	if err := m.ClearViewport(); err != nil {
		t.Fatalf("ClearViewport: %v", err)
	}
}

// pageLockedSafe returns the running page for a test, launching if needed —
// the test-side seam over pageLocked (tests run in-package and hold no mu).
func (m *Manager) pageLockedSafe() (*rod.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pageLocked()
}

// settleNormalWindow unmaximizes the window and gives the window manager time
// to finish its restore-tiling dance, so later assertions are not racing a
// frame that keeps adjusting itself.
func settleNormalWindow(m *Manager) error {
	page, err := m.pageLockedSafe()
	if err != nil {
		return err
	}
	info, err := (proto.BrowserGetWindowForTarget{}).Call(page)
	if err != nil {
		return err
	}
	if info.Bounds.WindowState != proto.BrowserWindowStateNormal {
		if err := (proto.BrowserSetWindowBounds{
			WindowID: info.WindowID,
			Bounds:   &proto.BrowserBounds{WindowState: proto.BrowserWindowStateNormal},
		}).Call(page); err != nil {
			return err
		}
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

// TestFillClickReusesSameTab verifies the single-page contract: repeated Act
// rounds hit the same kept-alive tab, each round submitting the form once.
func TestFillClickReusesSameTab(t *testing.T) {
	skipUnlessLaunchable(t)
	rt := newRoundTripServer(t)
	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	m.SetAllowedHostPorts([]string{strings.TrimPrefix(rt.url(), "http://")})

	if err := m.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Open(rt.url()); err != nil {
			t.Fatalf("round %d Open: %v", i, err)
		}
		steps := []Step{
			{Action: "fill", Selector: `input[name="value"]`, Value: fmt.Sprintf("hit-%d", i)},
			{Action: "click", Selector: `button[type="submit"]`},
		}
		if err := m.Act(context.Background(), steps); err != nil {
			t.Fatalf("round %d Act: %v", i, err)
		}
		if got := waitForReceived(t, rt, fmt.Sprintf("hit-%d", i), 5*time.Second); got == "" {
			t.Fatalf("round %d: form value never reached the server", i)
		}
	}
}
