package helpers

// go/types-based reader for the Decode[T] JSON codegen (Phase 6).
//
// Detection (collectJSONDecodeRoots) walks a ClientSideState body for
// `Decode[T](resp)` calls whose callee resolves — via TypesInfo — to the
// core/wasm Decode generic. The concrete T is read from TypesInfo.Instances
// (robust under inference) with a syntactic fallback. A detected Decode call
// whose T is NOT a struct is a hard build error (better than a cryptic TinyGo
// "undefined: Decode").
//
// Root types may be page-local (`Decode[User]`, T inlined by tree-shaking or
// dot-imported → bare name) OR qualified cross-package (`Decode[api.Echo]`, T in
// an imported package → qualified name + that import carried into main). Each
// root carries an Ident (collision-safe function-name fragment, e.g. `api_Echo`)
// and a GoType (the Go type as written, e.g. `api.Echo`).
//
// Struct reading (buildJSONReaderStructs) walks T and every PAGE-LOCAL nested
// named struct reachable from it via go/types, producing reader structs the
// codec layer turns into reflection-free readers. A nested struct field whose
// type is NOT in the page's own package is soft-failed (decoded as the field's
// zero value) — only the ROOT may be cross-package. Embedded (anonymous) struct
// fields are FLATTENED to match encoding/json's field promotion.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"reflect"
	"strings"

	"golang.org/x/tools/go/packages"
)

// coreWasmImportPath is the canonical import path of the dot-imported runtime
// stub package that declares Decode[T]. Pages dot-import it as
// `. "github.com/gothicframework/core/wasm"`.
const coreWasmImportPath = "github.com/gothicframework/core/wasm"

// jsonDecodeRoot is one resolved top-level Decode[T] type argument.
type jsonDecodeRoot struct {
	named  *types.Named
	ident  string // _jsonDecode_<ident> / _jsonRead_<ident>
	goType string // Go type as written at the call site (bare or qualified)
}

// jsonRootRef is the (Ident, GoType) pair a page carries for each Decode[T]
// root — enough to emit its decoder and to match its rewrite.
type jsonRootRef struct {
	Ident  string
	GoType string
}

// jsonReaderType is one struct type reachable from a Decode[T] call, with the
// data needed to emit its reflection-free reader.
type jsonReaderType struct {
	Ident  string // _jsonRead_<Ident>
	GoType string // reader return type / `var out <GoType>`
	Fields []fieldInfo
}

// isCoreWasmFunc reports whether obj is the core/wasm generic function named
// funcName (e.g. "Decode" or "Encode"). It matches the canonical import path, and
// also any package whose path ends in "/wasm" so the test-harness fixtures (which
// stand up a local `wasm` package) resolve — mirroring how
// ExtractClientSideStateBody accepts both the real router path and the fixture
// `routes` path.
func isCoreWasmFunc(obj types.Object, funcName string) bool {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Name() != funcName {
		return false
	}
	p := fn.Pkg()
	if p == nil {
		return false
	}
	return p.Path() == coreWasmImportPath || strings.HasSuffix(p.Path(), "/wasm")
}

// decodeFuncNameIdent unwraps a Decode[T](...) call's callee to (a) the
// identifier that names the function (the trailing ident of `Decode` or
// `pkg.Decode`) and (b) the first type-argument expression. Returns (nil, nil)
// for any non-generic-call shape.
func decodeFuncNameIdent(fun ast.Expr) (nameIdent *ast.Ident, typeArg ast.Expr) {
	var x, idx ast.Expr
	switch fn := fun.(type) {
	case *ast.IndexExpr:
		x, idx = fn.X, fn.Index
	case *ast.IndexListExpr:
		if len(fn.Indices) == 0 {
			return nil, nil
		}
		x, idx = fn.X, fn.Indices[0]
	default:
		return nil, nil
	}
	switch xe := x.(type) {
	case *ast.Ident:
		return xe, idx
	case *ast.SelectorExpr:
		return xe.Sel, idx
	}
	return nil, nil
}

// renderExpr formats an AST expression back to Go source text (e.g. the type
// argument `api.Echo` → "api.Echo").
func renderExpr(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := format.Node(&b, fset, e); err != nil {
		return ""
	}
	return b.String()
}

// sanitizeTypeIdent turns a Go type string into a collision-safe identifier
// fragment: every character that is not [A-Za-z0-9_] becomes '_' (so
// "api.Echo" → "api_Echo", "User" → "User").
func sanitizeTypeIdent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// collectJSONDecodeRoots returns the Decode[T] roots of a ClientSideState body.
func collectJSONDecodeRoots(pkg *packages.Package, body ast.Node, pagePath string) ([]jsonDecodeRoot, error) {
	return collectJSONFuncRoots(pkg, body, pagePath, "Decode")
}

