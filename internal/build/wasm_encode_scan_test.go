package helpers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEncodeScan_RewriteAndRender drives the full Encode[T] path for a page-local
// root: ScanPages detects Encode[User], the call is rewritten to
// _jsonEncode_User(u), and the rendered main contains the writer/encoder + the
// shared helpers, imports strconv, parses, and has no reflect / encoding/json.
func TestEncodeScan_RewriteAndRender(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/save_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

type Address struct {
	City string `+"`json:\"city\"`"+`
}

type User struct {
	Name    string  `+"`json:\"user_name\"`"+`
	Age     int     `+"`json:\"age\"`"+`
	Address Address `+"`json:\"address\"`"+`
	Nick    *string `+"`json:\"nick\"`"+`
}

var Page = routes.RouteConfig{
	ClientSideState: func() {
		u := User{Name: "x"}
		_, _ = Fetch("/api/user", FetchConfig{Method: "POST", BodyBytes: Encode[User](u)})
	},
}
`)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(pages))
	}
	page := pages[0]

	if got := page.JSONEncodeRoots; len(got) != 1 || got[0].Ident != "User" || got[0].GoType != "User" {
		t.Fatalf("JSONEncodeRoots: got %+v, want [{User User}]", got)
	}
	// Decode side must be untouched for an Encode-only page.
	if len(page.JSONDecodeRoots) != 0 {
		t.Fatalf("JSONDecodeRoots should be empty for an Encode-only page, got %+v", page.JSONDecodeRoots)
	}

	rootIdents := map[string]bool{}
	for _, r := range page.JSONEncodeRoots {
		rootIdents[r.Ident] = true
	}
	body, err := h.rewriteEncodeCalls(page.FuncBody, rootIdents)
	if err != nil {
		t.Fatalf("rewriteEncodeCalls: %v", err)
	}
	if !strings.Contains(body, "_jsonEncode_User(u)") {
		t.Fatalf("body should call _jsonEncode_User(u):\n%s", body)
	}
	if strings.Contains(body, "Encode[User]") {
		t.Fatalf("body should not still contain Encode[User]:\n%s", body)
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

	wantContains := []string{
		"func _jsonWrite_User(b *[]byte, v User) {",
		"func _jsonWrite_Address(b *[]byte, v Address) {",
		"func _jsonEncode_User(v User) []byte {",
		"func _jsonAppendString(b *[]byte, s string) {", // shared helpers emitted
		`"strconv"`,                       // helpers import strconv
		`"\"user_name\":"`,                // snake_case key baked in
		"_jsonWrite_Address(b, v.Address)", // nested struct dispatch
		"_jsonEncode_User(u)",             // rewritten call
	}
	for _, want := range wantContains {
		if !strings.Contains(src, want) {
			t.Errorf("generated main.go missing %q\n---\n%s", want, src)
		}
	}
	if strings.Contains(src, `"reflect"`) || strings.Contains(src, `"encoding/json"`) {
		t.Errorf("generated main.go must not import reflect/encoding/json\n---\n%s", src)
	}
	if _, perr := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); perr != nil {
		t.Fatalf("generated main.go does not parse: %v\n---\n%s", perr, src)
	}
}

