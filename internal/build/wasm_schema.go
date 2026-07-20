package helpers

import (
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
)

// wasm_schema.go implements the SCHEMA SEAM.
//
// From the SAME AST pass that builds the per-field codecs, we derive a compact,
// canonical descriptor of each topic struct's wire shape — field order, wire
// types, and gothic-tag widths — plus a content-hash `schemaId`. Both are
// threaded into the registration handshake (GothicRegisterSchema) as a reserved,
// additive control-plane slot for a FUTURE generic wire interpreter.
//
// Nothing interprets the descriptor in v3.0. It is written once, off the
// data-plane (never on per-field broadcasts), and read back by no 3.0 consumer.

// wireOpRe extracts the wire operations from a generated encode line. It matches
// both primitive encoder calls (`e.I64(`, `e.String(`, `e.U32(` …) and nested
// struct encodes (`_encode_Item(`). The ordered op sequence is a faithful,
// stable fingerprint of a field's wire layout: a `gothic:"i32"` tag surfaces as
// `I32`, `gothic:"skip"` yields no ops at all, and a `[]Item` surfaces as the
// length prefix `U32` followed by `struct:Item`.
var wireOpRe = regexp.MustCompile(`e\.([A-Za-z0-9]+)\(|_encode_([A-Za-z0-9]+)\(`)

// fieldWireToken returns the comma-joined wire-op token for one field's encode
// line, or "skip" when the field contributes no bytes (gothic:"skip").
func fieldWireToken(encLine string) string {
	matches := wireOpRe.FindAllStringSubmatch(encLine, -1)
	toks := make([]string, 0, len(matches))
	for _, m := range matches {
		switch {
		case m[1] != "":
			toks = append(toks, m[1])
		case m[2] != "":
			toks = append(toks, "struct:"+m[2])
		}
	}
	if len(toks) == 0 {
		return "skip"
	}
	return strings.Join(toks, ",")
}

// schemaDescriptor builds the canonical wire descriptor for a topic struct.
// The format is intentionally line-oriented and human-diffable:
//
//	gothic-wire/1
//	<Struct> key=<keyName>
//	  <Field>=<op>,<op>,...
//	  ...
//
// It is derived from the same codecLines() the codecs use, so the descriptor can
// never drift from the actual wire format.
func (h *WasmHelper) schemaDescriptor(s structInfo, structNames map[string]bool, aliases map[string]string, refAliases map[string]typeRef) (string, error) {
	var b strings.Builder
	b.WriteString("gothic-wire/1\n")
	b.WriteString(s.Name)
	if s.KeyName != "" {
		b.WriteString(" key=")
		b.WriteString(s.KeyName)
	}
	b.WriteByte('\n')
	for _, f := range s.Fields {
		enc, _, err := h.codecLines(f, structNames, aliases, refAliases)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  %s=%s\n", f.Name, fieldWireToken(enc))
	}
	return b.String(), nil
}

// schemaID is the crc32 (IEEE) content hash of a descriptor, as 8 hex chars.
// It is STABLE for an unchanged type and CHANGES whenever any field name, order,
// wire type, or gothic-tag width changes.
func schemaID(descriptor string) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(descriptor)))
}

// schemaFor computes (schemaID, quotedDescriptorLiteral) for a topic struct.
// The second return value is a ready-to-embed Go string literal (via
// strconv.Quote) so the multi-line descriptor drops into a generated `const`.
func (h *WasmHelper) schemaFor(s structInfo, structNames map[string]bool, aliases map[string]string, refAliases map[string]typeRef) (id, descriptorLit, descriptor string, err error) {
	descriptor, err = h.schemaDescriptor(s, structNames, aliases, refAliases)
	if err != nil {
		return "", "", "", err
	}
	return schemaID(descriptor), strconv.Quote(descriptor), descriptor, nil
}
