package helpers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDecodeProject stands up a minimal self-contained module in a temp cwd
// whose page's ClientSideState calls Decode[User](resp). It provides a local
// `wasm` package (path suffix "/wasm", so isDecodeFunc resolves it) supplying the
// symbols the body uses, plus a `routes` package with a RouteConfig so
// ExtractClientSideStateBody fires.
func setupDecodeProject(t *testing.T) {
	t.Helper()
	withTempCwd(t)
	writeProjectFile(t, "go.mod", "module example.com/app\n\ngo 1.21\n")

	writeProjectFile(t, "helpers/routes/routes.go", `package routes

type RouteConfig struct {
	ClientSideState func()
	WasmCompression string
	WasmCompiler    string
}
`)

	// Minimal stand-in for github.com/gothicframework/core/wasm — path ends in
	// "/wasm" so the Decode callee resolves.
	writeProjectFile(t, "wasm/wasm.go", `package wasm

type Response struct {
	Status int
	Body   []byte
}

func (r Response) MapAny() (map[string]any, error) { return nil, nil }

func Fetch(url string) (Response, error) { return Response{}, nil }

func SetText(id, value string) {}

func Decode[T any](r Response) (T, error) {
	var zero T
	return zero, nil
}
`)

	writeProjectFile(t, "src/pages/user_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

type Address struct {
	City string `+"`json:\"city\"`"+`
	Zip  int    `+"`json:\"zip\"`"+`
}

type User struct {
	Name    string   `+"`json:\"user_name\"`"+`
	Age     int      `+"`json:\"age\"`"+`
	Address Address  `+"`json:\"address\"`"+`
	Tags    []string `+"`json:\"tags\"`"+`
	Nick    *string  `+"`json:\"nick\"`"+`
	Secret  string   `+"`json:\"-\"`"+`
}

var Page = routes.RouteConfig{
	ClientSideState: func() {
		resp, _ := Fetch("/api/user")
		u, _ := Decode[User](resp)
		SetText("out", u.Name)
	},
}
`)
}

// TestDecodeScan_DetectsRootsAndReaders drives the real ScanPages pipeline
// (go/packages load + go/types struct reading) and asserts that a page calling
// Decode[User] yields the expected root + reachable reader structs.
func TestDecodeScan_DetectsRootsAndReaders(t *testing.T) {
	setupDecodeProject(t)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(pages))
	}
	page := pages[0]

	if got := page.JSONDecodeRoots; len(got) != 1 || got[0].Ident != "User" || got[0].GoType != "User" {
		t.Fatalf("JSONDecodeRoots: got %+v, want [{User User}]", got)
	}

	readers := map[string]jsonReaderType{}
	for _, s := range page.JSONDecodeTypes {
		readers[s.Ident] = s
	}
	if _, ok := readers["User"]; !ok {
		t.Fatalf("expected a User reader struct, got %v", keysOf(readers))
	}
	if _, ok := readers["Address"]; !ok {
		t.Fatalf("expected a transitively-reachable Address reader struct, got %v", keysOf(readers))
	}

	// The User reader must have picked up the json tag on Name and skipped the
	// unexported-decoder-unsupported nothing here; Secret (json:"-") is kept in
	// the struct info (the tag-skip happens at line generation), so assert the
	// tag was captured.
	var nameTag string
	for _, f := range readers["User"].Fields {
		if f.Name == "Name" {
			nameTag = f.JSONTag
		}
	}
	if nameTag != "user_name" {
		t.Errorf("User.Name JSONTag: got %q, want %q", nameTag, "user_name")
	}
}