// collectJSONEncodeRoots returns the Encode[T] roots of a ClientSideState body.
func collectJSONEncodeRoots(pkg *packages.Package, body ast.Node, pagePath string) ([]jsonDecodeRoot, error) {
	return collectJSONFuncRoots(pkg, body, pagePath, "Encode")
}

// collectJSONFuncRoots walks a ClientSideState body and returns, in source order
// and deduplicated by identifier, every top-level core/wasm funcName[T] root
// (funcName is "Decode" or "Encode"). Detection is type-aware (the callee must
// resolve to core/wasm.<funcName>), so an unrelated same-named function is
// ignored.
//
// Two shapes are diagnosed with a clear, actionable error rather than a cryptic
// TinyGo "undefined: <funcName>":
//   - a qualifying call whose explicit T is NOT a struct type, and
//   - a call written WITHOUT an explicit type argument (e.g. inferred
//     `Encode(v)`) — the rewrite is syntactic and cannot recover T, so an
//     explicit type argument is required.
//
// A page with no qualifying call yields (nil, nil).
func collectJSONFuncRoots(pkg *packages.Package, body ast.Node, pagePath, funcName string) ([]jsonDecodeRoot, error) {
	if pkg == nil || pkg.TypesInfo == nil || body == nil {
		return nil, nil
	}
	var roots []jsonDecodeRoot
	seen := map[string]bool{}
	var collErr error
	ast.Inspect(body, func(n ast.Node) bool {
		if collErr != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Explicit generic form: funcName[T](...).
		if nameIdent, typeArg := decodeFuncNameIdent(call.Fun); nameIdent != nil && nameIdent.Name == funcName {
			if !isCoreWasmFunc(pkg.TypesInfo.Uses[nameIdent], funcName) {
				return true
			}
			typeText := renderExpr(pkg.Fset, typeArg)
			named := resolveDecodeTypeArg(pkg, nameIdent, typeArg)
			if named == nil {
				collErr = fmt.Errorf("gothic: %s[T] requires T to be a struct type, got %s in %s", funcName, typeText, pagePath)
				return false
			}
			if _, ok := named.Underlying().(*types.Struct); !ok {
				collErr = fmt.Errorf("gothic: %s[T] requires T to be a struct type, got %s in %s", funcName, typeText, pagePath)
				return false
			}
			ident := sanitizeTypeIdent(typeText)
			if !seen[ident] {
				seen[ident] = true
				roots = append(roots, jsonDecodeRoot{named: named, ident: ident, goType: typeText})
			}
			return true
		}
		// Bare form without an explicit type argument: funcName(...). This is only
		// reachable when T is inferred (e.g. Encode(v)); the syntactic rewrite
		// cannot recover T, so require the explicit spelling.
		var bareIdent *ast.Ident
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			bareIdent = fn
		case *ast.SelectorExpr:
			bareIdent = fn.Sel
		}
		if bareIdent != nil && bareIdent.Name == funcName && isCoreWasmFunc(pkg.TypesInfo.Uses[bareIdent], funcName) {
			collErr = fmt.Errorf("gothic: %s[T] requires an explicit type argument — write %s[T](...) in %s", funcName, funcName, pagePath)
			return false
		}
		return true
	})
	if collErr != nil {
		return nil, collErr
	}
	return roots, nil
}

// resolveDecodeTypeArg resolves the concrete type argument of a Decode[T] call.
// It prefers TypesInfo.Instances (correct under inference), falling back to the
// syntactic type-argument expression. Returns nil for a non-*types.Named T (a
// pointer, slice, map, or basic type — which the caller rejects with a clear
// error).
func resolveDecodeTypeArg(pkg *packages.Package, nameIdent *ast.Ident, typeArg ast.Expr) *types.Named {
	if inst, ok := pkg.TypesInfo.Instances[nameIdent]; ok && inst.TypeArgs != nil && inst.TypeArgs.Len() > 0 {
		if named, ok := inst.TypeArgs.At(0).(*types.Named); ok {
			return named
		}
	}
	if typeArg != nil {
		if tv, ok := pkg.TypesInfo.Types[typeArg]; ok && tv.Type != nil {
			if named, ok := tv.Type.(*types.Named); ok {
				return named
			}
		}
		if id, ok := typeArg.(*ast.Ident); ok {
			if tn, ok := pkg.TypesInfo.Uses[id].(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok {
					return named
				}
			}
		}
	}
	return nil
}

