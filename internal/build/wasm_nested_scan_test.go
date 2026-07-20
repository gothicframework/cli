package helpers

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeSharedDTOFixture stands up the shared-DTO pattern: a `shared` package with
// a root struct EchoStruct that has a NESTED struct field of type shared.EchoNested
// (same package as the root). Mirrors the e2e own-BFF-JSON case.
func writeSharedDTOFixture(t *testing.T) {
	t.Helper()
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "shared/shared.go", `package shared

type EchoNested struct {
	Depth int `+"`json:\"depth\"`"+`
	Tag   string `+"`json:\"tag\"`"+`
}

type EchoStruct struct {
	Message string     `+"`json:\"message\"`"+`
	Count   int        `+"`json:\"count\"`"+`
	Nested  EchoNested `+"`json:\"nested\"`"+`
	Kids    []EchoNested `+"`json:\"kids\"`"+`
}
`)
}

// TestNestedScan_CrossPackageNestedStruct is the regression test for the
// cross-package nested-struct bug: a cross-package root shared.EchoStruct with a nested struct field of type
// shared.EchoNested (same package) must recurse — the generated reader/writer for
// the nested type must be emitted (qualified) and called, not soft-failed to zero.
func TestNestedScan_CrossPackageNestedStruct(t *testing.T) {
	writeSharedDTOFixture(t)
	writeProjectFile(t, "src/pages/echo_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	"example.com/app/shared"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		resp, _ := Fetch("/api/echo")
		e, _ := Decode[shared.EchoStruct](resp)
		SetText("out", e.Nested.Tag)
		s := shared.EchoStruct{Nested: shared.EchoNested{Depth: 7}}
		_, _ = Fetch("/api/echo", FetchConfig{Method: "POST", BodyBytes: Encode[shared.EchoStruct](s)})
	},
}
`)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	page := pages[0]

	// Both directions detect the qualified root, and the nested type is reachable.
	readerIdents := map[string]bool{}
	for _, s := range page.JSONDecodeTypes {
		readerIdents[s.Ident] = true
	}
	if !readerIdents["shared_EchoStruct"] || !readerIdents["shared_EchoNested"] {
		t.Fatalf("expected reader structs for shared_EchoStruct AND shared_EchoNested, got %v", readerIdents)
	}
	writerIdents := map[string]bool{}
	for _, s := range page.JSONEncodeTypes {
		writerIdents[s.Ident] = true
	}
	if !writerIdents["shared_EchoStruct"] || !writerIdents["shared_EchoNested"] {
		t.Fatalf("expected writer structs for shared_EchoStruct AND shared_EchoNested, got %v", writerIdents)
	}

	// Rewrite both calls, mirroring GeneratePage.
	body := page.FuncBody
	body, err = h.rewriteDecodeCalls(body, map[string]bool{"shared_EchoStruct": true})
	if err != nil {
		t.Fatalf("rewriteDecodeCalls: %v", err)
	}
	body, err = h.rewriteEncodeCalls(body, map[string]bool{"shared_EchoStruct": true})
	if err != nil {
		t.Fatalf("rewriteEncodeCalls: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "main.go")
	if err := h.writeWasmMain(
		page.SourceFile, body, page.Imports, page.Helpers,
		nil, nil, map[string]string{}, nil,
		page.JSONDecodeTypes, page.JSONDecodeRoots,
		page.JSONEncodeTypes, page.JSONEncodeRoots,
		false, dest,
	); err != nil {
		t.Fatalf("writeWasmMain: %v", err)
	}
	src := readGeneratedMain(t, dest)

	for _, want := range []string{
		"func _jsonRead_shared_EchoStruct(m map[string]any) shared.EchoStruct {",
		"func _jsonRead_shared_EchoNested(m map[string]any) shared.EchoNested {",
		`out.Nested = _jsonRead_shared_EchoNested(_m0)`,                    // nested recursion (read)
		`m["nested"].(map[string]any)`,                                    // reads the nested object
		"func _jsonWrite_shared_EchoStruct(b *[]byte, v shared.EchoStruct) {",
		"func _jsonWrite_shared_EchoNested(b *[]byte, v shared.EchoNested) {",
		"_jsonWrite_shared_EchoNested(b, v.Nested)",                       // nested recursion (write)
		`"example.com/app/shared"`,                                        // shared import survives
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated main.go missing %q\n---\n%s", want, src)
		}
	}
	if strings.Contains(src, `"reflect"`) || strings.Contains(src, `"encoding/json"`) {
		t.Errorf("generated main.go must not import reflect/encoding/json")
	}
	if _, perr := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); perr != nil {
		t.Fatalf("generated main.go does not parse: %v\n---\n%s", perr, src)
	}
}

// TestNestedEncode_RoundTripWithNested is a genuine round-trip proving the nested
// field survives: it synthesizes a host program from the generated writers (root +
// nested) + helpers, go-runs it on a value with a populated nested struct and a
// slice of nested structs, then unmarshals and asserts the nested depth is 7 (the
// exact e2e case), not the zero value.
func TestNestedEncode_RoundTripWithNested(t *testing.T) {
	writers := []jsonReaderType{
		{Ident: "EchoStruct", GoType: "EchoStruct", Fields: []fieldInfo{
			jsonField("Message", "string", "message"),
			{Name: "Nested", Type: "EchoNested", TypeRef: Named{Name: "EchoNested"}, JSONTag: "nested"},
			{Name: "Kids", Type: "[]EchoNested", TypeRef: SliceOf{Elem: Named{Name: "EchoNested"}}, JSONTag: "kids"},
		}},
		{Ident: "EchoNested", GoType: "EchoNested", Fields: []fieldInfo{
			jsonField("Depth", "int", "depth"),
			jsonField("Tag", "string", "tag"),
		}},
	}
	roots := []jsonRootRef{{Ident: "EchoStruct", GoType: "EchoStruct"}}
	h := DefaultWasmHelper()
	writerData, encoderData := h.buildJSONEncodeData(writers, roots)

	var prog strings.Builder
	prog.WriteString("package main\n\nimport (\n\t\"os\"\n\t\"strconv\"\n)\n\n")
	prog.WriteString("type EchoNested struct { Depth int; Tag string }\n")
	prog.WriteString("type EchoStruct struct { Message string; Nested EchoNested; Kids []EchoNested }\n\n")
	prog.WriteString(jsonEncodeHelpersSrc + "\n")
	for _, w := range writerData {
		prog.WriteString("func _jsonWrite_" + w.Ident + "(b *[]byte, v " + w.GoType + ") {\n\t*b = append(*b, '{')\n")
		for _, f := range w.Fields {
			prog.WriteString("\t*b = append(*b, " + f.KeyPrefixLit + "...)\n\t" + f.ValueLine + "\n")
		}
		prog.WriteString("\t*b = append(*b, '}')\n}\n\n")
	}
	for _, e := range encoderData {
		prog.WriteString("func _jsonEncode_" + e.Ident + "(v " + e.GoType + ") []byte { var b []byte; _jsonWrite_" + e.Ident + "(&b, v); return b }\n")
	}
	prog.WriteString(`