// TestDecodeScan_RewriteAndRender exercises the full generation path for a
// Decode[User] page: rewrite Decode[User](resp) → _jsonDecode_User(resp), then
// render the WASM main and assert the generated readers/decoder are present, the
// call was rewritten, the snake_case key is used, the output parses as valid Go,
// and no reflect / encoding/json leaked in.
func TestDecodeScan_RewriteAndRender(t *testing.T) {
	setupDecodeProject(t)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	page := pages[0]

	// Rewrite the Decode[User](resp) call, mirroring GeneratePage.
	rootIdents := map[string]bool{}
	for _, r := range page.JSONDecodeRoots {
		rootIdents[r.Ident] = true
	}
	body, err := h.rewriteDecodeCalls(page.FuncBody, rootIdents)
	if err != nil {
		t.Fatalf("rewriteDecodeCalls: %v", err)
	}
	if !strings.Contains(body, "_jsonDecode_User(resp)") {
		t.Fatalf("rewritten body should call _jsonDecode_User(resp):\n%s", body)
	}
	if strings.Contains(body, "Decode[User]") {
		t.Fatalf("rewritten body should NOT still contain Decode[User]:\n%s", body)
	}

	// Render the full WASM main. page.Helpers carries the tree-shaken User/Address
	// type declarations, so the generated readers reference valid types.
	dest := filepath.Join(t.TempDir(), "main.go")
	if err := h.writeWasmMain(
		page.SourceFile, body, page.Imports, page.Helpers,
		nil, nil, map[string]string{}, nil, // no topics
		page.JSONDecodeTypes, page.JSONDecodeRoots,
		page.JSONEncodeTypes, page.JSONEncodeRoots,
		false, dest,
	); err != nil {
		t.Fatalf("writeWasmMain: %v", err)
	}

	srcBytes, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read generated main: %v", rerr)
	}
	src := string(srcBytes)

	wantContains := []string{
		"func _jsonRead_User(m map[string]any) User {",
		"func _jsonRead_Address(m map[string]any) Address {",
		"func _jsonDecode_User(r Response) (User, error) {",
		"m, err := r.MapAny()",
		`m["user_name"]`,           // snake_case rename in the generated reader
		"_jsonDecode_User(resp)",   // rewritten call inlined into main
		"_jsonRead_Address(",       // nested struct dispatch
	}
	for _, want := range wantContains {
		if !strings.Contains(src, want) {
			t.Errorf("generated main.go missing %q\n---\n%s", want, src)
		}
	}

	for _, banned := range []string{`"reflect"`, `"encoding/json"`} {
		if strings.Contains(src, banned) {
			t.Errorf("generated main.go must not import %s\n---\n%s", banned, src)
		}
	}

	// The generated file must be syntactically valid Go.
	if _, perr := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); perr != nil {
		t.Fatalf("generated main.go does not parse: %v\n---\n%s", perr, src)
	}
}

