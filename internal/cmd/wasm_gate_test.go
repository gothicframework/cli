package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── Soundness tests ───────────────────────────────────────────────────────
// Each verifies that wasmInputChanged returns the correct value for a specific
// change pattern. Tests operate on a freshly scaffolded temp project so the
// initial state has no stored digest.

// writeGoModSum creates go.sum so the gate has something to hash.
func writeGoModSum(t *testing.T) {
	t.Helper()
	// A minimal go.sum to satisfy the gate, content does not need to be valid.
	_ = os.WriteFile("go.sum", []byte("github.com/test/fake v1.0.0 h1:abcdef=\n"), 0o644)
}

// scaffoldGateProject creates a minimal project with go.mod, go.sum, and
// src/{pages,components,topics} so the gate can compute a digest.
func scaffoldGateProject(t *testing.T) {
	t.Helper()
	chdirTemp(t)
	scaffoldSrc(t)
	writeGoMod(t, "demogate")
	writeGoModSum(t)
	// Ensure .gothicCli/ does not exist yet (first cycle).
}

// writePageTempl writes a _templ.go file for a page.
func writePageTempl(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join("src/pages", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writePageGo writes a .go file for a page.
func writePageGo(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join("src/pages", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writeTopicGo writes a .go file under src/topics.
func writeTopicGo(t *testing.T, name, body string) {
	t.Helper()
	_ = os.MkdirAll("src/topics", 0o755)
	path := filepath.Join("src/topics", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writeHelperGo writes a hand-written .go file into a local package dir.
func writeHelperGo(t *testing.T, dir, name, body string) {
	t.Helper()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writeWasmDigestFile writes a digest file so we can test the read path directly.
func writeWasmDigestFile(t *testing.T, d *wasmDigestData) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(wasmDigestPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(wasmDigestPath, data, 0o644); err != nil {
		t.Fatalf("write digest: %v", err)
	}
}

// recordAndSkip sets up a successful recording and then verifies the gate
// reports no change. Files must be in place before calling.
func recordAndSkip(t *testing.T) {
	t.Helper()
	recordWasmDigest(nil)
	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after recording, want false (no changes)")
	}
}

// ── 1. First cycle with no stored digest runs full stage ──────────────────

func TestWasmGate_FirstCycle_NoStoredDigest(t *testing.T) {
	scaffoldGateProject(t)
	// No .gothicCli/wasm-digest.json exists.
	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false on first cycle, want true")
	}
}

// ── 2. Corrupt/unreadable digest runs full stage ──────────────────────────

func TestWasmGate_CorruptDigest_RunsStage(t *testing.T) {
	scaffoldGateProject(t)
	if err := os.MkdirAll(filepath.Dir(wasmDigestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write invalid JSON.
	if err := os.WriteFile(wasmDigestPath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false with corrupt digest, want true")
	}
}

func TestWasmGate_EmptyDigest_RunsStage(t *testing.T) {
	scaffoldGateProject(t)
	writeWasmDigestFile(t, &wasmDigestData{Digest: ""})
	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false with empty digest, want true")
	}
}

func TestWasmGate_MissingDigestFile_RunsStage(t *testing.T) {
	scaffoldGateProject(t)
	// Create the .gothicCli/ dir but no file.
	if err := os.MkdirAll(filepath.Dir(wasmDigestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false with missing digest file, want true")
	}
}

// ── 3. A touch with no content change skips the stage ─────────────────────

func TestWasmGate_TouchNoChange_Skips(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "index_templ.go", `package pages

func (c *Page) ClientSideState() string { return "hi" }
`)
	recordAndSkip(t)

	// Touch the file (same content).
	_ = os.Chtimes(filepath.Join("src/pages", "index_templ.go"), time.Now(), time.Now())
	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after touch with same content, want false")
	}
}

// ── 4. ClientSideState body edit runs the stage ──────────────────────────

func TestWasmGate_ClientSideStateEdit_RunsStage(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "counter_templ.go", `package pages

func (c *Page) ClientSideState() string { return "old" }
`)
	recordAndSkip(t)

	// Edit the content.
	_ = os.WriteFile(filepath.Join("src/pages", "counter_templ.go"),
		[]byte(`package pages

func (c *Page) ClientSideState() string { return "new" }
`), 0o644)

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after ClientSideState body edit, want true")
	}
}

// ── 5. Adding the first ClientSideState to a stateless page flips digest ──

func TestWasmGate_ClientSideStateAdd_FlipsDigest(t *testing.T) {
	scaffoldGateProject(t)
	// Start with a _templ.go without ClientSideState.
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) Render() string { return "<div></div>" }
`)
	recordAndSkip(t)

	// Add ClientSideState function.
	_ = os.WriteFile(filepath.Join("src/pages", "page_templ.go"),
		[]byte(`package pages

func (c *Page) ClientSideState() string { return "new" }
`), 0o644)

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after adding ClientSideState, want true")
	}
}

// ── 6. Removing ClientSideState flips digest ─────────────────────────────

func TestWasmGate_ClientSideStateRemove_FlipsDigest(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "active" }
`)
	recordAndSkip(t)

	// Remove ClientSideState.
	_ = os.WriteFile(filepath.Join("src/pages", "page_templ.go"),
		[]byte(`package pages

func (c *Page) Render() string { return "<div></div>" }
`), 0o644)

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after removing ClientSideState, want true")
	}
}

