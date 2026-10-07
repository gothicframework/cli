package buildctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	helpers "github.com/gothicframework/cli/v4/internal/build"
)

// ── WASM input gate ───────────────────────────────────────────────────────
//
// This is the content-based gate that skips the entire WASM stage
// (PregenerateTopicStubs + ScanPages + GenerateAll) when no input the stage
// consumes has changed. It is the canonical implementation; the hot-reload
// command package reads it through thin wrappers so both layers compare the
// same digest file and can never disagree about freshness.
//
// The digest covers:
//   - go.mod and go.sum
//   - every .go and _templ.go under src/pages, src/components, src/topics
//     (so adding/removing ClientSideState flips it via the _templ.go content)
//   - every hand-written .go file in the local helper packages the previous
//     scan discovered (persisted as LocalPackageDirs in the digest file)
//   - the CLI binary identity (size + mtime), which stands in for the
//     embedded runtime FS, embedded templates, and build recipe, all compiled
//     into the binary and none of which change when the binary is not replaced
//
// Fail open: any error reading or computing the digest causes the stage to run.

// DigestPath is the on-disk location of the WASM input digest.
const DigestPath = ".gothicCli/wasm-digest.json"

// BuildCachePath mirrors the build package's own per-unit cache. It is read
// here, never written, so the gate can tell that something outside this
// session rewrote the artifacts.
const BuildCachePath = ".gothicCli/wasm-cache.json"

// DigestData is the on-disk format under DigestPath.
type DigestData struct {
	Digest           string   `json:"digest"`
	LocalPackageDirs []string `json:"localPackageDirs,omitempty"`
}

// Snapshot is the gate reading taken BEFORE a build: the digest of the inputs
// the build is about to consume, the dirs it was computed over, and whether
// it differs from the last recorded build.
type Snapshot struct {
	Digest  string
	Dirs    []string
	Changed bool
}

// TakeSnapshot reads the gate. Fail-open: on any error the stage runs.
func TakeSnapshot() Snapshot {
	stored := LoadDigest()
	if stored == nil {
		// No previous build: the reading is computed over the default file
		// set (no discovered local packages yet). A stage recording this
		// snapshot still stores the inputs the build is about to consume,
		// not a fresh reading taken after it finished.
		return Snapshot{Digest: ComputeDigest(nil), Changed: true}
	}
	current := ComputeDigest(stored.LocalPackageDirs)
	return Snapshot{
		Digest:  current,
		Dirs:    stored.LocalPackageDirs,
		Changed: current != stored.Digest,
	}
}

// InputChanged reports whether any WASM-stage input changed since the last
// recorded build, for callers that only read the gate and never record.
func InputChanged() bool {
	return TakeSnapshot().Changed
}

// RecordDigestNow records a digest read at call time. A build stage must NOT
// use this: it cannot tell a file the build compiled from one saved while it
// ran. Use RecordDigestFor there.
func RecordDigestNow(localDirs []string) {
	WriteDigest(ComputeDigest(localDirs), localDirs)
}

// RecordDigestFor persists the digest of what the build actually consumed.
//
// It records the snapshot taken BEFORE the build, not a fresh reading. A save
// landing while the stage is running is the whole reason: recomputing here
// would store a digest describing content this build never compiled, and the
// next cycle would compare against it, see no change, and skip the stage
// forever, leaving a stale binary with no output saying so. Measured: an edit
// made during the 25s initial rebuild left the page running the previous
// logic until an unrelated file was touched.
//
// A changed local-package set is the one case that has to recompute: the
// snapshot was taken over a different file set, so it does not describe these
// inputs either. That costs one extra rebuild, which is the safe direction.
func RecordDigestFor(snap Snapshot, localDirs []string) {
	digest := snap.Digest
	if digest == "" || !SameDirs(snap.Dirs, localDirs) {
		digest = ComputeDigest(localDirs)
	}
	WriteDigest(digest, localDirs)
}

