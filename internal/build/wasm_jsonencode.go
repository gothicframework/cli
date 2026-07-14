package helpers

// Reflection-free JSON encode-line generation for the Encode[T] path (Phase 7),
// the write-direction mirror of wasm_jsoncodec.go.
//
// jsonEncodeLines (via buildJSONEncodeData) emits, per struct field, a JSON
// key prefix literal plus statements that append the field's JSON value to a
// shared *[]byte buffer. The generated code imports NEITHER reflect NOR
// encoding/json — number formatting goes through a tiny set of emitted helpers
// (jsonEncodeHelpersSrc) backed by strconv (TinyGo-safe), and string escaping is
// done by the emitted _jsonAppendString.
//
// Serialization decisions (documented for Phase 11):
//   - Field order is the Go struct field order — deterministic, so output is
//     stable and testable.
//   - json tags: name rename honored; `json:"-"` skips; `,omitempty` (and every
//     other option) is IGNORED for v1 — the field is always emitted.
//   - nil slice / nil pointer / nil map → JSON null (matches encoding/json).
//   - map[string]T keys are emitted in sorted order (via the emitted
//     _jsonSortStrings) so map output is deterministic too.
//   - A field whose type the encoder cannot handle is OMITTED from the JSON
//     entirely (symmetric with Decode leaving it zero).

import (
	"fmt"
	"strconv"
	"strings"
)

// jsonEncodeHelpersSrc is the shared append/escape helper source emitted once per
// generated main when any _jsonWrite_<T> is present. Kept as a single source of
// truth so tests can synthesize a host-compilable program from the same text.
// It uses only strconv + []byte — no reflect, no encoding/json.
const jsonEncodeHelpersSrc = `func _jsonAppendString(b *[]byte, s string) {
	*b = append(*b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			*b = append(*b, '\\', '"')
		case '\\':
			*b = append(*b, '\\', '\\')
		case '\n':
			*b = append(*b, '\\', 'n')
		case '\r':
			*b = append(*b, '\\', 'r')
		case '\t':
			*b = append(*b, '\\', 't')
		case '\b':
			*b = append(*b, '\\', 'b')
		case '\f':
			*b = append(*b, '\\', 'f')
		default:
			if c < 0x20 {
				const _hex = "0123456789abcdef"
				*b = append(*b, '\\', 'u', '0', '0', _hex[c>>4], _hex[c&0xf])
			} else {
				*b = append(*b, c)
			}
		}
	}
	*b = append(*b, '"')
}

func _jsonAppendInt(b *[]byte, n int64) { *b = strconv.AppendInt(*b, n, 10) }

func _jsonAppendUint(b *[]byte, n uint64) { *b = strconv.AppendUint(*b, n, 10) }

func _jsonAppendFloat(b *[]byte, f float64, bits int) { *b = strconv.AppendFloat(*b, f, 'g', -1, bits) }

func _jsonSortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
`

