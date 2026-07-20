package helpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	wasmexec "github.com/gothicframework/core/wasmexec"
	wasmruntime "github.com/gothicframework/core/wasm"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// Build pipeline: orchestrates TinyGo compile + wasm-opt + compression for
// page WASMs and topic-manager WASMs, plus the wasm_exec.js copy. Methods
// here run on the host (not inside WASM).

// Template source paths inside the embedded FS (WasmTemplateFS). The CLI no
// longer reads these from the user's project tree — they are an implementation
// detail of the build pipeline and ship inside the binary.
const (
	tmplWasmPageMain     = EmbeddedTmplWasmPageMain
	tmplTopicManagerMain = EmbeddedTmplTopicManagerMain
)

func (h *WasmHelper) GeneratePage(page WasmPage, outDir string, warnOnce *sync.Once) error {
	compressedExt := compressionExt(page.Compression)
	var hash string
	if h.cache != nil {
		hash = h.pageInputHash(page)
		outPath := filepath.Join(outDir, page.OutputName+".wasm"+compressedExt)
		if h.cache.upToDate(page.OutputName, hash) {
			if _, err := os.Stat(outPath); err == nil {
				wasmUpToDate(page.OutputName)
				return nil
			}
		}
	}
	// Remove stale files from any previous compression method.
	for _, ext := range []string{".gz", ".br"} {
		if ext != compressedExt {
			os.Remove(filepath.Join(outDir, page.OutputName+".wasm"+ext))
		}
	}

	tempModDir, err := os.MkdirTemp("", "tinygo-runtime-*")
	if err != nil {
		return fmt.Errorf("wasm: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tempModDir)

	if err := wasmruntime.ExtractRuntime(tempModDir); err != nil {
		return fmt.Errorf("wasm: extract runtime: %w", err)
	}
	if err := h.writeModuleBridge(tempModDir); err != nil {
		return err
	}

	genDir, err := os.MkdirTemp(tempModDir, ".gen-")
	if err != nil {
		return fmt.Errorf("wasm: mkdirtemp gen: %w", err)
	}

	mainPath := filepath.Join(genDir, "main.go")
	topicSnippets, topicStructs, topicAliases, topicRefAliases := h.collectTopicSnippets()
	body, err := h.rewriteTopicCalls(page.FuncBody, topicStructs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wasm: rewrite topic calls %s: %v\n", page.SourceFile, err)
		os.Exit(1)
	}
	// Rewrite Decode[T](resp) → _jsonDecode_<Ident>(resp). The runtime
	// build has no Decode symbol, so this must run for every page that calls
	// Decode[T].
	if len(page.JSONDecodeRoots) > 0 {
		rootIdents := make(map[string]bool, len(page.JSONDecodeRoots))
		for _, r := range page.JSONDecodeRoots {
			rootIdents[r.Ident] = true
		}
		body, err = h.rewriteDecodeCalls(body, rootIdents)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wasm: rewrite decode calls %s: %v\n", page.SourceFile, err)
			os.Exit(1)
		}
	}
	// Rewrite Encode[T](v) → _jsonEncode_<Ident>(v).
	if len(page.JSONEncodeRoots) > 0 {
		rootIdents := make(map[string]bool, len(page.JSONEncodeRoots))
		for _, r := range page.JSONEncodeRoots {
			rootIdents[r.Ident] = true
		}
		body, err = h.rewriteEncodeCalls(body, rootIdents)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wasm: rewrite encode calls %s: %v\n", page.SourceFile, err)
			os.Exit(1)
		}
	}
	if err := h.writeWasmMain(page.SourceFile, body, page.Imports, page.Helpers, topicSnippets, topicStructs, topicAliases, topicRefAliases, page.JSONDecodeTypes, page.JSONDecodeRoots, page.JSONEncodeTypes, page.JSONEncodeRoots, page.Multiplexed, mainPath); err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir %s: %w", outDir, err)
	}

	absOutFile, err := filepath.Abs(filepath.Join(outDir, page.OutputName+".wasm"))
	if err != nil {
		return err
	}

	pkg := "./" + filepath.Base(genDir) + "/"
	cmd, err := h.buildCommandForCompiler(page.Compiler, pkg, absOutFile, tempModDir, warnOnce)
	if err != nil {
		return err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := runWithVendorFallback(cmd, tempModDir, func() (*exec.Cmd, error) {
		return h.buildCommandForCompiler(page.Compiler, pkg, absOutFile, tempModDir, warnOnce)
	}); err != nil {
		return fmt.Errorf("wasm: build %s (%s): %w", page.OutputName, compilerLabel(page.Compiler), err)
	}

	wasmSize, _ := h.fileSize(absOutFile)

	wasmOpt := "wasm-opt"
	if _, err := exec.LookPath(wasmOpt); err != nil {
		wasmOpt = ""
		if managed := h.BinaryenBinary(); managed != "" {
			if _, err := os.Stat(managed); err == nil {
				wasmOpt = managed
			}
		}
	}

	if wasmOpt != "" {
		tmp := absOutFile + ".opt"
		opt := exec.Command(wasmOpt, "-Oz", "--strip-debug", "-o", tmp, absOutFile)
		if err := opt.Run(); err == nil {
			os.Rename(tmp, absOutFile)
		} else {
			os.Remove(tmp)
		}
	} else {
		if warnOnce != nil {
			warnOnce.Do(func() {
				wasmWarnf("wasm-opt not found; skipping manual optimization pass. Install Binaryen for smaller binaries.")
			})
		}
	}

	finalFile := absOutFile + compressedExt
	if err := h.compressWasmWith(absOutFile, finalFile, page.Compression); err != nil {
		return fmt.Errorf("wasm: compress %s: %w", page.OutputName, err)
	}
	os.Remove(absOutFile)

	finalSize, _ := h.fileSize(finalFile)
	wasmBuildResult(page.OutputName, h.formatBytes(wasmSize), h.formatBytes(finalSize), compressionLabel(page.Compression))
	if hash != "" {
		h.cache.update(page.OutputName, hash)
	}
	return nil
}

