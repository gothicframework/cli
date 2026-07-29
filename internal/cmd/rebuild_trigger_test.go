package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTakeTriggerIsConsumedOnce(t *testing.T) {
	var c HotReloadCommand
	c.pendingTrigger = "src/pages/index.templ"

	if got := c.takeTrigger(); got != "src/pages/index.templ" {
		t.Fatalf("first take should return the trigger, got %q", got)
	}
	// A rebuild with no file event behind it must stay quiet.
	if got := c.takeTrigger(); got != "" {
		t.Errorf("trigger must not survive being taken, got %q", got)
	}
}

func TestScheduleRebuildKeepsLastTriggerInWindow(t *testing.T) {
	var c HotReloadCommand
	c.scheduleRebuild("src/a.templ")
	c.scheduleRebuild("src/b.templ")
	// Empty calls (the initial build) must not erase a real trigger.
	c.scheduleRebuild("")

	c.debounceMu.Lock()
	if c.debounceTimer != nil {
		c.debounceTimer.Stop()
	}
	c.debounceMu.Unlock()

	if got := c.takeTrigger(); got != "src/b.templ" {
		t.Errorf("expected the most recent trigger, got %q", got)
	}
}

func TestDisplayPathIsProjectRelative(t *testing.T) {
	if got := displayPath("./src/pages/index.templ"); got != "src/pages/index.templ" {
		t.Errorf("leading ./ should go, got %q", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Skip("cwd unavailable")
	}
	abs := filepath.Join(wd, "src", "x.templ")
	if got := displayPath(abs); got != filepath.Join("src", "x.templ") {
		t.Errorf("absolute path should be made relative, got %q", got)
	}
}

func TestPhaseIsSilentUnlessVerbose(t *testing.T) {
	var c HotReloadCommand

	out := captureStdout(t, func() { c.phase("Build routes...") })
	if out != "" {
		t.Errorf("a build phase must stay hidden by default, got %q", out)
	}

	c.verbose = true
	out = captureStdout(t, func() { c.phase("Build routes...") })
	if !strings.Contains(out, "Build routes...") {
		t.Errorf("--verbose must bring the phase back, got %q", out)
	}
}