// jsonNestedRef is a nested struct type discovered while walking a reader's
// fields, carrying the Go type string (bare or package-qualified) under which its
// own reader/writer must be emitted.
type jsonNestedRef struct {
	named  *types.Named
	goType string
}

// buildJSONReaderStructs walks each root and every nested named struct reachable
// from it, returning a deduplicated slice of reader structs plus the per-root
// refs, both in stable discovery order.
//
// A nested struct field is recursed into when its type lives in the SAME package
// as its ENCLOSING struct type (not the page package): that is the shared-DTO
// pattern — a DTO package holding a root struct and its nested types. Such a
// nested struct is emitted under the enclosing struct's qualifier (bare for a
// page-local container, `pkg.Type` / ident `pkg_Type` for a cross-package one),
// which resolves because the enclosing type's package is already imported into
// main. A nested struct field in any OTHER package (a third, not-necessarily
// imported package) is soft-failed and decodes as the field's zero value.
//
// pagePkg is retained only to keep the page-local (bare, tree-shaken) case exact:
// a bare-qualifier container recurses into a nested struct only when it is truly
// page-local (so its declaration is inlined), otherwise soft-fails.
func buildJSONReaderStructs(roots []jsonDecodeRoot, pagePkg *types.Package) (readers []jsonReaderType, rootRefs []jsonRootRef) {
	seen := map[string]bool{}
	type qitem struct {
		named  *types.Named
		goType string
	}
	var queue []qitem
	for _, r := range roots {
		rootRefs = append(rootRefs, jsonRootRef{Ident: r.ident, GoType: r.goType})
		queue = append(queue, qitem{named: r.named, goType: r.goType})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		ident := sanitizeTypeIdent(it.goType)
		if seen[ident] {
			continue
		}
		seen[ident] = true
		st, ok := it.named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		containerPkg := it.named.Obj().Pkg()
		prefix := jsonQualifierPrefix(it.goType)
		var discovered []jsonNestedRef
		fields := collectJSONFields(st, containerPkg, prefix, pagePkg, &discovered)
		readers = append(readers, jsonReaderType{Ident: ident, GoType: it.goType, Fields: fields})
		for _, d := range discovered {
			queue = append(queue, qitem{named: d.named, goType: d.goType})
		}
	}
	return readers, rootRefs
}

// jsonQualifierPrefix returns the package qualifier of a Go type string
// ("shared.Echo" → "shared", "User" → "").
func jsonQualifierPrefix(goType string) string {
	if i := strings.LastIndex(goType, "."); i >= 0 {
		return goType[:i]
	}
	return ""
}

// collectJSONFields returns the readable fields of a struct, flattening embedded
// (anonymous) struct fields the way encoding/json promotes them. An embedded
// field with an explicit json name is treated as a normal named field (json does
// not promote a tagged embed); `json:"-"` skips it; an untagged embedded struct
// (or *struct) has its exported fields promoted against the PARENT object.
//
// containerPkg / prefix describe the enclosing struct type: nested struct fields
// resolve relative to it (see jsonFieldType).
func collectJSONFields(st *types.Struct, containerPkg *types.Package, prefix string, pagePkg *types.Package, discovered *[]jsonNestedRef) []fieldInfo {
	var out []fieldInfo
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		rawTag, _ := reflect.StructTag(st.Tag(i)).Lookup("json")

		if f.Anonymous() {
			tagName := ""
			if rawTag != "" {
				tagName = strings.Split(rawTag, ",")[0]
			}
			if rawTag == "-" {
				continue // json:"-" — skip
			}
			if tagName == "" {
				// Untagged embed → promote its fields into the parent object. The
				// promoted fields resolve relative to the embedded type's own package
				// (same qualifier when the embed is in the container's package).
				if embSt := underlyingStruct(f.Type()); embSt != nil {
					embPkg, embPrefix := containerPkg, prefix
					if en := namedOf(f.Type()); en != nil && en.Obj().Pkg() != nil {
						embPkg = en.Obj().Pkg()
						if embPkg != containerPkg {
							embPrefix = "" // cross-package embed: promoted nested structs soft-fail
						}
					}
					out = append(out, collectJSONFields(embSt, embPkg, embPrefix, pagePkg, discovered)...)
				}
				// Embedded non-struct (e.g. embedded primitive) is not promotable
				// as an object key — skip it rather than read a wrong key.
				continue
			}
			// Tagged embed → falls through and is read as a normal named field.
		}

		ref, goType, ok := jsonFieldType(f.Type(), containerPkg, prefix, pagePkg, discovered)
		if !ok {
			continue
		}
		out = append(out, fieldInfo{Name: f.Name(), Type: goType, TypeRef: ref, JSONTag: rawTag})
	}
	return out
}

