package helpers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hh "github.com/gothicframework/core/render"
)

// parseGo confirms rendered template output is syntactically valid Go. It parses
// syntax only (imports are not resolved), which is enough to catch a broken
// template edit — a missing brace, a stray verb, an unbalanced call.
func parseGo(t *testing.T, label, src string) {
	t.Helper()
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, label+".go", src, parser.AllErrors); err != nil {
		t.Fatalf("%s did not parse as valid Go: %v\n---\n%s", label, err, src)
	}
}

// TestTopicTemplatesRenderValidGo renders the consumer (page) and manager
// templates with a REAL topic struct and asserts both parse as valid Go. This
// exercises the schema-seam edits — the version-byte NewDecoder sites, the manager
// _frame wrapper / _rebuildWhole header / SetReqField re-frame, and the schema
// seam registration — which the empty-topic render tests never touch.
func TestTopicTemplatesRenderValidGo(t *testing.T) {
	h := DefaultWasmHelper()
	th := hh.NewTemplateHelper()

	page := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
	}}
	structs := []structInfo{page}
	structNames := map[string]bool{"Page": true}

	codecs, err := h.buildCodecData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildCodecData: %v", err)
	}
	wasmFuncs, err := h.buildWasmTopicFuncData(structs, nil, nil)
	if err != nil {
		t.Fatalf("buildWasmTopicFuncData: %v", err)
	}
	managerFields, err := h.buildManagerFieldData(page, structNames, nil, nil)
	if err != nil {
		t.Fatalf("buildManagerFieldData: %v", err)
	}
	schemaID, schemaLit, _, err := h.schemaFor(page, structNames, nil, nil)
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}

	// ── consumer/page template ──
	pageOut := filepath.Join(t.TempDir(), "page_main.go")
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplWasmPageMain, pageOut, WasmPageMainData{
		SourceFile: "src/pages/index_templ.go",
		Codecs:     codecs,
		TopicTypes: h.buildTopicTypeData(structs),
		KeyVars:    h.buildKeyVarData(structs),
		WasmFuncs:  wasmFuncs,
		Body:       "\t_ = PageTopic()\n",
	}); err != nil {
		t.Fatalf("render page main: %v", err)
	}
	pageSrc, _ := os.ReadFile(pageOut)
	parseGo(t, "wasm_page_main", string(pageSrc))
	// The consumer decodes wire frames via NewDecoder and registers the schema seam.
	if !strings.Contains(string(pageSrc), "NewDecoder(detail)") {
		t.Errorf("page template must decode per-field frames via NewDecoder:\n%s", pageSrc)
	}
	if !strings.Contains(string(pageSrc), `GothicRegisterSchema("page", "`+schemaID+`"`) {
		t.Errorf("page template must register the schema seam:\n%s", pageSrc)
	}

	// ── topic-manager template ──
	mgrOut := filepath.Join(t.TempDir(), "manager_main.go")
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplTopicManagerMain, mgrOut, WasmTopicManagerMainData{
		StructName:          page.Name,
		KeyName:             page.KeyName,
		Codecs:              codecs,
		Fields:              managerFields,
		SchemaID:            schemaID,
		SchemaDescriptorLit: schemaLit,
	}); err != nil {
		t.Fatalf("render manager main: %v", err)
	}
	mgrSrc, _ := os.ReadFile(mgrOut)
	parseGo(t, "wasm_topic_manager_main", string(mgrSrc))
	// The manager frames per-field broadcasts and version-prefixes the whole frame.
	for _, want := range []string{
		"func _frame(raw []byte) string",
		"buf = append(buf, WireVersion)",
		"NewDecoder(detail)",
		"GothicRegisterSchema(\"page\"",
	} {
		if !strings.Contains(string(mgrSrc), want) {
			t.Errorf("manager template missing %q:\n%s", want, mgrSrc)
		}
	}
}