// TestEncodeScan_CrossPackageRoot verifies the qualified Encode[api.EchoStruct]
// path: rewrite → _jsonEncode_api_EchoStruct, writer uses the qualified type, the
// api import survives, output parses, no reflect.
func TestEncodeScan_CrossPackageRoot(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "api/api.go", `package api

type EchoStruct struct {
	Message string `+"`json:\"message\"`"+`
	Count   int    `+"`json:\"count\"`"+`
}
`)
	writeProjectFile(t, "src/pages/echoenc_templ.go", `package pages

import (
	"example.com/app/api"
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		e := api.EchoStruct{Message: "hi"}
		_, _ = Fetch("/api/echo", FetchConfig{Method: "POST", BodyBytes: Encode[api.EchoStruct](e)})
	},
}
`)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	page := pages[0]

	if got := page.JSONEncodeRoots; len(got) != 1 || got[0].Ident != "api_EchoStruct" || got[0].GoType != "api.EchoStruct" {
		t.Fatalf("JSONEncodeRoots: got %+v, want [{api_EchoStruct api.EchoStruct}]", got)
	}

	rootIdents := map[string]bool{"api_EchoStruct": true}
	body, err := h.rewriteEncodeCalls(page.FuncBody, rootIdents)
	if err != nil {
		t.Fatalf("rewriteEncodeCalls: %v", err)
	}
	if !strings.Contains(body, "_jsonEncode_api_EchoStruct(e)") {
		t.Fatalf("body should call _jsonEncode_api_EchoStruct(e):\n%s", body)
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
		"func _jsonWrite_api_EchoStruct(b *[]byte, v api.EchoStruct) {",
		"func _jsonEncode_api_EchoStruct(v api.EchoStruct) []byte {",
		`"example.com/app/api"`,
		"_jsonEncode_api_EchoStruct(e)",
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

// TestEncodeScan_NonStructTypeErrors asserts Encode[T] with a non-struct T fails
// the scan with a clear, actionable error.
func TestEncodeScan_NonStructTypeErrors(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/badenc_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		_, _ = Fetch("/api/n", FetchConfig{BodyBytes: Encode[int](5)})
	},
}
`)

	h := DefaultWasmHelper()
	_, err := h.ScanPages("src/pages", "")
	if err == nil {
		t.Fatalf("expected an error for Encode[int], got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Encode[T] requires T to be a struct type") || !strings.Contains(msg, "int") {
		t.Fatalf("error should name the non-struct T and be actionable, got: %v", err)
	}
	if !strings.Contains(msg, "badenc_templ.go") {
		t.Errorf("error should name the offending page, got: %v", err)
	}
}

// TestEncodeScan_InferredTypeErrors asserts an inferred Encode(v) (no explicit
// type argument) fails with an actionable message rather than a cryptic
// "undefined: Encode" at TinyGo compile.
func TestEncodeScan_InferredTypeErrors(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/infenc_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

type User struct {
	Name string `+"`json:\"name\"`"+`
}

var Page = routes.RouteConfig{
	ClientSideState: func() {
		u := User{Name: "x"}
		_, _ = Fetch("/api/user", FetchConfig{BodyBytes: Encode(u)})
	},
}
`)

	h := DefaultWasmHelper()
	_, err := h.ScanPages("src/pages", "")
	if err == nil {
		t.Fatalf("expected an error for inferred Encode(u), got nil")
	}
	if !strings.Contains(err.Error(), "requires an explicit type argument") {
		t.Fatalf("error should ask for an explicit type argument, got: %v", err)
	}
}

// TestEncodeScan_TreeShaking confirms a page with no Encode[T] call produces no
// encoder, no writer, and does NOT pull the encode helpers / strconv into main.
func TestEncodeScan_TreeShaking(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/plain_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		SetText("out", "hello")
	},
}
`)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	page := pages[0]
	if len(page.JSONEncodeRoots) != 0 || len(page.JSONEncodeTypes) != 0 {
		t.Fatalf("Encode-free page must have no encode roots/types, got %+v / %+v", page.JSONEncodeRoots, page.JSONEncodeTypes)
	}

	dest := filepath.Join(t.TempDir(), "main.go")
	if err := h.writeWasmMain(
		page.SourceFile, page.FuncBody, page.Imports, page.Helpers,
		nil, nil, map[string]string{}, nil,
		page.JSONDecodeTypes, page.JSONDecodeRoots,
		page.JSONEncodeTypes, page.JSONEncodeRoots,
		false, dest,
	); err != nil {
		t.Fatalf("writeWasmMain: %v", err)
	}
	src := readGeneratedMain(t, dest)
	for _, banned := range []string{"_jsonEncode_", "_jsonWrite_", "_jsonAppendString", `"strconv"`} {
		if strings.Contains(src, banned) {
			t.Errorf("Encode-free page should not emit %q (tree-shaking)\n---\n%s", banned, src)
		}
	}
}

func readGeneratedMain(t *testing.T, dest string) string {
	t.Helper()
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read generated main: %v", err)
	}
	return string(b)
}
