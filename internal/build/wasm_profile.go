package helpers

// Per-version TinyGo capability profile.
//
// A patched TinyGo build can change which runtime the browser needs. The most
// important case: whether the wasm_exec shim must carry Gothic's manual GC
// reclamation (the bundled 0.41.1 has NO syscall/js finalizers, so it must) or
// the STOCK shim (a patched/newer TinyGo WITH finalizers must, because the
// manual reclamation would double-free — a use-after-free under concurrent
// callbacks). This capability is a property of the specific toolchain BUILD and
// MUST NEVER be inferred from the version number: a future bare 0.42.0 could
// ship with or without the fix, so an unverified version always gets the safe
// default. See cli/docs/patched-tinygo-channel.md.

// toolchainProfile describes the runtime capabilities of a resolved TinyGo
// toolchain. It is deliberately a struct (not a bool) so future per-patch
// toggles can be added as fields without touching every call site.
type toolchainProfile struct {
	// StockWasmExec selects the stock wasm_exec shim (no manual GC reclamation)
	// over Gothic's manual-GC default. True ONLY for toolchains VERIFIED to
	// provide syscall/js finalizers.
	StockWasmExec bool
}

// knownToolchainProfiles maps a resolved TinyGo version to its VERIFIED
// capability profile. A version is listed here only after a human has confirmed
// the toolchain's actual runtime behavior.
//
// MIGRATION: when an official TinyGo release is verified to contain syscall/js
// finalizers (upstream PR #5521), add its version here with StockWasmExec:true
// and retire the -gothic.* fork pin (pin the official version instead). Do the
// same, with StockWasmExec:false, for any official release confirmed to still
// lack finalizers. NEVER guess from the version — verify, then add a row.
var knownToolchainProfiles = map[string]toolchainProfile{
	// Bundled default: 0.41.1's SetFinalizer is a no-op → syscall/js never
	// finalizes → the manual-GC shim is REQUIRED.
	"0.41.1": {StockWasmExec: false},
	// Patched fork build carrying upstream PR #5521 (real finalizers) → the
	// manual-GC shim would double-free, so the STOCK shim is required.
	"0.41.1-gothic.1": {StockWasmExec: true},
	// PR #5521 finalizers plus the follow-up (upstream PR #5545) that collects
	// on finalizer pressure at the scheduler's idle point, so js.Value slots are
	// reclaimed without a manual runtime.GC(). Still finalizer-carrying → STOCK
	// shim. Built on the 0.42.0 dev line (where #5521 landed).
	"0.42.0-gothic.1": {StockWasmExec: true},
	// The #5545 build with the maintainer's review feedback applied (per-goroutine
	// finishing flag, documented scheduler partition, expanded threshold doc).
	// Behaviorally identical to gothic.1 → STOCK shim.
	"0.42.0-gothic.2": {StockWasmExec: true},
}

// ProfileFor returns the VERIFIED capability profile for a resolved TinyGo
// version, or the SAFE DEFAULT for any unknown/unverified version. The safe
// default is the manual-GC runtime (StockWasmExec:false): on the bundled 0.41.1
// it is correct, and on any not-yet-verified toolchain it can at worst leave a
// slow leak — never the use-after-free crash that a wrongly-assumed stock shim
// would cause. A wrong or missing row therefore fails safe.
func ProfileFor(version string) toolchainProfile {
	if p, ok := knownToolchainProfiles[version]; ok {
		return p
	}
	return toolchainProfile{StockWasmExec: false}
}

// StockWasmExec reports whether this helper's resolved TinyGo version wants the
// stock wasm_exec shim. The CLI translates this into the GOTHIC_WASM_EXEC=stock
// signal it hands the server it launches (hot-reload) or the Lambda it deploys,
// so the runtime serves the matching shim.
func (h *WasmHelper) StockWasmExec() bool {
	return ProfileFor(h.Version).StockWasmExec
}

// WasmExecEnvKey is the environment variable a Gothic SERVER reads once at
// process start to decide which TinyGo wasm_exec shim to serve at
// /_gothic/wasm_exec.js. Its only meaningful value is "stock"; unset (the
// default) means the manual-GC shim. Kept here as the single source of truth
// shared by the CLI (which sets it) and core/wasmexec (which reads it).
const WasmExecEnvKey = "GOTHIC_WASM_EXEC"

// WasmExecEnviron returns the env entries the CLI must add to a Gothic SERVER
// process so it serves the wasm_exec shim matching this toolchain: a single
// GOTHIC_WASM_EXEC=stock entry when the stock shim is required, else nil (the
// server defaults to the manual-GC shim). This is how the build-time capability
// decision reaches the separate server process.
func (h *WasmHelper) WasmExecEnviron() []string {
	if h.StockWasmExec() {
		return []string{WasmExecEnvKey + "=stock"}
	}
	return nil
}