// jsonQuoteString returns s as a JSON-quoted, JSON-escaped string literal (with
// surrounding double quotes). Used at build time to bake struct/tag keys — which
// are known statically — into the generated writer. Mirrors the escaping the
// runtime _jsonAppendString applies to dynamic values.
func jsonQuoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// buildJSONEncodeData turns the reachable writer structs + root refs into the
// template data for the generated _jsonWrite_<Ident> writers and
// _jsonEncode_<Ident> entry points. structNames (keyed by writer Ident — the bare
// name for every page-local struct a field can reference) drives nested struct
// dispatch inside jsonWriteValue.
func (h *WasmHelper) buildJSONEncodeData(writers []jsonReaderType, roots []jsonRootRef) ([]JSONWriterData, []JSONEncoderData) {
	// Keyed by GoType (bare or `pkg.Type`) — the value a nested struct field's
	// TypeRef.Name carries — so jsonWriteValue's structNames lookup matches.
	structNames := make(map[string]bool, len(writers))
	for _, s := range writers {
		structNames[s.GoType] = true
	}
	writerData := make([]JSONWriterData, 0, len(writers))
	for _, s := range writers {
		wd := JSONWriterData{Ident: s.Ident, GoType: s.GoType}
		first := true
		for _, f := range s.Fields {
			key, skip := jsonFieldKey(f)
			if skip || f.TypeRef == nil {
				continue
			}
			val := jsonWriteValue("v."+f.Name, f.TypeRef, structNames, 0)
			if val == "" {
				continue // unsupported field type — omit from JSON (symmetric with Decode)
			}
			prefix := jsonQuoteString(key) + ":"
			if !first {
				prefix = "," + prefix
			}
			first = false
			wd.Fields = append(wd.Fields, JSONFieldEncode{
				KeyPrefixLit: strconv.Quote(prefix),
				ValueLine:    val,
			})
		}
		writerData = append(writerData, wd)
	}
	encoderData := make([]JSONEncoderData, 0, len(roots))
	for _, r := range roots {
		encoderData = append(encoderData, JSONEncoderData{Ident: r.Ident, GoType: r.GoType})
	}
	return writerData, encoderData
}

// jsonWriteValue emits Go statement(s) that append the JSON encoding of the Go
// value expression src (of type ref) to the shared `b *[]byte`. depth seeds
// unique temp-var names so nested slices/pointers/maps never collide. Returns ""
// for a type the encoder cannot handle (caller omits the field).
func jsonWriteValue(src string, ref typeRef, structNames map[string]bool, depth int) string {
	s := fmt.Sprintf("%d", depth)
	switch r := ref.(type) {
	case Named:
		switch r.Name {
		case "string":
			return fmt.Sprintf("_jsonAppendString(b, string(%s))", src)
		case "bool":
			return fmt.Sprintf("if %s { *b = append(*b, \"true\"...) } else { *b = append(*b, \"false\"...) }", src)
		case "float64":
			return fmt.Sprintf("_jsonAppendFloat(b, float64(%s), 64)", src)
		case "float32":
			return fmt.Sprintf("_jsonAppendFloat(b, float64(%s), 32)", src)
		case "int", "int8", "int16", "int32", "int64":
			return fmt.Sprintf("_jsonAppendInt(b, int64(%s))", src)
		case "uint", "uint8", "uint16", "uint32", "uint64":
			return fmt.Sprintf("_jsonAppendUint(b, uint64(%s))", src)
		default:
			if structNames[r.Name] {
				return fmt.Sprintf("_jsonWrite_%s(b, %s)", sanitizeTypeIdent(r.Name), src)
			}
			return ""
		}

	case SliceOf:
		inner := jsonWriteValue("_e"+s, r.Elem, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf(
			"if %s == nil { *b = append(*b, \"null\"...) } else { *b = append(*b, '['); for _i%s, _e%s := range %s { if _i%s > 0 { *b = append(*b, ',') }; %s }; *b = append(*b, ']') }",
			src, s, s, src, s, inner)

	case PointerOf:
		inner := jsonWriteValue(fmt.Sprintf("(*%s)", src), r.Elem, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf("if %s == nil { *b = append(*b, \"null\"...) } else { %s }", src, inner)

	case MapOf:
		inner := jsonWriteValue("_mv"+s, r.Val, structNames, depth+1)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf(
			"if %s == nil { *b = append(*b, \"null\"...) } else { *b = append(*b, '{'); _mk%s := make([]string, 0, len(%s)); for _k%s := range %s { _mk%s = append(_mk%s, _k%s) }; _jsonSortStrings(_mk%s); for _i%s, _k%s := range _mk%s { if _i%s > 0 { *b = append(*b, ',') }; _jsonAppendString(b, _k%s); *b = append(*b, ':'); _mv%s := %s[_k%s]; %s }; *b = append(*b, '}') }",
			src, s, src, s, src, s, s, s, s, s, s, s, s, s, s, src, s, inner)
	}
	return ""
}