func SameDirs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func WriteDigest(digest string, localDirs []string) {
	d := DigestData{
		Digest:           digest,
		LocalPackageDirs: localDirs,
	}
	if err := os.MkdirAll(filepath.Dir(DigestPath), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(DigestPath, data, 0o644)
}

// LoadDigest reads the stored digest. Returns nil on any error.
func LoadDigest() *DigestData {
	data, err := os.ReadFile(DigestPath)
	if err != nil {
		return nil
	}
	var d DigestData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	if d.Digest == "" {
		return nil
	}
	return &d
}

// ComputeDigest returns a hex-encoded SHA-256 digest of everything the
// WASM scan and generate stages consume.
func ComputeDigest(localDirs []string) string {
	h := sha256.New()

	// Module definition and dependency lock.
	feedFileContent(h, "go.mod")
	feedFileContent(h, "go.sum")

	// Source files under the watched source directories.
	for _, dir := range []string{pagesDir, componentsDir, "src/topics"} {
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

	// The per-unit cache, which is the record of what is actually on disk and
	// how it was shaped. Every other input here is a source file, so the gate
	// would otherwise miss the one way the artifacts can change with no source
	// changing: `gothic wasm` or a deploy rewrites them at production shaping
	// and rewrites this file. Skipping on a stale digest would then defer the
	// mismatch to the developer's first real edit, turning a one-line change
	// into a rebuild of every unit. Feeding it in moves that rebuild to session
	// start, where it is expected.
	feedFileContent(h, BuildCachePath)

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

// hashCLIBinary feeds the running CLI binary's identity into h: the
// executable path and its size + mtime. This catches embedded template,
// runtime FS, and recipe changes because all are compiled into the binary.
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

// ── WASM stage ────────────────────────────────────────────────────────────

// bodyWasm guards the runner with the digest gate, so any runner — the
// production one or a test's — only ever runs when a WASM input changed
// since the last recorded build. The snapshot is taken here, BEFORE the
// runner executes, and handed to it: the stage's digest record must describe
// the inputs the build is about to consume, not whatever is on disk by the
// time the build finishes (a save landing mid-compile would otherwise be
// absorbed into the record and the next cycle would skip the stage, leaving
// a stale binary served silently).
func (c *Controller) bodyWasm(target string) (BuildResult, error) {
	snap := TakeSnapshot()
	if !snap.Changed {
		return BuildResult{}, nil
	}
	return c.WasmRun(target, snap)
}

// realWasmStage is the full WASM stage: topic stubs → scan (+ tidy retry) →
// incremental generate → digest record. DevShaping and QuietSummary are
// caller-owned settings on the WasmHelper; the stage does not touch them.
// snap is the pre-build gate reading bodyWasm took before calling in.
func (c *Controller) realWasmStage(target string, snap Snapshot) (BuildResult, error) {
	var res BuildResult
	// Topic stubs must exist before the scan: pages that call a topic
	// accessor cannot type-check until topic_gen.go is on disk.
	c.cli.Wasm.PregenerateTopicStubs()
	pages, err := c.scanWithTidy()
	if err != nil {
		return res, err
	}
	var nPages, nComponents int
	for _, p := range pages {
		if p.IsComponent {
			nComponents++
		} else {
			nPages++
		}
	}
	// The inventory line only announces an inventory that exists; the no-pages
	// case records its digest and stays silent, as the stage always has.
	if len(pages) > 0 && c.opts.WasmInventory != nil {
		c.opts.WasmInventory(nPages, nComponents, c.cli.Wasm.CountTopics())
	}

	before := statWasmArtifacts()
	err = c.cli.Wasm.GenerateAll(pages, wasmOutDir)
	if err != nil {
		c.wasmError("build failed (continuing with stale binaries): %v", err)
		// No digest record: the next cycle re-runs the stage for the same
		// inputs, which is the safe direction.
		return res, err
	}
	// Persist the pre-build snapshot only after success: a save landing
	// mid-build must not be recorded as compiled content. The recorded
	// digest is the one taken before the stage started, so the next cycle
	// still sees the mid-build save as a change.
	RecordDigestFor(snap, collectLocalDirs(pages))

	after := statWasmArtifacts()
	res.Recompiled = c.wasmRecompiled(before, after, pages)
	res.Counts = &StageCounts{
		Rebuilt:  int(c.cli.Wasm.RebuiltCount()),
		UpToDate: int(c.cli.Wasm.UpToDateCount()),
	}
	return res, nil
}

// scanWithTidy scans the routed folders, retrying once after `go mod tidy`
// when the scan reports a stale go.mod (a new import added to a page).
func (c *Controller) scanWithTidy() ([]helpers.WasmPage, error) {
	pages, err := c.cli.Wasm.ScanPages(pagesDir, componentsDir)
	if err == nil {
		return pages, nil
	}
	if strings.Contains(err.Error(), "go mod tidy") || strings.Contains(err.Error(), "updates to go.mod needed") {
		c.wasmLog("go.mod out of date, running go mod tidy...")
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Stderr = os.Stderr
		if tidyErr := tidy.Run(); tidyErr != nil {
			c.wasmError("go mod tidy failed: %v", tidyErr)
			return nil, tidyErr
		}
		return c.cli.Wasm.ScanPages(pagesDir, componentsDir)
	}
	c.wasmError("scan failed: %v", err)
	return nil, err
}

func (c *Controller) wasmLog(format string, args ...any) {
	if c.opts.WasmLogf != nil {
		c.opts.WasmLogf(format, args...)
	}
}

func (c *Controller) wasmError(format string, args ...any) {
	if c.opts.WasmErrorf != nil {
		c.opts.WasmErrorf(format, args...)
	}
}

// wasmArtifact is the on-disk state of one compiled WASM artifact.
type wasmArtifact struct {
	size  int64
	mtime time.Time
}

// statWasmArtifacts snapshots the compiled WASM inventory (file → size/mtime).
func statWasmArtifacts() map[string]wasmArtifact {
	out := make(map[string]wasmArtifact)
	entries, err := os.ReadDir(wasmOutDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = wasmArtifact{size: info.Size(), mtime: info.ModTime()}
	}
	return out
}

// wasmRecompiled diffs the artifact inventory across the build. The build
// package announces units itself, so the diff is how the structured result
// learns which units actually rebuilt.
func (c *Controller) wasmRecompiled(before, after map[string]wasmArtifact, pages []helpers.WasmPage) []Recompiled {
	byName := make(map[string]helpers.WasmPage, len(pages))
	for _, p := range pages {
		byName[p.OutputName] = p
	}
	var out []Recompiled
	for name, afterInfo := range after {
		beforeInfo, existed := before[name]
		if existed && beforeInfo.size == afterInfo.size && beforeInfo.mtime.Equal(afterInfo.mtime) {
			continue
		}
		path := "wasm:" + name
		if p, ok := byName[wasmUnitName(name)]; ok && p.HttpPath != "" {
			path = "wasm:" + p.HttpPath
		}
		out = append(out, Recompiled{Path: path, Bytes: afterInfo.size})
	}
	sortRecompiled(out)
	return out
}

// wasmUnitName strips the compression suffix from an artifact file name.
func wasmUnitName(fileName string) string {
	for _, suffix := range []string{".wasm.gz", ".wasm.br"} {
		if strings.HasSuffix(fileName, suffix) {
			return strings.TrimSuffix(fileName, suffix)
		}
	}
	return strings.TrimSuffix(fileName, filepath.Ext(fileName))
}

func sortRecompiled(out []Recompiled) {
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
}
