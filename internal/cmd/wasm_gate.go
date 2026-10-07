package cmd

import (
	wasmhelper "github.com/gothicframework/cli/v4/internal/build"
	buildctl "github.com/gothicframework/cli/v4/internal/buildctl"
)

// ── WASM input gate (delegating) ──────────────────────────────────────────
//
// The gate's canonical implementation lives in internal/buildctl — it is the
// freshness signal of the BuildWasm tool and of BuildStatus, and the
// hot-reload stage must compare the same digest file. These delegations keep
// the package-local names the stage code and tests use.

const (
	wasmDigestPath     = buildctl.DigestPath
	wasmBuildCachePath = buildctl.BuildCachePath
)

// wasmDigestData is the on-disk format under .gothicCli/wasm-digest.json.
type wasmDigestData = buildctl.DigestData

// wasmInputSnapshot is the gate reading taken BEFORE a build: the digest of
// the inputs the build is about to consume, the dirs it was computed over,
// and whether it differs from the last recorded build.
type wasmInputSnapshot struct {
	digest  string
	dirs    []string
	changed bool
}

// takeWasmInputSnapshot reads the gate. Fail-open: on any error the stage runs.
func takeWasmInputSnapshot() wasmInputSnapshot {
	stored := buildctl.LoadDigest()
	if stored == nil {
		return wasmInputSnapshot{changed: true}
	}
	current := buildctl.ComputeDigest(stored.LocalPackageDirs)
	return wasmInputSnapshot{
		digest:  current,
		dirs:    stored.LocalPackageDirs,
		changed: current != stored.Digest,
	}
}

// wasmInputChanged reports whether any WASM-stage input changed since the last
// recorded build, for callers that only read the gate and never record.
func wasmInputChanged() bool {
	return buildctl.InputChanged()
}

// recordWasmDigestNow records a digest read at call time. A build stage must
// NOT use this: it cannot tell a file the build compiled from one saved while
// it ran. Use recordWasmDigestFor there.
func recordWasmDigestNow(localDirs []string) {
	buildctl.RecordDigestNow(localDirs)
}

// recordWasmDigestFor persists the digest of what the build actually consumed
// (the pre-build snapshot, not a fresh reading — see buildctl.RecordDigestFor).
func recordWasmDigestFor(snap wasmInputSnapshot, localDirs []string) {
	buildctl.RecordDigestFor(buildctl.Snapshot{
		Digest:  snap.digest,
		Dirs:    snap.dirs,
		Changed: snap.changed,
	}, localDirs)
}

// loadWasmDigest reads the stored digest. Returns nil on any error.
func loadWasmDigest() *wasmDigestData {
	return buildctl.LoadDigest()
}

// computeWasmDigest returns a hex-encoded SHA-256 digest of everything the
// WASM scan and generate stages consume.
func computeWasmDigest(localDirs []string) string {
	return buildctl.ComputeDigest(localDirs)
}

// collectWasmLocalDirs unions the LocalPackageDirs fields of all pages and
// returns them sorted. Returns nil when no pages or no dirs.
func collectWasmLocalDirs(pages []wasmhelper.WasmPage) []string {
	return buildctl.CollectLocalDirs(pages)
}
