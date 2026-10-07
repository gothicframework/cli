package browser

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestConsoleRingRetainsErrors drives a real browser: the fixture page logs
// an error, a warning, and a plain log on load; the Manager's ring must keep
// the error and the warning only, newest first.
func TestConsoleRingRetainsErrors(t *testing.T) {
	skipUnlessLaunchable(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><script>
			console.error("console-test-error");
			console.warn("console-test-warning");
			console.log("console-test-log");
		</script></body></html>`))
	}))
	t.Cleanup(srv.Close)

	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)

	if err := m.Open(srv.URL); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The events ride rod's event channel; give them a short settle window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(m.Console(0)) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console ring empty after %s; got %v", 5*time.Second, m.Console(0))
		}
		time.Sleep(100 * time.Millisecond)
	}

	entries := m.Console(0)
	var texts []string
	for _, e := range entries {
		if e.Type != "error" && e.Type != "warning" {
			t.Errorf("ring keeps a non-error/warning type %q: %+v", e.Type, entries)
		}
		texts = append(texts, e.Text)
	}
	joined := strings.Join(texts, " ")
	if !strings.Contains(joined, "console-test-error") || !strings.Contains(joined, "console-test-warning") {
		t.Errorf("ring missing the fixture messages: %v", entries)
	}
	if strings.Contains(joined, "console-test-log") {
		t.Errorf("the plain log line must not be retained: %v", entries)
	}
	// last bounds the read; newest first, so last=1 is the most recent.
	if got := m.Console(1); len(got) != 1 {
		t.Errorf("Console(1) = %d entries, want 1", len(got))
	}
}
