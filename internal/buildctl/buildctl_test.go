package buildctl

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// ── Sync idempotence ──────────────────────────────────────────────────────

// TestSyncIdempotentWithFakeExecutors runs Sync twice against a scaffolded
// project whose external executors are fakes (a no-op tailwind binary, a
// counting WASM runner). The first Sync compiles; the second must find every
// cache clean and recompile nothing.
func TestSyncIdempotentWithFakeExecutors(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	bin := writeFakeTailwind(t, true)
	writeConfig(t, bin, bin)

	ctl := newTestController(t, Options{MainBinary: "tmp/main"})
	wasmCalls := 0
	ctl.WasmRun = func(target string, snap Snapshot) (BuildResult, error) {
		wasmCalls++
		// A fake stage that leaves the artifacts untouched: it records the
		// pre-build snapshot like the real stage, keeping the gate consistent.
		RecordDigestFor(snap, nil)
		return BuildResult{}, nil
	}

	first := ctl.Sync()
	if !first.OK {
		t.Fatalf("first Sync failed: %s", first.Err)
	}
	if len(first.Recompiled) == 0 {
		t.Error("the first Sync of a cold project must recompile something (routes_gen)")
	}

	second := ctl.Sync()
	if !second.OK {
		t.Fatalf("second Sync failed: %s", second.Err)
	}
	if len(second.Recompiled) != 0 {
		t.Errorf("second Sync recompiled %+v; every artifact was fresh — nothing should have recompiled", second.Recompiled)
	}
	if wasmCalls != 1 {
		t.Errorf("the WASM runner ran %d times across two Syncs, want 1 (the digest gate must skip the second)", wasmCalls)
	}
}

// ── invalidation ordering ─────────────────────────────────────────────────

// TestTemplEditMarksRoutesGenAndWasmStale proves the invalidation order: a
// templ edit first makes the templ stage dirty; compiling it produces a fresh
// _templ.go whose content flips the route registry and the WASM digest, so
// the build result declares routes_gen stale and the stateful unit stale.
func TestTemplEditMarksRoutesGenAndWasmStale(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")

	page := scaffoldPage(t, "counter.templ", `package pages

import (
	"strconv"

	routes "github.com/gothicframework/core/router"
)

type CounterProps = interface{}

var CounterConfig = routes.RouteConfig[CounterProps]{
	Type:       routes.DYNAMIC,
	HttpMethod: routes.GET,
	ClientSideState: func() {
		count := CreateObservable(0)
		Observe(func() {
			SetText("count", strconv.Itoa(count.Get()))
		}, count)
	},
}

templ Counter(props CounterProps) {
	<div id="count">0</div>
}
`)
	ctl := newTestController(t, Options{})

	res := ctl.BuildTempl("")
	if !res.OK {
		t.Fatalf("BuildTempl failed: %s", res.Err)
	}
	if len(res.Recompiled) == 0 {
		t.Error("a first compile of a new page must report the generated counterpart as recompiled")
	}
	stale := strings.Join(res.NowStale, ",")
	if !strings.Contains(stale, "routes_gen") {
		t.Errorf("a templ compile must declare routes_gen stale, got %+v", res.NowStale)
	}
	if !strings.Contains(stale, "wasm:/counter") {
		t.Errorf("compiling a stateful page must declare its WASM unit stale, got %+v", res.NowStale)
	}

	// BuildStatus agrees through the layer's own signals: templ fresh (just
	// compiled), the route registry stale (not regenerated yet), the WASM
	// inventory stale (the digest covers the changed _templ.go).
	status := ctl.BuildStatus()
	got := map[string]StatusArtifact{}
	for _, a := range status.Artifacts {
		got[a.Name] = a
	}
	if got[StageRoutesGen].State != StatusStale {
		t.Errorf("routes_gen state = %q, want stale after the templ compile", got[StageRoutesGen].State)
	}
	if got[StageWasm].State != StatusStale {
		t.Errorf("wasm state = %q, want stale after the stateful page changed", got[StageWasm].State)
	}
	_ = page
}

