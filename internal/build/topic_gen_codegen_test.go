package helpers

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	hh "github.com/gothicframework/core/render"
)

// moduleRoot walks up from this test file to the module root (where go.mod is).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find module root")
	return ""
}

// renderTopicGenFixture renders topic_gen.go.tmpl with a real topic and returns
// the generated source.
func renderTopicGenFixture(t *testing.T) string {
	t.Helper()
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
	out := filepath.Join(t.TempDir(), "topic_gen.go")
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplTopicGen, out, TopicGenData{
		PkgName:     "codegencompile",
		HasTopics:   h.hasTopicStructs(structs),
		HasTime:     h.hasTimeFields(structs),
		Codecs:      codecs,
		KeyVars:     h.buildKeyVarData(structs),
		TopicTypes:  h.buildTopicTypeData(structs),
		ServerFuncs: h.buildServerTopicFuncData(structs, nil, nil),
	}); err != nil {
		t.Fatalf("render topic_gen: %v", err)
	}
	src, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered topic_gen: %v", err)
	}
	return string(src)
}

// TestTopicGenTemplateImportsOrgPath is a regression guard for the module
// rename: topic_gen.go is generated into the USER's project and imports the
// gothicframework runtime package. A stale pre-rename legacy or major-version path
// there breaks `gothic build`. This asserts, via the parsed AST, that every
// gothicframework import resolves to the suffixless org path (never pre-rename legacy,
// never a /vN segment). (v3 removed the topic mount, so the generated file no
// longer imports the routes package.)
func TestTopicGenTemplateImportsOrgPath(t *testing.T) {
	src := renderTopicGenFixture(t)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "topic_gen.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("topic_gen did not parse: %v\n---\n%s", err, src)
	}
	var seen int
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.Contains(path, "gothicframework/") {
			continue
		}
		seen++
		// The generated topic_gen import must resolve to the suffixless core module
		// (github.com/gothicframework/core/...). This one prefix check rejects every
		// stale form at once: the legacy org, a /v2 or /v3 major-version segment, or
		// the components module — none of which start with this prefix.
		if !strings.HasPrefix(path, "github.com/gothicframework/core/") {
			t.Errorf("topic_gen import %q is not under the suffixless core module (github.com/gothicframework/core/...) — stale org or major-version segment?", path)
		}
	}
	if seen < 1 {
		t.Errorf("expected topic_gen to import the wasm gothicframework package, saw %d", seen)
	}
}

// TestTopicGenTemplateCompilesAgainstV3 is the stronger guard the reviewer asked
// for: it doesn't just string-match — it actually COMPILES the generated
// topic_gen.go against the real /v3 packages. If any generated import path or
// referenced symbol is wrong for v3, `go build` fails here instead of surfacing
// in the user's `gothic init`→`gothic build`.
//
// The generated file is compiled as a throwaway package inside THIS module (so
// /v3 imports resolve from the module's own go.mod and build cache), then
// removed. Skipped in -short mode since it shells out to the compiler.
func TestTopicGenTemplateCompilesAgainstV3(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build compile check in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	src := renderTopicGenFixture(t)
	root := moduleRoot(t)

	pkgDir, err := os.MkdirTemp(root, "codegen_compile_")
	if err != nil {
		t.Fatalf("mkdir temp pkg: %v", err)
	}
	defer os.RemoveAll(pkgDir)

	if err := os.WriteFile(filepath.Join(pkgDir, "gen.go"), []byte(src), 0644); err != nil {
		t.Fatalf("write gen.go: %v", err)
	}
	// In a real project topic_gen.go is generated INTO the user's src/topics
	// package, where the topic struct is defined. Supply that definition so the
	// compile validates the generated code + its /v3 imports, not a missing type.
	typesSrc := "package codegencompile\n\ntype Page struct {\n\tCount int\n\tLabel string\n}\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "types.go"), []byte(typesSrc), 0644); err != nil {
		t.Fatalf("write types.go: %v", err)
	}

	cmd := exec.Command("go", "build", "./"+filepath.Base(pkgDir))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated topic_gen.go failed to compile against /v3:\n%s\n--- generated source ---\n%s", out, src)
	}
}