// buildCommandForCompiler returns the exec.Cmd that compiles the WASM package
// using the chosen compiler. tempModDir is the temp module root (contains
// go.mod with module name "wasm-runtime") and pkg is the relative import path
// ("./<genDir>/"). The runtime files use `//go:build js && wasm`, so both
// TinyGo's -target=wasm and standard Go's GOOS=js GOARCH=wasm satisfy them.
// tinygoWasmFlags and goWasmFlags/goWasmEnv are the stable, build-identifying
// arguments for each compiler — the flags that determine the produced WASM binary,
// excluding the per-build -o/pkg operands. They are the SINGLE source of truth: the
// build commands below are assembled from them, and buildRecipeFingerprint folds
// them into the WASM cache hash (see wasm_cache.go). This way a recipe change — e.g.
// the -gc conservative switch — can never silently reuse a stale cached binary.
var (
	tinygoWasmFlags = []string{"build", "-no-debug", "-opt=z", "-target", "wasm", "-gc", "precise"}
	goWasmFlags     = []string{"build", "-ldflags=-s -w", "-trimpath"}
	goWasmEnv       = []string{"GOOS=js", "GOARCH=wasm"}
)

// buildRecipeFingerprint returns a stable string identifying every compiler's build
// recipe. It is folded into the per-page and per-topic input hashes so a flag-only
// change (no runtime .go source change) still invalidates the cache and forces a
// rebuild. It intentionally covers all recipes, not just the page's chosen one:
// over-invalidating (rebuilding) is the safe direction.
func buildRecipeFingerprint() string {
	return "tinygo:" + strings.Join(tinygoWasmFlags, " ") +
		"|go:" + strings.Join(goWasmFlags, " ") + " " + strings.Join(goWasmEnv, " ")
}

// tinygoBuildArgs assembles the full TinyGo argv from the shared flags plus the
// per-build output path and package.
func tinygoBuildArgs(absOutFile, pkg string) []string {
	return append(append([]string{}, tinygoWasmFlags...), "-o", absOutFile, pkg)
}