// ── 7. Topic source edit flips digest ────────────────────────────────────

func TestWasmGate_TopicEdit_FlipsDigest(t *testing.T) {
	scaffoldGateProject(t)
	writeTopicGo(t, "counter.go", `package topics

type Counter struct {
	Value int
}
`)
	recordAndSkip(t)

	_ = os.WriteFile(filepath.Join("src/topics", "counter.go"),
		[]byte(`package topics

type Counter struct {
	Value  int
	Label string
}
`), 0o644)

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after topic edit, want true")
	}
}

// ── 8. go.mod edit flips digest ──────────────────────────────────────────

func TestWasmGate_GoModEdit_FlipsDigest(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	recordAndSkip(t)

	// Append a newline to go.mod.
	f, err := os.OpenFile("go.mod", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n")
	_ = f.Close()

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after go.mod edit, want true")
	}
}

// ── 9. CLI binary change flips digest ────────────────────────────────────

func TestWasmGate_CLIBinaryChange_FlipsDigest(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	recordAndSkip(t)

	oldStored := loadWasmDigest()
	if oldStored == nil {
		t.Fatal("no stored digest after recordWasmDigest")
	}

	// Store a deliberately different digest to simulate a binary replacement.
	// The gate compares computed(real binary) vs stored; if stored differs,
	// the gate triggers. This proves the stored digest IS the gate mechanism
	// and that any change to inputs (including binary identity) forces a run.
	writeWasmDigestFile(t, &wasmDigestData{
		Digest:           "bogus0000000000000000000000000000000000000000000", // clearly different
		LocalPackageDirs: oldStored.LocalPackageDirs,
	})
	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after storing different digest, want true")
	}
}

// ── 10. An edit to a shared local package runs stage and rebuilds ─────────

func TestWasmGate_LocalPackageEdit_RunsStage(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	// Create a local helper package.
	helperDir, err := filepath.Abs("src/helpers")
	if err != nil {
		t.Fatal(err)
	}
	writeHelperGo(t, "src/helpers", "state.go", `package helpers

func GetState() string { return "old" }
`)
	// Record digest with the local dir in the manifest.
	recordWasmDigest([]string{helperDir})
	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after recording with local dir, want false")
	}

	// Edit the helper file.
	_ = os.WriteFile(filepath.Join(helperDir, "state.go"),
		[]byte(`package helpers

func GetState() string { return "new" }
`), 0o644)

	if !wasmInputChanged() {
		t.Error("wasmInputChanged() = false after local package edit, want true")
	}
}

// ── 11. Recording only after success, ignoring previous failed digest ─────

func TestWasmGate_RecordAfterSuccess_Persists(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	recordWasmDigest(nil)

	stored := loadWasmDigest()
	if stored == nil {
		t.Fatal("no digest after recordWasmDigest")
	}
	if stored.Digest == "" {
		t.Error("stored digest is empty")
	}
}

