package helpers

// Reflection-free JSON decode-line generation for the Decode[T] path (Phase 6).
//
// Model mirrors the binary codec (wasm_codec.go): jsonDecodeLines dispatches per
// field and buildJSONDecodeData folds the results into template data — but the
// wire source here is the runtime's parsed `map[string]any` (Response.MapAny →
// jsonparse.go), NOT the binary Encoder/Decoder. The generated code imports
// NEITHER reflect NOR encoding/json.
//
// Coercion (locked decision D5), reading a JSON value out of map[string]any:
//   - JSON number is float64: int/int8/.../uint64 → <kind>(f) (int64 > 2^53 loses
//     precision — inherent to float64's 53-bit mantissa); float32 → float32(f);
//     float64 → direct.
//   - string → string, bool → bool.
//   - null or MISSING key → the field's zero value (the comma-ok type assertion
//     fails, leaving the pre-zeroed field untouched).
//   - nested struct → recurse via _jsonRead_<T>; []T → iterate []any; *T →
//     allocate when the value is present (non-nil), else leave nil.
//   - unknown JSON keys are ignored (we only read keys we know).

import (
	"fmt"
	"strings"
)

// jsonFieldKey resolves the JSON object key for a field: the first comma segment
// of its `json:"..."` tag, or the Go field name when no tag / empty name. Returns
// skip=true for `json:"-"` (the "ignore this field" spelling).
func jsonFieldKey(fi fieldInfo) (key string, skip bool) {
	tag := strings.TrimSpace(fi.JSONTag)
	if tag == "" {
		return fi.Name, false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "-" && len(parts) == 1 {
		return "", true
	}
	if name == "" {
		return fi.Name, false
	}
	return name, false
}

// buildJSONDecodeData turns the reachable reader structs + root refs into the
// template data for the generated _jsonRead_<Ident> readers and
// _jsonDecode_<Ident> entry points. structNames (keyed by reader Ident, which is
// the bare name for every page-local struct a field can reference) drives nested
// struct dispatch inside jsonDecodeLines.
func (h *WasmHelper) buildJSONDecodeData(readers []jsonReaderType, roots []jsonRootRef) ([]JSONReaderData, []JSONDecoderData) {
	// Keyed by GoType (bare or `pkg.Type`) — the value a nested struct field's
	// TypeRef.Name carries — so jsonAssign's structNames lookup matches.
	structNames := make(map[string]bool, len(readers))
	for _, s := range readers {
		structNames[s.GoType] = true
	}
	readerData := make([]JSONReaderData, 0, len(readers))
	for _, s := range readers {
		rd := JSONReaderData{Ident: s.Ident, GoType: s.GoType}
		for _, f := range s.Fields {
			line := jsonDecodeLines(f, structNames)
			if line == "" {
				continue
			}
			rd.Fields = append(rd.Fields, JSONFieldDecode{DecLine: line})
		}
		readerData = append(readerData, rd)
	}
	decoderData := make([]JSONDecoderData, 0, len(roots))
	for _, r := range roots {
		decoderData = append(decoderData, JSONDecoderData{Ident: r.Ident, GoType: r.GoType})
	}
	return readerData, decoderData
}

// jsonDecodeLines returns the decode statement for one struct field, reading its
// JSON value out of the reader's `m map[string]any` into `out.<Field>`. Returns
// "" when the field is skipped (json:"-", no TypeRef) or its type is unsupported
// (field left at its zero value).
func jsonDecodeLines(fi fieldInfo, structNames map[string]bool) string {
	key, skip := jsonFieldKey(fi)
	if skip || fi.TypeRef == nil {
		return ""
	}
	dst := "out." + fi.Name
	src := fmt.Sprintf("m[%q]", key)
	return jsonAssign(dst, src, fi.TypeRef, structNames, 0)
}

// jsonAssign emits Go statement(s) that read the JSON `any` value srcExpr into
// the lvalue dst (of Go type ref), coercing per D5. depth seeds unique temp-var
// names so nested slices/pointers/maps never collide. Returns "" for a type the
// reflection-free decoder cannot handle (caller leaves the field zero).
func jsonAssign(dst, src string, ref typeRef, structNames map[string]bool, depth int) string {
	s := fmt.Sprintf("%d", depth)
	switch r := ref.(type) {
	case Named:
		switch r.Name {
		case "string":
			return fmt.Sprintf("if _s%s, _ok%s := %s.(string); _ok%s { %s = _s%s }", s, s, src, s, dst, s)
		case "bool":
			return fmt.Sprintf("if _b%s, _ok%s := %s.(bool); _ok%s { %s = _b%s }", s, s, src, s, dst, s)
		case "float64":
			return fmt.Sprintf("if _f%s, _ok%s := %s.(float64); _ok%s { %s = _f%s }", s, s, src, s, dst, s)
		case "float32":
			return fmt.Sprintf("if _f%s, _ok%s := %s.(float64); _ok%s { %s = float32(_f%s) }", s, s, src, s, dst, s)
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
			return fmt.Sprintf("if _f%s, _ok%s := %s.(float64); _ok%s { %s = %s(_f%s) }", s, s, src, s, dst, r.Name, s)
		default:
			if structNames[r.Name] {
				return fmt.Sprintf("if _m%s, _ok%s := %s.(map[string]any); _ok%s { %s = _jsonRead_%s(_m%s) }", s, s, src, s, dst, sanitizeTypeIdent(r.Name), s)
			}
			return ""
		}

	case SliceOf:
		elemGT := r.Elem.String()
		inner := jsonAssign(fmt.Sprintf("%s[_i%s]", dst, s), fmt.Sprintf("_a%s[_i%s]", s, s), r.Elem, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf(
			"if _a%s, _ok%s := %s.([]any); _ok%s { %s = make([]%s, len(_a%s)); for _i%s := range _a%s { %s } }",
			s, s, src, s, dst, elemGT, s, s, s, inner)

	case PointerOf:
		baseGT := r.Elem.String()
		inner := jsonAssign("_p"+s, src, r.Elem, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf("if %s != nil { var _p%s %s; %s; %s = &_p%s }", src, s, baseGT, inner, dst, s)

	case MapOf:
		valGT := r.Val.String()
		inner := jsonAssign("_mv"+s, "_vv"+s, r.Val, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf(
			"if _mm%s, _ok%s := %s.(map[string]any); _ok%s { %s = make(map[string]%s, len(_mm%s)); for _mk%s, _vv%s := range _mm%s { var _mv%s %s; %s; %s[_mk%s] = _mv%s } }",
			s, s, src, s, dst, valGT, s, s, s, s, s, valGT, inner, dst, s, s)
	}
	return ""
}