func (h *WasmHelper) buildCommandForCompiler(choice WasmCompilerChoice, pkg, absOutFile, tempModDir string, warnOnce *sync.Once) (*exec.Cmd, error) {
	switch choice {
	case WasmCompilerLocalTinyGo:
		tinygo, err := exec.LookPath("tinygo")
		if err != nil {
			return nil, fmt.Errorf("wasm: WasmCompiler=LocalTinyGo but tinygo not found in PATH: %w", err)
		}
		cmd := exec.Command(tinygo, tinygoBuildArgs(absOutFile, pkg)...)
		cmd.Dir = tempModDir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		hasWasmOpt := false
		if _, err := exec.LookPath("wasm-opt"); err == nil {
			hasWasmOpt = true
		} else if b := h.BinaryenBinary(); b != "" {
			if _, err := os.Stat(b); err == nil {
				hasWasmOpt = true
				cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(b)+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
		}
		if !hasWasmOpt {
			cmd.Env = append(cmd.Env, "WASMOPT=false")
		}
		return cmd, nil

	case WasmCompilerGolang:
		goExe, err := exec.LookPath("go")
		if err != nil {
			return nil, fmt.Errorf("wasm: WasmCompiler=Golang but go not found in PATH: %w", err)
		}
		goArgs := append(append([]string{}, goWasmFlags...), "-o", absOutFile, pkg)
		cmd := exec.Command(goExe, goArgs...)
		cmd.Dir = tempModDir
		cmd.Env = append(os.Environ(), append(append([]string{}, goWasmEnv...), "GOWORK=off", "GOFLAGS=-mod=mod")...)
		return cmd, nil

	default: // WasmCompilerGothicTinyGo
		tinygo := h.TinyGoBinary()
		if h.ConfigOverride != "" {
			tinygo = h.ConfigOverride
		}
		cmd := exec.Command(tinygo, tinygoBuildArgs(absOutFile, pkg)...)
		cmd.Dir = tempModDir
		cmd.Env = append(os.Environ(), h.EnvironWithWarn(warnOnce)...)
		cmd.Env = append(cmd.Env, "GOWORK=off", "GOFLAGS=-mod=mod")
		return cmd, nil
	}
}

// writeModuleBridge writes a go.mod into tempModDir that links back to the
// user's project (CWD) so imports of user-project packages in the generated
// main.go can resolve. Called once per WASM build (per-page and topic-manager).
func (h *WasmHelper) writeModuleBridge(tempModDir string) error {
	const projectRoot = "."
	modulePath, goVersion, err := ReadUserModulePath(projectRoot)
	if err != nil {
		return fmt.Errorf("wasm: read user module: %w", err)
	}
	if err := WriteBridgeGoMod(tempModDir, modulePath, projectRoot, goVersion); err != nil {
		return fmt.Errorf("wasm: write bridge go.mod: %w", err)
	}
	return nil
}

// runWithVendorFallback runs cmd. If it fails, attempts `go mod vendor` inside
// tempModDir and retries by re-creating the command via rebuild. Returns the
// original error if the fallback also fails.
func runWithVendorFallback(cmd *exec.Cmd, tempModDir string, rebuild func() (*exec.Cmd, error)) error {
	origErr := cmd.Run()
	if origErr == nil {
		return nil
	}
	goExe, lookErr := exec.LookPath("go")
	if lookErr != nil {
		return origErr
	}
	vendor := exec.Command(goExe, "mod", "vendor")
	vendor.Dir = tempModDir
	vendor.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	vendor.Stdout = os.Stderr
	vendor.Stderr = os.Stderr
	if err := vendor.Run(); err != nil {
		return origErr
	}
	retry, err := rebuild()
	if err != nil {
		return origErr
	}
	retry.Stdout = os.Stdout
	retry.Stderr = os.Stderr
	if err := retry.Run(); err != nil {
		return origErr
	}
	return nil
}

func compilerLabel(c WasmCompilerChoice) string {
	switch c {
	case WasmCompilerLocalTinyGo:
		return "local tinygo"
	case WasmCompilerGolang:
		return "go (js/wasm)"
	default:
		return "embedded tinygo"
	}
}

// CountTopicManagers returns the number of topic structs that will produce a
// topic-manager WASM binary (i.e. structs that have a KeyName set).
func (h *WasmHelper) CountTopicManagers() int {
	_, structs, _, _ := h.collectTopicSnippets()
	n := 0
	for _, s := range structs {
		if s.KeyName != "" {
			n++
		}
	}
	return n
}

// pagesUseStandardGo returns true if any page uses the standard Go compiler.
// Such pages need the standard-Go wasm_exec.js, which is incompatible with TinyGo's.
func pagesUseStandardGo(pages []WasmPage) bool {
	for _, p := range pages {
		if p.Compiler == WasmCompilerGolang {
			return true
		}
	}
	return false
}

func (h *WasmHelper) GenerateAll(pages []WasmPage, outDir string) error {
	if err := h.EnsureBinary(); err != nil {
		return err
	}
	// One-time migration: remove any pre-v2.17 on-disk template copies the
	// project was seeded with. They are now embedded in the CLI binary and
	// the on-disk versions would be silently ignored, so we delete them so
	// users notice and so the path can never drift back into use.
	if err := CleanupLegacyTemplates("."); err != nil {
		return err
	}
	// The shared gothic-core.js runtime and the prebuilt full-Go static
	// core are NO LONGER copied into public/. They are served straight
	// from the framework embed via the /_gothic/ route (see pkg/helpers/runtimeassets
	// and pkg/server), so a framework upgrade updates them for every project with
	// no file churn and no stale copies.
	if len(pages) == 0 {
		return nil
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir %s: %w", outDir, err)
	}

	h.cache = loadWasmCache()

	warnOnce := &sync.Once{}

	// Topics no longer compile to a per-topic MANAGER WASM. The
	// always-loaded full-Go static core (served from the framework embed via the
	// /_gothic/ route) is now the single generic topic hub — it store-and-forwards
	// every topic's per-field
	// binary frames opaquely and replays state on join. Consumers self-register
	// their key + field names with the core at runtime (RegisterTopicWithCore in
	// the generated page main), so there is nothing to build here. The
	// GenerateTopicManagers / buildTopicManager machinery is retained (and still
	// directly unit-tested) but is no longer part of the build pipeline.

	g, gctx := errgroup.WithContext(context.Background())
	sem := semaphore.NewWeighted(int64(runtime.NumCPU()))
	for _, page := range pages {
		page := page
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return err
			}
			defer sem.Release(1)
			return h.GeneratePage(page, outDir, warnOnce)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	h.cache.save()
	// TinyGo's wasm_exec.js is no longer copied into public/ — it is served from
	// the framework embed via /_gothic/wasm_exec.js (see pkg/helpers/runtimeassets).
	// The standard-Go wasm_exec_go.js, however, is version-tied to the USER's Go
	// toolchain, so it is still copied from their GOROOT below.
	//
	// Pages built with the standard Go compiler need the matching wasm_exec.js
	// from GOROOT (TinyGo's shim is ABI-incompatible). Emit it side-by-side as
	// wasm_exec_go.js so the bootstrap layer can pick the right one.
	if pagesUseStandardGo(pages) {
		if err := h.CopyGoWasmExec("public"); err != nil {
			return err
		}
	}
	return nil
}

// CopyGoWasmExec copies the standard Go wasm_exec.js from GOROOT into destDir
// as wasm_exec_go.js. Tries GOROOT/lib/wasm (Go 1.24+) then GOROOT/misc/wasm
// (older versions).
func (h *WasmHelper) CopyGoWasmExec(destDir string) error {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return fmt.Errorf("wasm: go env GOROOT: %w", err)
	}
	goroot := strings.TrimSpace(string(out))
	candidates := []string{
		filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"),
		filepath.Join(goroot, "misc", "wasm", "wasm_exec.js"),
	}
	var srcPath string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			srcPath = c
			break
		}
	}
	if srcPath == "" {
		return fmt.Errorf("wasm: could not locate wasm_exec.js under %s (looked in lib/wasm and misc/wasm)", goroot)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("wasm: read %s: %w", srcPath, err)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir %s: %w", destDir, err)
	}
	dst := filepath.Join(destDir, "wasm_exec_go.js")
	return os.WriteFile(dst, data, 0644)
}

