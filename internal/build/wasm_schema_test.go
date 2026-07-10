package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaStructNames returns the struct-name set for a slice of structInfo.
func schemaStructNames(structs []structInfo) map[string]bool {
	m := make(map[string]bool, len(structs))
	for _, s := range structs {
		m[s.Name] = true
	}
	return m
}

// TestSchemaFingerprintStable verifies the Phase 15 schema seam: the schemaId is
// STABLE for an unchanged type and CHANGES when the type's fields/tags change.
func TestSchemaFingerprintStable(t *testing.T) {
	h := DefaultWasmHelper()

	base := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
	}}
	names := schemaStructNames([]structInfo{base})

	id1, _, desc1, err := h.schemaFor(base, names, nil, nil)
	if err != nil {
		t.Fatalf("schemaFor base: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected a non-empty schemaId")
	}

	// Re-computing the SAME type yields the SAME id.
	id1b, _, _, _ := h.schemaFor(base, names, nil, nil)
	if id1 != id1b {
		t.Errorf("schemaId not stable for unchanged type: %q vs %q", id1, id1b)
	}

	// The descriptor is derived from the codec, so it names the wire ops.
	if !strings.Contains(desc1, "Count=I64") || !strings.Contains(desc1, "Label=String") {
		t.Errorf("descriptor missing expected wire ops:\n%s", desc1)
	}

	// Changing a field's WIRE TYPE via a gothic tag changes the id.
	taggedField := fieldInfo{Name: "Count", Type: "int", TypeRef: testFieldInfo("Count", "int").TypeRef, GothicTag: "i32"}
	tagged := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		taggedField,
		testFieldInfo("Label", "string"),
	}}
	id2, _, desc2, _ := h.schemaFor(tagged, names, nil, nil)
	if id2 == id1 {
		t.Errorf("schemaId must change when a field's wire width changes (i64→i32); both %q", id1)
	}
	if !strings.Contains(desc2, "Count=I32") {
		t.Errorf("tagged descriptor should encode Count as I32:\n%s", desc2)
	}

	// Reordering fields changes the id (field order is part of the wire shape).
	reordered := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Label", "string"),
		testFieldInfo("Count", "int"),
	}}
	id3, _, _, _ := h.schemaFor(reordered, names, nil, nil)
	if id3 == id1 {
		t.Errorf("schemaId must change when fields are reordered; both %q", id1)
	}

	// Adding a field changes the id.
	added := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
		testFieldInfo("Active", "bool"),
	}}
	id4, _, _, _ := h.schemaFor(added, names, nil, nil)
	if id4 == id1 {
		t.Errorf("schemaId must change when a field is added; both %q", id1)
	}
}

// TestSchemaSeamInGeneratedManager renders the topic-manager template and asserts
// the generated registration output carries the schemaId + descriptor and calls
// GothicRegisterSchema — proving the seam is threaded into registration.
func TestSchemaSeamInGeneratedManager(t *testing.T) {
	h := DefaultWasmHelper()

	s := structInfo{Name: "Page", KeyName: "page", Fields: []fieldInfo{
		testFieldInfo("Count", "int"),
		testFieldInfo("Label", "string"),
	}}
	names := schemaStructNames([]structInfo{s})
	id, lit, _, err := h.schemaFor(s, names, nil, nil)
	if err != nil {
		t.Fatalf("schemaFor: %v", err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "main.go")
	if err := h.Template.UpdateFromTemplateFS(WasmTemplateFS, tmplTopicManagerMain, out, WasmTopicManagerMainData{
		StructName:          s.Name,
		KeyName:             s.KeyName,
		SchemaID:            id,
		SchemaDescriptorLit: lit,
	}); err != nil {
		t.Fatalf("render manager template: %v", err)
	}
	rendered, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered: %v", err)
	}
	got := string(rendered)

	wants := []string{
		`const _gothicSchemaID = "` + id + `"`,
		`GothicRegisterSchema("page", _gothicSchemaID, _gothicSchemaDescriptor)`,
		`Count=I64`, // descriptor content embedded in the const literal
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("generated manager missing %q\n---\n%s", w, got)
		}
	}
}