func main() {
	s := EchoStruct{Message: "hi", Nested: EchoNested{Depth: 7, Tag: "root"},
		Kids: []EchoNested{{Depth: 1, Tag: "a"}, {Depth: 2, Tag: "b"}}}
	os.Stdout.Write(_jsonEncode_EchoStruct(s))
}
`)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog.String()), 0644); err != nil {
		t.Fatalf("write program: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module rt\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run failed: %v\n%s\n---\n%s", err, out, prog.String())
	}

	var got struct {
		Message string `json:"message"`
		Nested  struct {
			Depth int    `json:"depth"`
			Tag   string `json:"tag"`
		} `json:"nested"`
		Kids []struct {
			Depth int    `json:"depth"`
			Tag   string `json:"tag"`
		} `json:"kids"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("generated JSON does not unmarshal: %v\nJSON: %s", err, out)
	}
	if got.Nested.Depth != 7 || got.Nested.Tag != "root" {
		t.Errorf("nested field round-trip failed (the cross-package nested-struct bug): got %+v, want {Depth:7 Tag:root}", got.Nested)
	}
	if len(got.Kids) != 2 || got.Kids[0].Depth != 1 || got.Kids[1].Tag != "b" {
		t.Errorf("slice-of-nested round-trip failed: %+v", got.Kids)
	}
}
