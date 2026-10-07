package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	buildctl "github.com/gothicframework/cli/v3/internal/buildctl"
	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
)

// fakeBuildCtl wires a buildctl controller whose stages are test doubles:
// templ and routes render instantly, the WASM runner is a no-op, and the app
// build fails or succeeds as the test asks.
func fakeBuildCtl(cli *gothic_cli.GothicCli, goBuildErrText string, afterSync func(buildctl.BuildResult)) *buildctl.Controller {
	ctl := buildctl.New(cli, buildctl.Options{MainBinary: "tmp/main", AfterSync: afterSync})
	ctl.TemplRun = func(file string) (string, error) { return "", nil }
	ctl.RoutesRun = func(goModName string) error { return nil }
	ctl.CssRun = func() error { return nil }
	ctl.WasmRun = func(target string, snap buildctl.Snapshot) (buildctl.BuildResult, error) {
		return buildctl.BuildResult{Counts: &buildctl.StageCounts{}}, nil
	}
	ctl.GoRun = func(binary string) (string, error) {
		if goBuildErrText == "" {
			return "", nil
		}
		return goBuildErrText, errors.New("exit status 1")
	}
	return ctl
}

// TestRebuildRoutesThroughBuildCtlAndCapturesDiagnostics proves the watcher's
// rebuild runs the pipeline stages through the build controller and surfaces
// a failing go build as structured diagnostics with the raw compiler text
// preserved — while the terminal contract lines stay in place.
func TestRebuildRoutesThroughBuildCtlAndCapturesDiagnostics(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo","tailwindBinary":"/nonexistent/tw","wasmBinary":"/nonexistent/tw"}`)

	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	ctl := fakeBuildCtl(&cli, "src/shared/dto.go:4:2: net/http not supported by TinyGo", nil)
	cmd.sleeper = func(time.Duration) {}
	var sseMu sync.Mutex
	var sseEvents []string
	cmd.sseSend = func(et, d string) {
		sseMu.Lock()
		sseEvents = append(sseEvents, et+":"+d)
		sseMu.Unlock()
	}
	cmd.ctl = ctl

	out := captureStdout(t, func() { cmd.rebuild() })

	if !strings.Contains(out, "cannot build the app: exit status 1") {
		t.Errorf("the terminal contract line for a go-build failure must stay, got:\n%s", out)
	}
	if !strings.Contains(out, "diagnosis: ") || !strings.Contains(out, "net/http") {
		t.Errorf("a failing build must mirror its translated diagnosis to the terminal, got:\n%s", out)
	}

	sseMu.Lock()
	snapshot := append([]string(nil), sseEvents...)
	sseMu.Unlock()
	var builderror string
	for _, e := range snapshot {
		if strings.HasPrefix(e, "builderror:go build:") {
			builderror = e
		}
	}
	if builderror == "" {
		t.Fatalf("the go build failure must emit the builderror sse event, got %+v", snapshot)
	}
	if !strings.Contains(builderror, "go build: exit status 1") {
		t.Errorf("the builderror event must keep the raw error text, got %q", builderror)
	}

	// The structured result carries the raw compiler text + translation.
	goRes := ctl.BuildGo()
	if goRes.OK {
		t.Fatal("expected the fake build to keep failing")
	}
	found := false
	for _, d := range goRes.Diagnostics {
		if !strings.Contains(d.Raw, "net/http not supported by TinyGo") {
			t.Errorf("diagnostic Raw must preserve the raw compiler text, got %+v", d)
		}
		if d.Diagnosis == "" || d.Fix == "" {
			t.Errorf("diagnostic must carry diagnosis + fix, got %+v", d)
		}
		found = true
	}
	if !found {
		t.Error("no diagnostic surfaced from the captured compiler output")
	}
}

// TestEditSessionSuppressesWatcherRebuildsUntilEditEnd proves the edit lock
// at the session level: while a session holds the build lock, saves arm no
// watcher rebuild; EditEnd's after-sync hook re-arms the normal cycle.
func TestEditSessionSuppressesWatcherRebuildsUntilEditEnd(t *testing.T) {
	chdirTemp(t)
	cli := gothic_cli.NewCli()
	cmd := newHotReloadCommandCli(&cli)
	cmd.ctl = fakeBuildCtl(&cli, "", cmd.afterEditSync)

	ctl := cmd.buildCtl()
	ctl.EditBegin()
	if !ctl.Editing() {
		t.Fatal("the edit session did not open")
	}

	cmd.scheduleRebuild("src/pages/index.templ")
	cmd.debounceMu.Lock()
	armed := cmd.debounceTimer != nil
	cmd.debounceMu.Unlock()
	if armed {
		t.Error("a save during an edit session must not arm a watcher rebuild")
	}

	ctl.EditEnd()
	// afterEditSync re-arms the normal cycle; the debounce fires within 150ms.
	deadline := time.Now().Add(2 * time.Second)
	armed = false
	for time.Now().Before(deadline) {
		cmd.debounceMu.Lock()
		armed = cmd.debounceTimer != nil
		cmd.debounceMu.Unlock()
		if armed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !armed {
		t.Error("EditEnd must re-arm the watcher cycle through the after-sync hook")
	}

	cmd.debounceMu.Lock()
	if cmd.debounceTimer != nil {
		cmd.debounceTimer.Stop()
		cmd.debounceTimer = nil
	}
	cmd.debounceMu.Unlock()
}

// TestWasmGateShimAgreesWithBuildctl pins that the hot-reload package's
// delegating wrappers and buildctl's gate read the same digest file with the
// same formula.
func TestWasmGateShimAgreesWithBuildctl(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")

	if got, want := computeWasmDigest(nil), buildctl.ComputeDigest(nil); got != want {
		t.Errorf("cmd shim digest = %s, buildctl digest = %s", got, want)
	}

	thatDir, _ := filepath.Abs("src/pages")
	recordWasmDigestNow([]string{thatDir})
	if buildctl.InputChanged() {
		t.Error("buildctl's gate disagreed with the digest the cmd shim just recorded")
	}

	snapCmd := takeWasmInputSnapshot()
	snapCtl := buildctl.TakeSnapshot()
	if snapCmd.changed != snapCtl.Changed {
		t.Errorf("changed signals disagree: cmd=%v buildctl=%v", snapCmd.changed, snapCtl.Changed)
	}
}
