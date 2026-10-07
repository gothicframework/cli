package buildctl

import (
	"os"
	"path/filepath"
	"testing"
)

// ── the pre-build digest record ───────────────────────────────────────────

// TestWasmStageRecordsPreBuildDigest proves the stage's digest record is the
// snapshot taken before the build: a save landing between the build start and
// the digest recording is not absorbed, so the next cycle still sees it as a
// change and re-runs the stage instead of serving a stale binary forever.
func TestWasmStageRecordsPreBuildDigest(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	ctl := newTestController(t, Options{})
	// EnsureBinary's override path only stats the file, so the stage runs
	// without any TinyGo toolchain (the zero-page GenerateAll is a no-op).
	dummy := filepath.Join(t.TempDir(), "tinygo-override")
	if err := os.WriteFile(dummy, []byte("not a binary"), 0o644); err != nil {
		t.Fatalf("write dummy override: %v", err)
	}
	ctl.cli.Wasm.ConfigOverride = dummy

	snap := TakeSnapshot()

	// A save lands while the stage runs: go.sum is a digest input the build
	// never compiles, so editing it simulates the mid-build save cleanly.
	if err := appendFile("go.sum", "github.com/late/save v9.9.9 h1:z4Yh=\n"); err != nil {
		t.Fatalf("append go.sum: %v", err)
	}
	if ComputeDigest(nil) == snap.Digest {
		t.Fatal("the mid-build save did not change the digest; the test proves nothing")
	}

	if _, err := ctl.realWasmStage("", snap); err != nil {
		t.Fatalf("realWasmStage failed: %v", err)
	}

	stored := LoadDigest()
	if stored == nil {
		t.Fatal("the stage recorded no digest")
	}
	if stored.Digest != snap.Digest {
		t.Errorf("recorded digest %s, want the pre-build snapshot %s — a mid-build save was absorbed into the record", stored.Digest, snap.Digest)
	}
	if !TakeSnapshot().Changed {
		t.Error("the gate reports no change after a mid-build save; the next cycle would skip the stage and strand a stale binary")
	}
}

// TestBuildWasmHandsPreBuildSnapshotToStage proves the wiring: the gate
// snapshot bodyWasm takes reaches the stage BEFORE the stage body runs, so
// the record the stage makes describes the inputs the build set out to
// consume. A save performed inside the stage (landing mid-build) must not be
// part of the snapshot it received.
func TestBuildWasmHandsPreBuildSnapshotToStage(t *testing.T) {
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demo")
	ctl := newTestController(t, Options{})

	pre := TakeSnapshot()

	var got Snapshot
	ctl.WasmRun = func(target string, snap Snapshot) (BuildResult, error) {
		got = snap
		// A save lands while the stage runs.
		if err := appendFile("go.sum", "github.com/late/save v9.9.9 h1:z4Yh=\n"); err != nil {
			t.Errorf("append go.sum: %v", err)
		}
		return BuildResult{}, nil
	}

	res := ctl.BuildWasm("")
	if !res.OK {
		t.Fatalf("BuildWasm failed: %s", res.Err)
	}
	if got.Digest != pre.Digest {
		t.Errorf("the stage received digest %s, want the pre-run reading %s — the snapshot was taken after the stage started", got.Digest, pre.Digest)
	}
	if !got.Changed {
		t.Error("the stage received Changed=false on the first build of a project with no recorded digest")
	}
	if ComputeDigest(nil) == pre.Digest {
		t.Fatal("the mid-build save did not change the digest; the test proves nothing")
	}
	if got.Digest == ComputeDigest(nil) {
		t.Error("the snapshot handed to the stage included the mid-build save")
	}
}

// appendFile appends a line to path, for simulating a save that lands while a
// stage is running.
func appendFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + line)
	return err
}
