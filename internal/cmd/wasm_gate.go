package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wasmhelper "github.com/gothicframework/cli/v3/internal/build"
)

// ── WASM input gate ────────────────────────────────────────────────────────
//
// wasmInputChanged / recordWasmDigest form a content-based gate that skips the
// entire WASM stage (PregenerateTopicStubs + ScanPages + GenerateAll) when no
// input the stage consumes has changed.
//
// The digest covers:
//   - go.mod and go.sum
//   - every .go and _templ.go under src/pages, src/components, src/topics
//     (so adding/removing ClientSideState flips it via the _templ.go content)
//   - every hand-written .go file in the local helper packages the previous scan
//     discovered (persisted as localPackageDirs in the digest file)
//   - the CLI binary identity (size + mtime), which stands in for the embedded
//     runtime FS, embedded templates, and build recipe, all compiled into the
//     binary and none of which change when the binary is not replaced
//
// Fail open: any error reading or computing the digest causes the stage to run.

const wasmDigestPath = ".gothicCli/wasm-digest.json"

// wasmBuildCachePath mirrors the build package's own constant. It is read here,
// never written, so the gate can tell that something outside this session
// rewrote the artifacts.
const wasmBuildCachePath = ".gothicCli/wasm-cache.json"

// wasmDigestData is the on-disk format under .gothicCli/wasm-digest.json.
type wasmDigestData struct {
	Digest           string   `json:"digest"`
	LocalPackageDirs []string `json:"localPackageDirs,omitempty"`
}

// wasmInputChanged returns true when any WASM-stage input has changed since the
// last recorded successful build. Fail-open: on any error the stage runs.
func wasmInputChanged() bool {
	stored := loadWasmDigest()
	if stored == nil {
		return true
	}
	current := computeWasmDigest(stored.LocalPackageDirs)
	return current != stored.Digest
}

// recordWasmDigest persists a new digest after a successful WASM build.
// Errors are silently ignored; the next cycle runs the stage.
func recordWasmDigest(localDirs []string) {
	d := wasmDigestData{
		Digest:           computeWasmDigest(localDirs),
		LocalPackageDirs: localDirs,
	}
	if err := os.MkdirAll(filepath.Dir(wasmDigestPath), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(wasmDigestPath, data, 0o644)
}

// loadWasmDigest reads the stored digest. Returns nil on any error.
func loadWasmDigest() *wasmDigestData {
	data, err := os.ReadFile(wasmDigestPath)
	if err != nil {
		return nil
	}
	var d wasmDigestData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	if d.Digest == "" {
		return nil
	}
	return &d
}

// computeWasmDigest returns a hex-encoded SHA-256 digest of everything the
// WASM scan and generate stages consume.
func computeWasmDigest(localDirs []string) string {
	h := sha256.New()

	// Module definition and dependency lock.
	feedFileContent(h, "go.mod")
	feedFileContent(h, "go.sum")

	// Source files under the watched source directories.
	for _, dir := range []string{"src/pages", "src/components", "src/topics"} {
		hashWatchedDir(h, dir)
	}

	// Local helper packages from the previous scan.
	seen := make(map[string]bool)
	for _, dir := range localDirs {
		abs := dir
		if !filepath.IsAbs(abs) {
			var err error
			abs, err = filepath.Abs(abs)
			if err != nil {
				continue
			}
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		hashHandwrittenPackageDir(h, abs)
	}

	// CLI binary identity, proxies embedded templates, runtime FS, recipe.
	hashCLIBinary(h)

	// The per-page cache, which is the record of what is actually on disk and
	// how it was shaped. Every other input here is a source file, so the gate
	// would otherwise miss the one way the artifacts can change with no source
	// changing: `gothic wasm` or a deploy rewrites them at production shaping
	// and rewrites this file. Skipping on a stale digest would then defer the
	// mismatch to the developer's first real edit, turning a one-line change
	// into a rebuild of every unit. Feeding it in moves that rebuild to session
	// start, where it is expected.
	feedFileContent(h, wasmBuildCachePath)

	return hex.EncodeToString(h.Sum(nil))
}

// hashWatchedDir hashes every .go file (including _templ.go) under dir,
// recursing into subdirectories. Entries are sorted for hash stability.
func hashWatchedDir(h io.Writer, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files, subdirs []string
	for _, e := range entries {
		if e.IsDir() {
			subdirs = append(subdirs, e.Name())
		} else if strings.HasSuffix(e.Name(), ".go") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(subdirs)
	sort.Strings(files)

	for _, sd := range subdirs {
		hashWatchedDir(h, filepath.Join(dir, sd))
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		io.WriteString(h, dir)
		h.Write([]byte{'/'})
		io.WriteString(h, name)
		h.Write([]byte{0})
		h.Write(data)
	}
}

// hashHandwrittenPackageDir hashes hand-written .go files in dir, skipping
// _templ.go, _gen.go, and _test.go. Non-recursive: each local package is a
// single Go directory discovered by the scanner.
func hashHandwrittenPackageDir(h io.Writer, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_templ.go") ||
			strings.HasSuffix(name, "_gen.go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		io.WriteString(h, dir)
		h.Write([]byte{'/'})
		io.WriteString(h, name)
		h.Write([]byte{0})
		h.Write(data)
	}
}

// hashCLIBinary feeds the running CLI binary's identity into h: the executable
// path and its size + mtime. This catches embedded template, runtime FS, and
// recipe changes because all are compiled into the binary.
func hashCLIBinary(h io.Writer) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	io.WriteString(h, exe)
	h.Write([]byte{0})
	fi, err := os.Stat(exe)
	if err != nil {
		return
	}
	io.WriteString(h, fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano()))
}

func feedFileContent(h io.Writer, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	io.WriteString(h, path)
	h.Write([]byte{0})
	h.Write(data)
}

// collectWasmLocalDirs unions the LocalPackageDirs fields of all pages and
// returns them sorted. Returns nil when no pages or no dirs.
func collectWasmLocalDirs(pages []wasmhelper.WasmPage) []string {
	seen := make(map[string]struct{})
	for _, p := range pages {
		for _, d := range p.LocalPackageDirs {
			seen[d] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}