// TestTemplNoOpDeclaresNothingStale pins the idempotent corner: a build with
// nothing dirty recompiles nothing and declares nothing stale.
func TestTemplNoOpDeclaresNothingStale(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")

	ctl := newTestController(t, Options{})
	_ = ctl.BuildTempl("") // compile once

	res := ctl.BuildTempl("")
	if !res.OK {
		t.Fatalf("BuildTempl failed: %s", res.Err)
	}
	if len(res.Recompiled) != 0 {
		t.Errorf("a clean tree recompiled %+v", res.Recompiled)
	}
	if len(res.NowStale) != 0 {
		t.Errorf("a no-op compile declared stale: %+v", res.NowStale)
	}
}

// ── verified-unchanged mtime refresh ──────────────────────────────────────

// routesStatusOf extracts one artifact from a status report.
func routesStatusOf(t *testing.T, res StatusResult, name string) StatusArtifact {
	t.Helper()
	for _, a := range res.Artifacts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("artifact %q missing from status %+v", name, res.Artifacts)
	return StatusArtifact{}
}

// TestVerifiedUnchangedRoutesGenPersistsAcrossRestart pins the false-stale
// fix for the write-if-changed generated stages: the stage verifies its
// artifact current without rewriting it (the artifact keeps its old mtime),
// and a fresh process (no in-session lastOK) would compare the newer source
// against that stale mtime and report false "stale". The persisted
// verification marker is what flips the signal back to fresh.
func TestVerifiedUnchangedRoutesGenPersistsAcrossRestart(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, "", "")
	// A routed source the status comparison walks.
	if err := os.WriteFile("src/pages/placeholder.go", []byte("package pages\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctl := newTestController(t, Options{})
	if res := ctl.BuildRoutes(); !res.OK {
		t.Fatalf("first BuildRoutes failed: %s", res.Err)
	}
	if _, err := os.Stat(routesGenPath); err != nil {
		t.Fatalf("routes_gen not generated: %v", err)
	}

	// Backdate the artifact (the write-if-changed skip state) and make the
	// source newer than every recorded time: the false-stale setup.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(routesGenPath, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("src/pages/placeholder.go", []byte("package pages\n// touched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The "restart": a new controller has no in-session times. Before the
	// stage re-runs, the signal is the stale one the bug reports.
	fresh := newTestController(t, Options{})
	if a := routesStatusOf(t, fresh.BuildStatus(), StageRoutesGen); a.State != StatusStale {
		t.Fatalf("precondition: the backdated artifact must read stale before the re-run, got %+v", a)
	}

	// Re-running the stage finds the content unchanged (no recompile) and
	// persists the verification.
	res := fresh.BuildRoutes()
	if !res.OK {
		t.Fatalf("second BuildRoutes failed: %s", res.Err)
	}
	if len(res.Recompiled) != 0 {
		t.Errorf("an unchanged route registry must not recompile, got %+v", res.Recompiled)
	}
	if fi, err := os.Stat(routesGenPath); err != nil {
		t.Fatal(err)
	} else if !fi.ModTime().Before(past.Add(time.Minute)) {
		t.Errorf("the verified-unchanged artifact was rewritten (%s); the fix must not touch it", fi.ModTime())
	}

	// And the status signal agrees across the "restart": fresh, not stale.
	// A third controller isolates the persisted marker from any in-session
	// lastOK time — its only fresh signal is the verification marker on disk,
	// which is what a real cross-process restart relies on.
	third := newTestController(t, Options{})
	if a := routesStatusOf(t, third.BuildStatus(), StageRoutesGen); a.State != StatusFresh {
		t.Errorf("routes_gen read from the persisted marker alone = %+v, want fresh", a)
	}
	if a := routesStatusOf(t, fresh.BuildStatus(), StageRoutesGen); a.State != StatusFresh {
		t.Errorf("routes_gen after a verified-unchanged re-run = %+v, want fresh", a)
	}
}

// TestVerifiedUnchangedCSSPersistsAcrossRestart is the CSS twin: a build that
// computes the same CSS (the tool keeps the old mtime) persists the
// verification instead of touching the output — touching it would make the
// downstream stages that walk the source tree see a "changed" file.
func TestVerifiedUnchangedCSSPersistsAcrossRestart(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, "", "")
	if err := os.MkdirAll("public", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cssOutPath, []byte("<css>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("src/app.css", []byte("body {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cssOutPath, past, past); err != nil {
		t.Fatal(err)
	}

	ctl := newTestController(t, Options{})
	if a := routesStatusOf(t, ctl.BuildStatus(), StageCSS); a.State != StatusStale {
		t.Fatalf("precondition: the backdated CSS must read stale before the build, got %+v", a)
	}
	// The tool computes the identical CSS and writes nothing (the
	// write-if-changed behaviour the real build shows when the output is
	// unchanged).
	ctl.CssRun = func() error { return nil }

	res := ctl.BuildCSS("")
	if !res.OK {
		t.Fatalf("BuildCSS failed: %s", res.Err)
	}
	if fi, err := os.Stat(cssOutPath); err != nil {
		t.Fatal(err)
	} else if !fi.ModTime().Before(past.Add(time.Minute)) {
		t.Errorf("the verified-unchanged CSS was rewritten (%s); the fix must not touch it", fi.ModTime())
	}
	// A third controller reads the persisted marker alone (no in-session
	// times), which pins the actual cross-restart behaviour the fix exists for.
	third := newTestController(t, Options{})
	if a := routesStatusOf(t, third.BuildStatus(), StageCSS); a.State != StatusFresh {
		t.Errorf("css read from the persisted marker alone = %+v, want fresh", a)
	}
	if a := routesStatusOf(t, ctl.BuildStatus(), StageCSS); a.State != StatusFresh {
		t.Errorf("css after a verified-unchanged build = %+v, want fresh", a)
	}
}

// ── go build capture ──────────────────────────────────────────────────────

// TestGoBuildErrorsSurfaceAsDiagnostics injects a failing go build whose
// combined output carries a compile error; the result must carry the raw
// compiler text verbatim inside the diagnostic, plus the translation.
func TestGoBuildErrorsSurfaceAsDiagnostics(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, "", "")

	ctl := newTestController(t, Options{MainBinary: "tmp/main"})
	const compilerText = "src/pages/counter_templ.go:12:2: undefined: AppTopic"
	ctl.GoRun = func(binary string) (string, error) {
		return compilerText, errors.New("exit status 1")
	}

	res := ctl.BuildGo()
	if res.OK {
		t.Fatal("BuildGo reported success for a failing build")
	}
	if res.Err != "exit status 1" {
		t.Errorf("Err = %q, want the tool's own error text", res.Err)
	}
	found := false
	for _, d := range res.Diagnostics {
		if d.Raw != compilerText {
			t.Errorf("a diagnostic's Raw must preserve the compiler text verbatim, got %q", d.Raw)
		}
		if d.Diagnosis == "" {
			t.Errorf("a compile error must translate to a diagnosis, got %+v", d)
		}
		if d.Skill == "" {
			t.Errorf("a compile error must name the relevant skill, got %+v", d)
		}
		found = true
	}
	if !found {
		t.Error("no diagnostic surfaced from the captured compiler output")
	}
}

// ── edit lock ─────────────────────────────────────────────────────────────

// TestEditLockSuppressesWatcherBuildsThenRunsOneSync proves the edit-session
// contract: while EditBegin holds the build lock, stage calls short-circuit
// without work; EditEnd releases the lock and runs exactly one Sync.
func TestEditLockSuppressesWatcherBuildsThenRunsOneSync(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	bin := writeFakeTailwind(t, true)
	writeConfig(t, bin, bin)

	beginCalls, endCalls := 0, 0
	afterSyncCalls := 0
	ctl := newTestController(t, Options{
		BeginEdit: func() (end func()) {
			beginCalls++
			return func() { endCalls++ }
		},
		AfterSync: func(res BuildResult) { afterSyncCalls++ },
	})
	templCalls := 0
	ctl.TemplRun = func(file string) (string, error) {
		templCalls++
		return "", nil
	}

	if ctl.Editing() {
		t.Fatal("Editing() = true before any session")
	}

	ctl.EditBegin()
	if !ctl.Editing() {
		t.Fatal("Editing() = false inside an open session")
	}
	if beginCalls != 1 {
		t.Errorf("BeginEdit hook ran %d times, want 1", beginCalls)
	}

	// Watcher-driven builds and other stage calls are suppressed, instantly.
	if res := ctl.BuildTempl("src/pages/counter.templ"); !res.Suppressed || templCalls != 0 {
		t.Errorf("a stage call inside an edit session must short-circuit: %+v, templCalls=%d", res, templCalls)
	}
	if res := ctl.Sync(); !res.Suppressed {
		t.Errorf("Sync inside an edit session must short-circuit: %+v", res)
	}

	// EditEnd: the lock is released and exactly one sync runs.
	res := ctl.EditEnd()
	if !res.OK {
		t.Fatalf("EditEnd's sync failed: %s", res.Err)
	}
	if templCalls != 1 {
		t.Errorf("EditEnd ran the templ stage %d times, want exactly once", templCalls)
	}
	if endCalls != 1 {
		t.Errorf("the watcher-resume hook ran %d times, want 1", endCalls)
	}
	if afterSyncCalls != 1 {
		t.Errorf("AfterSync ran %d times, want 1", afterSyncCalls)
	}
	if ctl.Editing() {
		t.Error("Editing() must be false after EditEnd")
	}

	// A second EditEnd without a session is a no-op.
	if res := ctl.EditEnd(); !res.OK || afterSyncCalls != 1 {
		t.Errorf("a session-less EditEnd must be a no-op: %+v, afterSyncCalls=%d", res, afterSyncCalls)
	}
}

// TestBuildStatusMidRebuild pins the mid-rebuild signal: BuildStatus reports
// the artifact whose stage is currently running.
func TestBuildStatusMidRebuild(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	bin := writeFakeTailwind(t, true)
	writeConfig(t, bin, bin)

	ctl := newTestController(t, Options{})
	entered := make(chan struct{})
	release := make(chan struct{})
	ctl.TemplRun = func(file string) (string, error) {
		close(entered)
		<-release
		return "", nil
	}

	go func() {
		ctl.BuildTempl("")
	}()
	<-entered

	status := ctl.BuildStatus()
	if !status.Rebuilding {
		t.Error("BuildStatus during a running stage must report Rebuilding")
	}
	for _, a := range status.Artifacts {
		if a.Name != StageTempl && a.State == StatusMidRebuild {
			t.Errorf("artifact %q = %q during a templ build; only templ may report mid-rebuild", a.Name, a.State)
		}
	}
	if a := status.Artifacts[0]; a.Name != StageTempl || a.State != StatusMidRebuild {
		t.Errorf("templ artifact during its own run = %+v, want mid-rebuild", a)
	}
	close(release)
}

// TestTemplRenderFileUpdatesCache covers the targeted-build seam: after a
// named render the file's cache entry is refreshed, so a following full run
// skips it.
func TestTemplRenderFileUpdatesCache(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	scaffoldPage(t, "home.templ", "package pages\n\ntempl Home() {\n\t<div>hi</div>\n}\n")

	ctl := newTestController(t, Options{})
	if res := ctl.BuildTempl("src/pages/home.templ"); !res.OK {
		t.Fatalf("BuildTempl(named) failed: %s", res.Err)
	}
	if _, err := os.Stat("src/pages/home_templ.go"); err != nil {
		t.Fatalf("the named build produced no counterpart: %v", err)
	}
	// The cache now knows the file: a full run must skip it (instant).
	dirty, _ := templDirtyFiles()
	if len(dirty) != 0 {
		t.Errorf("after a named compile the tree is still dirty: %+v", dirty)
	}
}

// ── per-artifact mid-rebuild ──────────────────────────────────────────────

// blockStage is the body of a blocking fake stage seam: it announces entry
// and waits to be released.
func blockStage(entered, release chan struct{}) {
	close(entered)
	<-release
}

// TestBuildStatusMidRebuildPerArtifact pins the mid-rebuild signal for every
// artifact: while a stage runs (the busy bits fillRun maintains), BuildStatus
// reports that artifact — and only that artifact — as mid-rebuild.
func TestBuildStatusMidRebuildPerArtifact(t *testing.T) {
	cases := []struct {
		name string // the artifact expected mid-rebuild
		// arm installs the blocking fake seam; nil for stages without one.
		arm func(ctl *Controller, entered, release chan struct{})
		// invoke runs the stage; it must signal entry on entered (the
		// blocking seams do it themselves).
		invoke func(ctl *Controller, entered, release chan struct{})
	}{
		{
			name: StageTempl,
			arm: func(ctl *Controller, entered, release chan struct{}) {
				ctl.TemplRun = func(string) (string, error) {
					blockStage(entered, release)
					return "", nil
				}
			},
			invoke: func(ctl *Controller, entered, release chan struct{}) { ctl.BuildTempl("") },
		},
		{
			name: StageRoutesGen,
			arm: func(ctl *Controller, entered, release chan struct{}) {
				ctl.RoutesRun = func(string) error {
					blockStage(entered, release)
					return nil
				}
			},
			invoke: func(ctl *Controller, entered, release chan struct{}) { ctl.BuildRoutes() },
		},
		{
			// The topic stage has no executor seam, so the busy bit is set the
			// same way fillRun sets it; the assertion is on the status routing.
			name: StageTopicGen,
			invoke: func(ctl *Controller, entered, release chan struct{}) {
				ctl.busy.Store(busyTopicGen)
				close(entered)
			},
		},
		{
			name: StageWasm,
			arm: func(ctl *Controller, entered, release chan struct{}) {
				ctl.WasmRun = func(string, Snapshot) (BuildResult, error) {
					blockStage(entered, release)
					return BuildResult{}, nil
				}
			},
			invoke: func(ctl *Controller, entered, release chan struct{}) { ctl.BuildWasm("") },
		},
		{
			name: StageCSS,
			arm: func(ctl *Controller, entered, release chan struct{}) {
				ctl.CssRun = func() error {
					blockStage(entered, release)
					return nil
				}
			},
			invoke: func(ctl *Controller, entered, release chan struct{}) { ctl.BuildCSS("") },
		},
		{
			name: "app",
			arm: func(ctl *Controller, entered, release chan struct{}) {
				ctl.GoRun = func(string) (string, error) {
					blockStage(entered, release)
					return "", nil
				}
			},
			invoke: func(ctl *Controller, entered, release chan struct{}) { ctl.BuildGo() },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			scaffoldSrc(t)
			writeGoMod(t, "demo")
			bin := writeFakeTailwind(t, true)
			writeConfig(t, bin, bin)
			ctl := newTestController(t, Options{MainBinary: "tmp/main"})

			entered := make(chan struct{})
			release := make(chan struct{})
			if tc.arm != nil {
				tc.arm(ctl, entered, release)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.invoke(ctl, entered, release)
			}()
			<-entered

			status := ctl.BuildStatus()
			if !status.Rebuilding {
				t.Errorf("BuildStatus during a running %s stage must report Rebuilding", tc.name)
			}
			for _, a := range status.Artifacts {
				if a.Name == tc.name {
					if a.State != StatusMidRebuild {
						t.Errorf("%s during its own run = %+v, want mid-rebuild", tc.name, a)
					}
				} else if a.State == StatusMidRebuild {
					t.Errorf("artifact %q = %q during a %s build; only %s may report mid-rebuild", a.Name, a.State, tc.name, tc.name)
				}
			}

			close(release)
			<-done
			if tc.arm == nil {
				ctl.busy.Store(0) // the injection path has no fillRun to un-set it
			}
		})
	}
}
