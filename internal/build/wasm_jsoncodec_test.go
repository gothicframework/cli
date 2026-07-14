package helpers

import (
	"strings"
	"testing"
)

// jsonField builds a fieldInfo (with a populated TypeRef via testFieldInfo) and
// attaches a raw json tag value — mirroring what the go/types reader / topic
// parser store on fieldInfo.JSONTag.
func jsonField(name, typeStr, jsonTag string) fieldInfo {
	fi := testFieldInfo(name, typeStr)
	fi.JSONTag = jsonTag
	return fi
}

// TestJSONDecodeLines_RepresentativeStruct exercises jsonDecodeLines +
// buildJSONDecodeData over a struct covering primitives, a json-tag rename to
// snake_case, a nested struct, a slice of primitives, a slice of structs, a
// pointer, and a json:"-" ignored field — asserting the generated decode
// statements and confirming NO reflect / encoding/json leaks in.
func TestJSONDecodeLines_RepresentativeStruct(t *testing.T) {
	readers := []jsonReaderType{
		{Ident: "User", GoType: "User", Fields: []fieldInfo{
			jsonField("Name", "string", "user_name"),        // snake_case rename
			jsonField("Age", "int", ""),                     // no tag → key "Age"
			jsonField("Score", "float64", "score"),          // float64 direct
			jsonField("Ratio", "float32", "ratio"),          // float32 cast
			jsonField("Active", "bool", ""),                 // bool
			jsonField("Address", "Address", "address"),      // nested struct
			jsonField("Tags", "[]string", "tags"),           // slice of primitive
			jsonField("Items", "[]Item", "items"),           // slice of struct
			jsonField("Nick", "*string", "nick"),            // pointer to primitive
			jsonField("Secret", "string", "-"),              // ignored
		}},
		{Ident: "Address", GoType: "Address", Fields: []fieldInfo{
			jsonField("City", "string", "city"),
			jsonField("Zip", "int", "zip"),
		}},
		{Ident: "Item", GoType: "Item", Fields: []fieldInfo{
			jsonField("SKU", "string", "sku"),
		}},
	}
	roots := []jsonRootRef{{Ident: "User", GoType: "User"}}

	h := DefaultWasmHelper()
	readerData, decoderData := h.buildJSONDecodeData(readers, roots)

	if len(decoderData) != 1 || decoderData[0].Ident != "User" || decoderData[0].GoType != "User" {
		t.Fatalf("decoders: got %+v, want one entry (Ident=User, GoType=User)", decoderData)
	}

	// Collect the User reader's decode lines.
	var userLines string
	var addrSeen, itemSeen bool
	for _, rd := range readerData {
		switch rd.Ident {
		case "User":
			var b strings.Builder
			for _, f := range rd.Fields {
				b.WriteString(f.DecLine)
				b.WriteString("\n")
			}
			userLines = b.String()
		case "Address":
			addrSeen = true
		case "Item":
			itemSeen = true
		}
	}
	if !addrSeen || !itemSeen {
		t.Fatalf("expected Address and Item readers to be emitted, got %d readers", len(readerData))
	}

	wantContains := []string{
		`if _s0, _ok0 := m["user_name"].(string); _ok0 { out.Name = _s0 }`,     // rename + string
		`if _f0, _ok0 := m["Age"].(float64); _ok0 { out.Age = int(_f0) }`,      // int coercion, default key
		`if _f0, _ok0 := m["score"].(float64); _ok0 { out.Score = _f0 }`,       // float64 direct
		`out.Ratio = float32(_f0)`,                                             // float32 cast
		`if _b0, _ok0 := m["Active"].(bool); _ok0 { out.Active = _b0 }`,        // bool
		`if _m0, _ok0 := m["address"].(map[string]any); _ok0 { out.Address = _jsonRead_Address(_m0) }`, // nested
		`out.Tags = make([]string, len(_a0))`,                                  // []string
		`out.Items = make([]Item, len(_a0))`,                                   // []struct
		`out.Items[_i0] = _jsonRead_Item(_m1)`,                                 // slice-of-struct element
		`if m["nick"] != nil { var _p0 string;`,                               // pointer alloc-if-present
		`out.Nick = &_p0`,
	}
	for _, want := range wantContains {
		if !strings.Contains(userLines, want) {
			t.Errorf("User decode lines missing expected fragment:\n  want: %s\n  got:\n%s", want, userLines)
		}
	}

	// json:"-" field must produce NO statement.
	if strings.Contains(userLines, "out.Secret") {
		t.Errorf("json:\"-\" field Secret should be omitted, but a line references it:\n%s", userLines)
	}

	// The generated decode logic must be reflection-free and json-free.
	for _, banned := range []string{"reflect", "encoding/json"} {
		if strings.Contains(userLines, banned) {
			t.Errorf("generated decode lines must not reference %q:\n%s", banned, userLines)
		}
	}
}

// TestJSONFieldKey covers the json-tag key resolution rules.
func TestJSONFieldKey(t *testing.T) {
	cases := []struct {
		name, tag string
		wantKey   string
		wantSkip  bool
	}{
		{"Name", "", "Name", false},                    // no tag → field name
		{"Name", "user_name", "user_name", false},      // rename
		{"Name", "user_name,omitempty", "user_name", false}, // options stripped
		{"Name", "-", "", true},                        // ignore
		{"Name", ",omitempty", "Name", false},          // empty name → field name
	}
	for _, c := range cases {
		key, skip := jsonFieldKey(fieldInfo{Name: c.name, JSONTag: c.tag})
		if key != c.wantKey || skip != c.wantSkip {
			t.Errorf("jsonFieldKey(name=%q tag=%q): got (%q,%v), want (%q,%v)",
				c.name, c.tag, key, skip, c.wantKey, c.wantSkip)
		}
	}
}