func (h *WasmHelper) CopyWasmExec(destDir string) error {
	dst := filepath.Join(destDir, "wasm_exec.js")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir wasm_exec dir: %w", err)
	}
	return os.WriteFile(dst, wasmexec.Shim, 0644)
}

// GenerateTopicManagers builds a per-topic MANAGER WASM for each topic struct.
//
// This was RETIRED from the build pipeline: GenerateAll no longer calls it,
// because the full-Go static core is now the generic topic hub (see the note in
// GenerateAll). The method and buildTopicManager are kept so the existing
// unit tests that drive them directly still compile and pass, and so a manager
// binary can be produced on demand if ever needed — but a normal build emits no
// topic-<key>.wasm.
func (h *WasmHelper) GenerateTopicManagers(outDir string, warnOnce *sync.Once) error {
	snippets, structs, aliases, refAliases := h.collectTopicSnippets()
	if !h.hasTopicStructs(structs) {
		return nil
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir %s: %w", outDir, err)
	}
	for _, s := range structs {
		if s.KeyName == "" {
			continue
		}
		if err := h.buildTopicManager(s, snippets, structs, aliases, refAliases, outDir, warnOnce); err != nil {
			return err
		}
	}
	return nil
}

func (h *WasmHelper) buildTopicManager(s structInfo, snippets []string, allStructs []structInfo, aliases map[string]string, refAliases map[string]typeRef, outDir string, warnOnce *sync.Once) error {
	wasmName := "topic-" + s.KeyName
	compression := s.Compression
	var hash string
	if h.cache != nil {
		hash = h.topicManagerInputHash(s)
		outPath := filepath.Join(outDir, wasmName+".wasm"+compressionExt(compression))
		if h.cache.upToDate(wasmName, hash) {
			if _, err := os.Stat(outPath); err == nil {
				wasmUpToDate(wasmName)
				return nil
			}
		}
	}
	// Remove stale files from any previous compression method.
	for _, ext := range []string{".gz", ".br"} {
		if ext != compressionExt(compression) {
			os.Remove(filepath.Join(outDir, wasmName+".wasm"+ext))
		}
	}

	tempModDir, err := os.MkdirTemp("", "tinygo-topic-*")
	if err != nil {
		return fmt.Errorf("wasm: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tempModDir)

	if err := wasmruntime.ExtractRuntime(tempModDir); err != nil {
		return fmt.Errorf("wasm: extract runtime: %w", err)
	}
	if err := h.writeModuleBridge(tempModDir); err != nil {
		return err
	}

	genDir, err := os.MkdirTemp(tempModDir, ".gen-")
	if err != nil {
		return fmt.Errorf("wasm: mkdirtemp gen: %w", err)
	}

	mainPath := filepath.Join(genDir, "main.go")
	codecs, err := h.buildCodecData(allStructs, aliases, refAliases)
	if err != nil {
		return fmt.Errorf("wasm: topic codec: %w", err)
	}
	structNames := make(map[string]bool, len(allStructs))
	for _, st := range allStructs {
		structNames[st.Name] = true
	}
	fields, err := h.buildManagerFieldData(s, structNames, aliases, refAliases)
	if err != nil {
		return fmt.Errorf("wasm: manager fields: %w", err)
	}
	schemaID, schemaLit, _, err := h.schemaFor(s, structNames, aliases, refAliases)
	if err != nil {
		return fmt.Errorf("wasm: manager schema: %w", err)
	}
	if err := h.Template.UpdateFromTemplateFS(WasmTemplateFS, tmplTopicManagerMain, mainPath, WasmTopicManagerMainData{
		StructName:          s.Name,
		KeyName:             s.KeyName,
		HasTime:             h.hasTimeFields(allStructs),
		Codecs:              codecs,
		TopicSnippets:       snippets,
		Fields:              fields,
		SchemaID:            schemaID,
		SchemaDescriptorLit: schemaLit,
	}); err != nil {
		return fmt.Errorf("wasm: render topic manager main.go: %w", err)
	}

	absOutFile, err := filepath.Abs(filepath.Join(outDir, wasmName+".wasm"))
	if err != nil {
		return err
	}

	pkg := "./" + filepath.Base(genDir) + "/"
	cmd, err := h.buildCommandForCompiler(s.Compiler, pkg, absOutFile, tempModDir, warnOnce)
	if err != nil {
		return err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := runWithVendorFallback(cmd, tempModDir, func() (*exec.Cmd, error) {
		return h.buildCommandForCompiler(s.Compiler, pkg, absOutFile, tempModDir, warnOnce)
	}); err != nil {
		return fmt.Errorf("wasm: tinygo build %s (%s): %w", wasmName, compilerLabel(s.Compiler), err)
	}

	wasmSize, _ := h.fileSize(absOutFile)

	wasmOpt := "wasm-opt"
	if _, err := exec.LookPath(wasmOpt); err != nil {
		wasmOpt = ""
		if managed := h.BinaryenBinary(); managed != "" {
			if _, err := os.Stat(managed); err == nil {
				wasmOpt = managed
			}
		}
	}

	if wasmOpt != "" {
		tmp := absOutFile + ".opt"
		if opt := exec.Command(wasmOpt, "-Oz", "--strip-debug", "-o", tmp, absOutFile); opt.Run() == nil {
			os.Rename(tmp, absOutFile)
		} else {
			os.Remove(tmp)
		}
	} else {
		if warnOnce != nil {
			warnOnce.Do(func() {
				wasmWarnf("wasm-opt not found; skipping manual optimization pass. Install Binaryen for smaller binaries.")
			})
		}
	}

	compOutFile := absOutFile + compressionExt(compression) // absOutFile already ends in .wasm
	if err := h.compressWasmWith(absOutFile, compOutFile, compression); err != nil {
		return fmt.Errorf("wasm: compress %s: %w", wasmName, err)
	}
	os.Remove(absOutFile)

	compSize, _ := h.fileSize(compOutFile)
	wasmBuildResult(wasmName, h.formatBytes(wasmSize), h.formatBytes(compSize), compressionLabel(compression))
	if hash != "" {
		h.cache.update(wasmName, hash)
	}
	return nil
}

func (h *WasmHelper) writeWasmMain(src, body string, stdImports []string, helpers []string, topicSnippets []string, topicStructs []structInfo, aliases map[string]string, refAliases map[string]typeRef, jsonReaders []jsonReaderType, jsonRoots []jsonRootRef, jsonWriters []jsonReaderType, jsonEncodeRoots []jsonRootRef, multiplexed bool, dest string) error {
	codecs, err := h.buildCodecData(topicStructs, aliases, refAliases)
	if err != nil {
		return fmt.Errorf("wasm: codec: %w", err)
	}
	wasmFuncs, err := h.buildWasmTopicFuncData(topicStructs, aliases, refAliases)
	if err != nil {
		return fmt.Errorf("wasm: topic func data: %w", err)
	}
	// Reflection-free Decode[T] readers + entry points.
	jsonReaderData, jsonDecoderData := h.buildJSONDecodeData(jsonReaders, jsonRoots)
	// Reflection-free Encode[T] writers + entry points. When any writer
	// is emitted, the shared append/escape helpers (which use strconv) are pulled
	// in — so "strconv" must be imported.
	jsonWriterData, jsonEncoderData := h.buildJSONEncodeData(jsonWriters, jsonEncodeRoots)
	jsonEncodeHelpers := ""
	if len(jsonWriterData) > 0 {
		jsonEncodeHelpers = jsonEncodeHelpersSrc
		hasStrconv := false
		for _, imp := range stdImports {
			if strings.Contains(imp, `"strconv"`) {
				hasStrconv = true
				break
			}
		}
		if !hasStrconv {
			stdImports = append(stdImports, `"strconv"`)
		}
	}
	// Inject "time" import when any topic struct uses time.Time and the page
	// hasn't already imported it from its own source file.
	if h.hasTimeFields(topicStructs) {
		hasTime := false
		for _, imp := range stdImports {
			if strings.Contains(imp, `"time"`) {
				hasTime = true
				break
			}
		}
		if !hasTime {
			stdImports = append(stdImports, `"time"`)
		}
	}
	// Strip a trailing `select {}` (or `select{}`) from the user's body before
	// indenting. The wasm_page_main template now always emits its own haltable
	// keep-alive (`select { case <-GothicHaltChan(): return }`, instance
	// teardown) at the end of main, so users no longer need to write a keep-alive
	// themselves. If a user still has the old bare `select {}` boilerplate, we
	// remove it here so it can't sit before — and dead-code-shadow — the
	// template's haltable copy, which stays the canonical keep-alive.
	trimmed := strings.TrimRight(body, " \t\r\n")
	if strings.HasSuffix(trimmed, "select{}") {
		trimmed = strings.TrimSuffix(trimmed, "select{}")
	} else if strings.HasSuffix(trimmed, "select {}") {
		trimmed = strings.TrimSuffix(trimmed, "select {}")
	}
	body = strings.TrimRight(trimmed, " \t\r\n")

	var indented strings.Builder
	for _, line := range strings.Split(body, "\n") {
		indented.WriteString("\t" + line + "\n")
	}

	return h.Template.UpdateFromTemplateFS(WasmTemplateFS, tmplWasmPageMain, dest, WasmPageMainData{
		SourceFile:    src,
		StdImports:    stdImports,
		Codecs:        codecs,
		KeyVars:       h.buildKeyVarData(topicStructs),
		TopicTypes:    h.buildTopicTypeData(topicStructs),
		WasmFuncs:     wasmFuncs,
		TopicSnippets: topicSnippets,
		Body:          indented.String(),
		Helpers:       helpers,
		Multiplexed:       multiplexed,
		JSONReaders:       jsonReaderData,
		JSONDecoders:      jsonDecoderData,
		JSONWriters:       jsonWriterData,
		JSONEncoders:      jsonEncoderData,
		JSONEncodeHelpers: jsonEncodeHelpers,
	})
}
