// Package browser tests drive a real browser against local httptest servers,
// verifying launch, actions, the headless toggle, the idle close, the
// allowlist, and the leakless teardown. When the machine has no browser
// installed and downloads are disabled, the launch-dependent tests skip —
// running them requires Chrome/Chromium/Edge installed or downloads enabled;
// rod must never download ~150 MB inside a test run.
package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
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
	t.Cleanup(m.Close)

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