// underlyingStruct returns the *types.Struct behind t, dereferencing a single
// pointer, or nil when t is not a (pointer-to-)struct.
func underlyingStruct(t types.Type) *types.Struct {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	switch u := t.(type) {
	case *types.Named:
		if s, ok := u.Underlying().(*types.Struct); ok {
			return s
		}
	case *types.Struct:
		return u
	}
	return nil
}

// namedOf returns the *types.Named behind t, dereferencing a single pointer, or
// nil when t is not a named type.
func namedOf(t types.Type) *types.Named {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return n
	}
	return nil
}

// jsonFieldType converts a go/types field type into (typeRef, Go-source type
// string, supported?). A nested named struct is included (and appended to
// *discovered) when it lives in the SAME package as its enclosing struct type
// (containerPkg) — emitted under that container's qualifier (prefix). A struct in
// any other package is soft-failed. For a bare-qualifier container the nested
// struct must additionally be page-local so its declaration is inlined; otherwise
// it soft-fails. Returns ok=false for any type the decoder cannot handle.
func jsonFieldType(t types.Type, containerPkg *types.Package, prefix string, pagePkg *types.Package, discovered *[]jsonNestedRef) (typeRef, string, bool) {
	switch u := t.(type) {
	case *types.Basic:
		name := basicJSONName(u)
		if name == "" {
			return nil, "", false
		}
		return Named{Name: name}, name, true

	case *types.Named:
		obj := u.Obj()
		// time.Time (and other stdlib structs) have unexported fields we cannot
		// read — skip so the field decodes as its zero value.
		if obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Time" {
			return nil, "", false
		}
		if _, ok := u.Underlying().(*types.Struct); ok {
			// Recurse into a nested struct in the SAME package as its container. For
			// a bare-qualifier container, also require it to be page-local so the
			// inlined declaration is in scope; a qualified container's package is
			// already imported, so the qualified reference resolves.
			if obj.Pkg() == containerPkg && (prefix != "" || obj.Pkg() == pagePkg) {
				ngoType := obj.Name()
				if prefix != "" {
					ngoType = prefix + "." + obj.Name()
				}
				*discovered = append(*discovered, jsonNestedRef{named: u, goType: ngoType})
				return Named{Name: ngoType}, ngoType, true
			}
			return nil, "", false // struct in another package: soft-fail to zero
		}
		// Named non-struct (alias over primitive/slice/map/…): unsupported for now.
		return nil, "", false

	case *types.Pointer:
		er, egt, ok := jsonFieldType(u.Elem(), containerPkg, prefix, pagePkg, discovered)
		if !ok {
			return nil, "", false
		}
		return PointerOf{Elem: er}, "*" + egt, true

	case *types.Slice:
		// []byte is JSON base64, not an array — not supported here.
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			return nil, "", false
		}
		er, egt, ok := jsonFieldType(u.Elem(), containerPkg, prefix, pagePkg, discovered)
		if !ok {
			return nil, "", false
		}
		return SliceOf{Elem: er}, "[]" + egt, true

	case *types.Map:
		// JSON object keys are strings — only a plain `string` key is supported
		// (a named string alias would make dst[key] a type error).
		kb, ok := u.Key().(*types.Basic)
		if !ok || kb.Kind() != types.String {
			return nil, "", false
		}
		vr, vgt, ok := jsonFieldType(u.Elem(), containerPkg, prefix, pagePkg, discovered)
		if !ok {
			return nil, "", false
		}
		return MapOf{Key: Named{Name: "string"}, Val: vr}, "map[string]" + vgt, true
	}
	return nil, "", false
}

// basicJSONName maps a supported basic kind to its Go keyword, or "" for kinds
// the JSON decoder does not handle (complex, uintptr, unsafe.Pointer, untyped).
func basicJSONName(b *types.Basic) string {
	switch b.Kind() {
	case types.Bool:
		return "bool"
	case types.Int:
		return "int"
	case types.Int8:
		return "int8"
	case types.Int16:
		return "int16"
	case types.Int32:
		return "int32"
	case types.Int64:
		return "int64"
	case types.Uint:
		return "uint"
	case types.Uint8:
		return "uint8"
	case types.Uint16:
		return "uint16"
	case types.Uint32:
		return "uint32"
	case types.Uint64:
		return "uint64"
	case types.Float32:
		return "float32"
	case types.Float64:
		return "float64"
	case types.String:
		return "string"
	}
	return ""
}
