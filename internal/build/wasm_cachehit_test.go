package helpers

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// These tests cover the hermetic *cache-hit early-return* prologues of
// GeneratePage and buildTopicManager. They never reach the toolchain-invoking
// build steps (which stay integration-only); they verify that an up-to-date
// cache entry plus an existing output file short-circuits the build.

func TestGeneratePage_CacheHitSkipsBuild(t *testing.T) {
	dir := withTempCwd(t)
	srcPath := filepath.Join(dir, "page.go")
	if err := os.WriteFile(srcPath, []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	outDir := filepath.Join(dir, "public", "wasm")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("mkdir outDir: %v", err)
	}

	h := DefaultWasmHelper()
	h.cache = loadWasmCache()

	page := WasmPage{
		SourceFile:  srcPath,
		OutputName:  "counter",
		Compression: WasmCompressionGzip,
	}

	// Pre-create the output file and seed the cache with the matching hash so
	// the up-to-date branch fires.  The hash must be computed over the same
	// rendered bytes that GeneratePage will produce.
	outFile := filepath.Join(outDir, page.OutputName+".wasm.gz")
	if err := os.WriteFile(outFile, []byte("prebuilt"), 0644); err != nil {
		t.Fatalf("write out file: %v", err)
	}
	// Render the page's main.go the same way GeneratePage will, so the hash matches.
	rendered := h.renderWasmMainBytesForTest(page, "")
	hash := h.pageInputHash(page, rendered)
	h.cache.update(page.OutputName, hash)

	if err := h.GeneratePage(page, outDir, &sync.Once{}, topicCodegenData{}); err != nil {
		t.Fatalf("GeneratePage cache-hit: %v", err)
	}
	// The prebuilt file must still be there (not rebuilt).
	if data, _ := os.ReadFile(outFile); string(data) != "prebuilt" {
		t.Errorf("expected prebuilt file untouched on cache hit, got %q", data)
	}
}

// TestGeneratePage_SourceChangeInvalidatesCache proves the dangerous direction:
// A change to the compiler build recipe (flags) must invalidate the per-page cache
// even when nothing in the source or runtime .go files changed, otherwise a
// framework release that only tweaks build flags (as the -gc conservative fix did)
// would keep serving a stale WASM. This guards the buildRecipeFingerprint wiring.
func TestGeneratePage_BuildRecipeChangeInvalidatesCache(t *testing.T) {
	dir := withTempCwd(t)
	srcPath := filepath.Join(dir, "page.go")
	if err := os.WriteFile(srcPath, []byte("package x\n\nvar Counter = 1\n"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	h := DefaultWasmHelper()
	h.cache = loadWasmCache()
	page := WasmPage{SourceFile: srcPath, OutputName: "counter", Compression: WasmCompressionGzip}
	rendered := []byte("package main\nfunc main() {}\n")

	before := h.pageInputHash(page, rendered)

	// Simulate a framework release changing the TinyGo recipe. Restore afterwards so
	// other tests see the real flags.
	saved := tinygoWasmFlags
	tinygoWasmFlags = append(append([]string{}, saved...), "-panic=trap")
	defer func() { tinygoWasmFlags = saved }()

	after := h.pageInputHash(page, rendered)
	if after == before {
		t.Fatal("expected pageInputHash to change after the build recipe changed")
	}
}

// When the rendered main.go content changes (e.g. a FuncBody edit), pageInputHash
// must differ from the cached hash so the build is NOT skipped.
func TestGeneratePage_RenderedBodyChangeInvalidatesCache(t *testing.T) {
	dir := withTempCwd(t)
	srcPath := filepath.Join(dir, "page.go")
	if err := os.WriteFile(srcPath, []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	h := DefaultWasmHelper()
	h.cache = loadWasmCache()

	page := WasmPage{
		SourceFile:  srcPath,
		OutputName:  "counter",
		Compression: WasmCompressionGzip,
		FuncBody:    "x := 1",
	}

	// Seed the cache with the hash of the ORIGINAL rendered body.
	originalRendered := h.renderWasmMainBytesForTest(page, page.FuncBody)
	originalHash := h.pageInputHash(page, originalRendered)
	h.cache.update(page.OutputName, originalHash)
	if !h.cache.upToDate(page.OutputName, originalHash) {
		t.Fatalf("sanity: original hash should be up-to-date")
	}

	// Change the FuncBody (simulating a ClientSideState edit).
	page.FuncBody = "x := 999"
	newRendered := h.renderWasmMainBytesForTest(page, page.FuncBody)
	newHash := h.pageInputHash(page, newRendered)
	if newHash == originalHash {
		t.Fatal("expected pageInputHash to change after FuncBody change")
	}
	if h.cache.upToDate(page.OutputName, newHash) {
		t.Error("expected cache to be stale after FuncBody change → build must not be skipped")
	}
}

func TestBuildTopicManager_CacheHitSkipsBuild(t *testing.T) {
	setupTopicProject(t)
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	h := DefaultWasmHelper()
	h.cache = loadWasmCache()

	s := structInfo{Name: "Page", KeyName: "page", Compression: WasmCompressionGzip}
	wasmName := "topic-" + s.KeyName
	outFile := filepath.Join(outDir, wasmName+".wasm.gz")
	if err := os.WriteFile(outFile, []byte("prebuilt-topic"), 0644); err != nil {
		t.Fatalf("write out: %v", err)
	}
	hash := h.topicManagerInputHash(s)
	h.cache.update(wasmName, hash)

	if err := h.buildTopicManager(s, nil, []structInfo{s}, map[string]string{}, nil, outDir, &sync.Once{}); err != nil {
		t.Fatalf("buildTopicManager cache-hit: %v", err)
	}
	if data, _ := os.ReadFile(outFile); string(data) != "prebuilt-topic" {
		t.Errorf("expected prebuilt topic file untouched, got %q", data)
	}
}