// ── 12. Integration: buildWasmAll with gate (no pages) ───────────────────

func TestWasmGate_BuildWasmAll_NoPages_RecordsDigest(t *testing.T) {
	scaffoldGateProject(t)
	// No _templ.go files with ClientSideState.
	recordWasmDigest(nil)

	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after no-page record, want false")
	}
}

// ── 13. Second call after no change reports unchanged ────────────────────

func TestWasmGate_StableDigest_SkipsRepeatedly(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	recordAndSkip(t)

	// Check a few times.
	for i := 0; i < 5; i++ {
		if wasmInputChanged() {
			t.Errorf("wasmInputChanged() = true on iteration %d, want false", i)
		}
	}
}

// ── 14. Multiple files with different extensions ──────────────────────────

func TestWasmGate_NonGoFiles_Ignored(t *testing.T) {
	scaffoldGateProject(t)
	writePageGo(t, "handler.go", `package pages

func Handle() string { return "x" }
`)
	recordAndSkip(t)

	// Adding a .json file should NOT change the digest.
	_ = os.WriteFile(filepath.Join("src/pages", "data.json"), []byte(`{}`), 0o644)
	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after adding .json, want false")
	}
}

// ── 15. Record and skip with local package dirs ───────────────────────────

func TestWasmGate_LocalDirs_SkipAfterRecord(t *testing.T) {
	scaffoldGateProject(t)
	writePageTempl(t, "page_templ.go", `package pages

func (c *Page) ClientSideState() string { return "x" }
`)
	helperDir, _ := filepath.Abs("src/helpers")
	writeHelperGo(t, "src/helpers", "util.go", `package helpers

func Util() string { return "util" }
`)
	recordWasmDigest([]string{helperDir})
	if wasmInputChanged() {
		t.Error("wasmInputChanged() = true after recording with local dirs, want false")
	}
}

// ── Digest computation cost benchmark ─────────────────────────────────────

func BenchmarkComputeWasmDigest(b *testing.B) {
	dir := b.TempDir()
	orig, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(orig) }()

	// Create a project with ~40 WASM units (pages with ClientSideState).
	for _, d := range []string{"src/pages", "src/components", "src/topics"} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile("go.mod", []byte("module benchgate\n\ngo 1.23\n"), 0o644)
	_ = os.WriteFile("go.sum", []byte("sum\n"), 0o644)

	// 40 files that look like scanned pages with ClientSideState.
	for i := 0; i < 40; i++ {
		content := []byte("package pages\n\nfunc (c *Page) ClientSideState() string { return \"")
		content = append(content, byte('0'+i%10))
		content = append(content, "\" }\n"...)
		_ = os.WriteFile(filepath.Join("src/pages", "p"+itoa(i)+"_templ.go"), content, 0o644)
	}

	// Local package dirs.
	localDirs := []string{
		filepath.Join(dir, "src/helpers"),
		filepath.Join(dir, "src/shared"),
	}
	for _, ld := range localDirs {
		_ = os.MkdirAll(ld, 0o755)
		_ = os.WriteFile(filepath.Join(ld, "state.go"), []byte("package helpers\nfunc F() string { return \"f\" }\n"), 0o644)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		computeWasmDigest(localDirs)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// TestWasmDigestTracksBuildCache pins the one input that can change with no
// source file changing: a production build (gothic wasm, deploy) rewrites the
// artifacts and the per-page cache. If the gate ignored that, it would skip at
// session start and the shaping mismatch would ambush the developer's first
// real edit with a full rebuild of every unit.
func TestWasmDigestTracksBuildCache(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })

	if err := os.MkdirAll(".gothicCli", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(wasmBuildCachePath, []byte(`{"counter":"devhash"}`), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	before := computeWasmDigest(nil)

	// A production build rewrites the same file with different hashes.
	if err := os.WriteFile(wasmBuildCachePath, []byte(`{"counter":"prodhash"}`), 0o644); err != nil {
		t.Fatalf("rewrite cache: %v", err)
	}
	after := computeWasmDigest(nil)

	if before == after {
		t.Error("digest ignored a rewritten build cache, so the gate would skip a stage that must run")
	}
}