func keysOf(m map[string]jsonReaderType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// writeDecodeFixtureCommon writes the shared go.mod + routes + wasm fixture
// packages used by the Decode tests below (everything except src/pages).
func writeDecodeFixtureCommon(t *testing.T) {
	t.Helper()
	withTempCwd(t)
	writeProjectFile(t, "go.mod", "module example.com/app\n\ngo 1.21\n")
	writeProjectFile(t, "helpers/routes/routes.go", `package routes

type RouteConfig struct {
	ClientSideState func()
	WasmCompression string
	WasmCompiler    string
}
`)
	writeProjectFile(t, "wasm/wasm.go", `package wasm

type Response struct {
	Status int
	Body   []byte
}

func (r Response) MapAny() (map[string]any, error) { return nil, nil }

type FetchConfig struct {
	Method    string
	BodyBytes []byte
}

func Fetch(url string, cfg ...FetchConfig) (Response, error) { return Response{}, nil }

func SetText(id, value string) {}

func Decode[T any](r Response) (T, error) {
	var zero T
	return zero, nil
}

func Encode[T any](v T) []byte { return nil }
`)
}

// TestDecodeScan_CrossPackageRoot exercises a QUALIFIED cross-package root type
// Decode[api.EchoStruct](resp) end to end: detection yields ident "api_EchoStruct"
// / GoType "api.EchoStruct", the call is rewritten to _jsonDecode_api_EchoStruct,
// the generated reader/decoder use the qualified type, the api import survives
// into main, the output parses, and no reflect leaks in.
func TestDecodeScan_CrossPackageRoot(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "api/api.go", `package api

type EchoStruct struct {
	Message string `+"`json:\"message\"`"+`
	Count   int    `+"`json:\"count\"`"+`
}
`)
	writeProjectFile(t, "src/pages/echo_templ.go", `package pages

import (
	"example.com/app/api"
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		resp, _ := Fetch("/api/echo")
		e, _ := Decode[api.EchoStruct](resp)
		SetText("out", e.Message)
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

	if got := page.JSONDecodeRoots; len(got) != 1 || got[0].Ident != "api_EchoStruct" || got[0].GoType != "api.EchoStruct" {
		t.Fatalf("JSONDecodeRoots: got %+v, want [{api_EchoStruct api.EchoStruct}]", got)
	}

	rootIdents := map[string]bool{}
	for _, r := range page.JSONDecodeRoots {
		rootIdents[r.Ident] = true
	}
	body, err := h.rewriteDecodeCalls(page.FuncBody, rootIdents)
	if err != nil {
		t.Fatalf("rewriteDecodeCalls: %v", err)
	}
	if !strings.Contains(body, "_jsonDecode_api_EchoStruct(resp)") {
		t.Fatalf("body should call _jsonDecode_api_EchoStruct(resp):\n%s", body)
	}
	if strings.Contains(body, "Decode[api.EchoStruct]") {
		t.Fatalf("body should not still contain Decode[api.EchoStruct]:\n%s", body)
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
	srcBytes, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read generated main: %v", rerr)
	}
	src := string(srcBytes)

	wantContains := []string{
		"func _jsonRead_api_EchoStruct(m map[string]any) api.EchoStruct {",
		"var out api.EchoStruct",
		"func _jsonDecode_api_EchoStruct(r Response) (api.EchoStruct, error) {",
		"return api.EchoStruct{}, err",
		`m["message"]`,
		"_jsonDecode_api_EchoStruct(resp)",
		`"example.com/app/api"`, // the qualified package import survives into main
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

// TestDecodeScan_NonStructTypeErrors asserts that Decode[T] with a non-struct T
// fails the scan with a clear, actionable error (not a cryptic TinyGo
// "undefined: Decode").
func TestDecodeScan_NonStructTypeErrors(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/bad_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

var Page = routes.RouteConfig{
	ClientSideState: func() {
		resp, _ := Fetch("/api/n")
		n, _ := Decode[int](resp)
		SetText("out", "")
		_ = n
	},
}
`)

	h := DefaultWasmHelper()
	_, err := h.ScanPages("src/pages", "")
	if err == nil {
		t.Fatalf("expected an error for Decode[int], got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "requires T to be a struct type") || !strings.Contains(msg, "int") {
		t.Fatalf("error should name the non-struct T and be actionable, got: %v", err)
	}
	if !strings.Contains(msg, "bad_templ.go") {
		t.Errorf("error should name the offending page, got: %v", err)
	}
}

// TestDecodeScan_EmbeddedFieldFlatten verifies that an untagged embedded struct
// field's fields are PROMOTED into the parent object (matching encoding/json),
// so the generated reader reads m["id"] (promoted) — never the wrong m["Base"].
func TestDecodeScan_EmbeddedFieldFlatten(t *testing.T) {
	writeDecodeFixtureCommon(t)
	writeProjectFile(t, "src/pages/emb_templ.go", `package pages

import (
	"example.com/app/helpers/routes"
	. "example.com/app/wasm"
)

type Base struct {
	ID int `+"`json:\"id\"`"+`
}

type User struct {
	Base
	Name string `+"`json:\"name\"`"+`
}

var Page = routes.RouteConfig{
	ClientSideState: func() {
		resp, _ := Fetch("/api/user")
		u, _ := Decode[User](resp)
		SetText("out", u.Name)
	},
}
`)

	h := DefaultWasmHelper()
	pages, err := h.ScanPages("src/pages", "")
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	page := pages[0]

	readerData, _ := h.buildJSONDecodeData(page.JSONDecodeTypes, page.JSONDecodeRoots)
	var userLines string
	for _, rd := range readerData {
		if rd.Ident == "User" {
			var b strings.Builder
			for _, f := range rd.Fields {
				b.WriteString(f.DecLine)
				b.WriteString("\n")
			}
			userLines = b.String()
		}
	}
	if !strings.Contains(userLines, `m["id"]`) {
		t.Errorf("embedded field should be promoted and read m[\"id\"]:\n%s", userLines)
	}
	if !strings.Contains(userLines, `out.ID = int(`) {
		t.Errorf("promoted field ID should assign out.ID:\n%s", userLines)
	}
	if !strings.Contains(userLines, `m["name"]`) {
		t.Errorf("expected the parent's own Name field read:\n%s", userLines)
	}
	// The embed must NOT be read as a single wrong-keyed object field.
	if strings.Contains(userLines, `m["Base"]`) || strings.Contains(userLines, "_jsonRead_Base") {
		t.Errorf("embedded struct must be flattened, not read as m[\"Base\"] / _jsonRead_Base:\n%s", userLines)
	}
}
