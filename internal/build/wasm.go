package helpers

import (
	"runtime"
	"sync/atomic"

	"github.com/gothicframework/cli/v3/internal/build/astx"
	helpers "github.com/gothicframework/core/render"
)

// wasmBuildCounts holds atomic counters for GenerateAll.
// Behind a pointer so WasmHelper stays copyable.
type wasmBuildCounts struct {
	upToDate atomic.Int32
	built    atomic.Int32
}

// tinyGoVersion is the default TinyGo toolchain (overridable via
// WasmTinyGoVersion). A -gothic.<n> suffix means a build of github.com/tinygo-org/tinygo/pull/5545
// that upstream has not released yet, downloaded from the fork rather than from
// tinygo-org; see wasm_binary.go and docs/patched-tinygo-channel.md. It carries the
// syscall/js finalizers, the idle-point finalizer-pressure GC that drains them, and
// the per-block registration bitmap that keeps registering one O(1). Swap this for
// the plain upstream version the moment a release contains that work.
const tinyGoVersion = "0.42.0-gothic.4"
const binaryenVersion = "117"

// ResolveTinyGoVersion returns the effective TinyGo toolchain version for a
// project: the gothic.config.go WasmTinyGoVersion pin when set, otherwise the
// bundled default. Every decision that keys off the toolchain version MUST
// resolve through this, so the pinned and the default paths agree with what the
// build actually compiles.
func ResolveTinyGoVersion(configVersion string) string {
	if configVersion != "" {
		return configVersion
	}
	return tinyGoVersion
}

// WasmHelper manages the TinyGo toolchain and compiles WASM pages.
// It follows the same struct + method pattern as TailwindHelper and FileBasedRouteHelper.
type WasmHelper struct {
	Template        helpers.TemplateHelper
	Runtime         string
	Arch            string
	Version         string
	BinaryenVersion string
	ConfigOverride  string
	// overrideRoot caches the TINYGOROOT that matches ConfigOverride's tinygo
	// binary. Populated single-threaded by EnsureBinary before any parallel
	// build starts, then read-only. Empty when no override is configured.
	overrideRoot string
	cache        *wasmCache
	astLoader    *astx.Loader

	// DevShaping controls compression level and wasm-opt pass for development builds.
	DevShaping bool
	// QuietSummary suppresses the aggregate "N up to date, M rebuilt" line so a
	// caller that times the whole stage can print it once with the duration.
	// Without it a hot-reload cycle prints the aggregate twice.
	QuietSummary bool
	// counts holds atomic build counters behind a pointer so WasmHelper
	// stays copyable (avoids go vet warnings when passed by value).
	counts *wasmBuildCounts
}

// WasmCompression is the compression algorithm for compiled WASM output.
// Mirrors routes.CompressionMethod to avoid a circular import with the helpers/routes package.
type WasmCompression int

const (
	WasmCompressionGzip   WasmCompression = iota // default (routes.GZIP == 0)
	WasmCompressionBrotli WasmCompression = iota // routes.BROTLI == 1
)

// WasmCompilerChoice mirrors routes.WasmCompiler to avoid circular imports.
type WasmCompilerChoice int

const (
	WasmCompilerGothicTinyGo WasmCompilerChoice = iota // default
	WasmCompilerLocalTinyGo
	WasmCompilerGolang
)

// WasmPage describes a single page that has a WASM state function.
type WasmPage struct {
	SourceFile  string
	FuncName    string
	FuncBody    string
	Imports     []string
	Helpers     []string
	HttpPath    string
	OutputName  string
	Compression WasmCompression
	Compiler    WasmCompilerChoice
	IsComponent bool // true when scanned from componentsDir, false for pagesDir
	// LocalPackageDirs lists absolute directories of local (user-module)
	// packages whose helpers/types are referenced by this page. Used by the
	// WASM cache to invalidate when a transitively imported local package
	// changes on disk. Sorted alphabetically and de-duplicated by the scanner.
	LocalPackageDirs []string
	// UsedDeclSources contains the formatted Go source of each AST declaration
	// (func/const/type) that the page's ClientSideState body transitively
	// references in its own package. Sorted alphabetically for hash stability.
	// Used by the WASM cache to invalidate only when a referenced symbol's
	// source actually changes, rather than any file in the package.
	UsedDeclSources []string
	// Multiplexed reflects RouteConfig.Multiplexed: when true the generated
	// main() registers the ClientSideState body via GothicRegisterScope so one
	// instance serves every placement of this route's component.
	Multiplexed bool
	// JSONDecodeTypes holds the reflection-free JSON reader structs for every
	// struct type reachable from a Decode[T] call in this page's ClientSideState,
	// deduplicated by identifier. These are extracted via go/types during
	// scanning (while the loader's type info is live) and consumed later by
	// writeWasmMain. Nil when the page makes no Decode[T] call, tree-shaking: no
	// Decode, no generated decoder, no runtime-parser cost.
	JSONDecodeTypes []jsonReaderType
	// JSONDecodeRoots holds the (Ident, GoType) of each top-level Decode[T] type
	// argument: one _jsonDecode_<Ident> is generated per root, and Decode[T] call
	// sites are rewritten to it. Nil when the page makes no Decode[T] call.
	JSONDecodeRoots []jsonRootRef
	// JSONEncodeTypes / JSONEncodeRoots are the Encode[T] mirror of the two fields
	// above: the reachable writer structs and the per-root refs for the
	// _jsonWrite_<Ident> / _jsonEncode_<Ident> functions. Nil when the page makes
	// no Encode[T] call.
	JSONEncodeTypes []jsonReaderType
	JSONEncodeRoots []jsonRootRef
}

func NewWasmHelper(goos, goarch string) WasmHelper {
	return WasmHelper{
		Template:        helpers.NewTemplateHelper(),
		Runtime:         goos,
		Arch:            goarch,
		Version:         tinyGoVersion,
		BinaryenVersion: binaryenVersion,
		counts:          &wasmBuildCounts{},
	}
}

// RebuiltCount returns the number of WASM pages that were actually compiled
// (not served from cache) in the last GenerateAll call. Returns 0 when no
// GenerateAll has completed or when the counts struct has been reset.
func (h *WasmHelper) RebuiltCount() int32 {
	if h.counts == nil {
		return 0
	}
	return h.counts.built.Load()
}

// UpToDateCount returns the number of WASM pages served from cache in the last
// GenerateAll call, so a caller printing its own aggregate reports the same
// numbers GenerateAll counted rather than deriving them from a page total.
func (h *WasmHelper) UpToDateCount() int32 {
	if h.counts == nil {
		return 0
	}
	return h.counts.upToDate.Load()
}

// DefaultWasmHelper creates a WasmHelper using the current runtime's OS and architecture.
func DefaultWasmHelper() WasmHelper {
	return NewWasmHelper(runtime.GOOS, runtime.GOARCH)
}
