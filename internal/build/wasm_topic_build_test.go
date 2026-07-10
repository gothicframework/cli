package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	hh "github.com/gothicframework/core/render"
	wasmruntime "github.com/gothicframework/core/wasm"
)

// TestPhase17ConsumerBuildsUnderJSWasm is the Phase-17 Go-level build gate: it
// renders a REAL topic consumer page main (per-field-fanout Set + per-field-replay
// online + RegisterTopicWithCore control-plane handshake) into a wasm-runtime
// module and compiles it with the standard Go js/wasm toolchain. The runtime
// files are `//go:build js && wasm`, which GOOS=js GOARCH=wasm satisfies exactly
// like TinyGo's -target=wasm — so a green build here proves the generated
// consumer + the runtime helpers it now calls (RegisterTopicWithCore,
// ListenTopicCoreOnline, _broadcastAll) link and type-check for the WASM target.
//
// The end-to-end BEHAVIOR (writer → core → consumer, per-field, online/ping
// hydration, large-payload stress) is proven by the Phase-21
// wasm-topic-consolidation.spec.ts Playwright suite on TestGothic — NOT here.
//
// Skipped when the Go toolchain is unavailable (matches the rest of the suite,
// which runs in toolchain-less CI).
func TestPhase17ConsumerBuildsUnderJSWasm(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping js/wasm build gate")
	}

	tmp := t.TempDir()
	if err := wasmruntime.ExtractRuntime(tmp); err != nil {
		t.Fatalf("extract runtime: %v", err)
	}

	h := DefaultWasmHelper()
	th := hh.NewTemplateHelper()

	page := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
	}}
	structs := []structInfo{page}

	codecs, err := h.buildCodecData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildCodecData: %v", err)
	}
	wasmFuncs, err := h.buildWasmTopicFuncData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildWasmTopicFuncData: %v", err)
	}

	genDir := filepath.Join(tmp, "gen")
	if err := os.MkdirAll(genDir, 0755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	mainPath := filepath.Join(genDir, "main.go")
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplWasmPageMain, mainPath, WasmPageMainData{
		SourceFile: "src/pages/index_templ.go",
		Codecs:     codecs,
		TopicTypes: h.buildTopicTypeData(structs),
		KeyVars:    h.buildKeyVarData(structs),
		WasmFuncs:  wasmFuncs,
		// The topic struct is normally inlined from src/topics/*.go via TopicSnippets.
		TopicSnippets: []string{"type Page struct {\n\tCount int\n\tLabel string\n}"},
		Body:          "\t_ = PageTopic()\n",
	}); err != nil {
		t.Fatalf("render page main: %v", err)
	}

	out := filepath.Join(tmp, "page.wasm")
	cmd := exec.Command("go", "build", "-o", out, "./gen/")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "GOWORK=off", "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GOOS=js GOARCH=wasm go build of generated consumer failed: %v\n%s", err, b)
	}
}

// TestTopicAccessorNoMountBuildsUnderJSWasm is the Phase-25 guarantee test: it
// pins the fact that a topic's mount (@AddXxxTopic() / TopicManagerComponent) is
// NOT required for a topic to register with the core. It renders a page whose
// ClientSideState uses ONLY the topic accessor (`PageTopic()`) — there is no
// AddXxxTopic() / mount reference anywhere in the generated program — then asserts
// the generated main still contains the control-plane registration
// (RegisterTopicWithCore) plus the per-field wiring and codec, and finally builds
// the whole thing under GOOS=js GOARCH=wasm.
//
// This is the codegen-level proof behind Phase 25's "the mount is optional /
// deprecated" claim: declaring a topic and using its accessor is sufficient to
// auto-register with the core. If a future refactor ever made registration depend
// on the mount, this test fails.
//
// Skipped when the Go toolchain is unavailable (matches the rest of the suite,
// which runs in toolchain-less CI).
func TestTopicAccessorNoMountBuildsUnderJSWasm(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping js/wasm build gate")
	}

	tmp := t.TempDir()
	if err := wasmruntime.ExtractRuntime(tmp); err != nil {
		t.Fatalf("extract runtime: %v", err)
	}

	h := DefaultWasmHelper()
	th := hh.NewTemplateHelper()

	page := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
	}}
	structs := []structInfo{page}

	codecs, err := h.buildCodecData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildCodecData: %v", err)
	}
	wasmFuncs, err := h.buildWasmTopicFuncData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildWasmTopicFuncData: %v", err)
	}

	genDir := filepath.Join(tmp, "gen")
	if err := os.MkdirAll(genDir, 0755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	mainPath := filepath.Join(genDir, "main.go")
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplWasmPageMain, mainPath, WasmPageMainData{
		SourceFile: "src/pages/index_templ.go",
		Codecs:     codecs,
		TopicTypes: h.buildTopicTypeData(structs),
		KeyVars:    h.buildKeyVarData(structs),
		WasmFuncs:  wasmFuncs,
		TopicSnippets: []string{"type Page struct {\n\tCount int\n\tLabel string\n}"},
		// Accessor-only usage: NO AddPageTopic() / mount anywhere. The single
		// accessor call is what must be enough to auto-register with the core.
		Body: "\t_ = PageTopic()\n",
	}); err != nil {
		t.Fatalf("render page main: %v", err)
	}

	genSrc, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read generated main: %v", err)
	}
	main := string(genSrc)

	// Guard the premise: this program must NOT depend on the mount in any way.
	for _, mount := range []string{"AddPageTopic", "TopicManagerComponent"} {
		if strings.Contains(main, mount) {
			t.Fatalf("generated accessor-only main unexpectedly references the mount %q — the no-mount premise is broken", mount)
		}
	}

	// The control-plane registration handshake must be emitted from the accessor
	// path alone — this is what makes the core subscribe + replay for the topic.
	if !strings.Contains(main, "RegisterTopicWithCore(") {
		t.Fatalf("generated accessor-only main is missing RegisterTopicWithCore( — the accessor did not pull in core registration:\n%s", main)
	}
	// Per-field wiring + codec must be present too (the topic is fully live from
	// the accessor, independent of any mount).
	for _, want := range []string{
		`RegisterTopicWithCore("page"`,
		`RequestTopicSetFieldBytes("page", "Count"`,
		`ListenTopicEventField("page", "Count"`,
		"topic.Count.SetBroadcast(",
		"func _encode_Page(",
		"func _decode_Page(",
	} {
		if !strings.Contains(main, want) {
			t.Fatalf("generated accessor-only main missing expected wiring %q:\n%s", want, main)
		}
	}

	out := filepath.Join(tmp, "page.wasm")
	cmd := exec.Command("go", "build", "-o", out, "./gen/")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "GOWORK=off", "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GOOS=js GOARCH=wasm go build of accessor-only consumer failed: %v\n%s", err, b)
	}
}
