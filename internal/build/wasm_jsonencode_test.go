package helpers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestJSONEncodeLines_RepresentativeStruct exercises jsonWriteValue +
// buildJSONEncodeData over a struct covering primitives, a json-tag rename, a
// nested struct, a slice, a pointer, and a json:"-" skip — asserting the
// generated append statements, the deterministic key prefixes (leading comma on
// every field after the first), and that no reflect / encoding/json leaks in.
func TestJSONEncodeLines_RepresentativeStruct(t *testing.T) {
	writers := []jsonReaderType{
		{Ident: "User", GoType: "User", Fields: []fieldInfo{
			jsonField("Name", "string", "user_name"),
			jsonField("Age", "int", ""),          // no tag → key "Age"
			jsonField("Ratio", "float32", "ratio"),
			jsonField("Active", "bool", "active"),
			jsonField("Address", "Address", "address"),
			jsonField("Tags", "[]string", "tags"),
			jsonField("Nick", "*string", "nick"),
			jsonField("Secret", "string", "-"), // skipped
		}},
		{Ident: "Address", GoType: "Address", Fields: []fieldInfo{
			jsonField("City", "string", "city"),
		}},
	}
	roots := []jsonRootRef{{Ident: "User", GoType: "User"}}

	h := DefaultWasmHelper()
	writerData, encoderData := h.buildJSONEncodeData(writers, roots)

	if len(encoderData) != 1 || encoderData[0].Ident != "User" || encoderData[0].GoType != "User" {
		t.Fatalf("encoders: got %+v, want one (Ident=User, GoType=User)", encoderData)
	}

	var userW JSONWriterData
	var addrSeen bool
	for _, w := range writerData {
		switch w.Ident {
		case "User":
			userW = w
		case "Address":
			addrSeen = true
		}
	}
	if !addrSeen {
		t.Fatalf("expected an Address writer to be emitted")
	}

	// json:"-" Secret must be dropped → 7 emitted fields.
	if len(userW.Fields) != 7 {
		t.Fatalf("expected 7 emitted fields (Secret dropped), got %d: %+v", len(userW.Fields), userW.Fields)
	}
	// First field has no leading comma; the rest do.
	if got := userW.Fields[0].KeyPrefixLit; got != `"\"user_name\":"` {
		t.Errorf("first field key prefix: got %s, want %q", got, `"\"user_name\":"`)
	}
	if got := userW.Fields[1].KeyPrefixLit; got != `",\"Age\":"` { // no tag → field name, leading comma
		t.Errorf("second field key prefix: got %s, want a leading comma", got)
	}

	var lines strings.Builder
	for _, f := range userW.Fields {
		lines.WriteString(f.ValueLine)
		lines.WriteString("\n")
	}
	got := lines.String()

	wantContains := []string{
		"_jsonAppendString(b, string(v.Name))",
		"_jsonAppendInt(b, int64(v.Age))",
		"_jsonAppendFloat(b, float64(v.Ratio), 32)",
		`if v.Active { *b = append(*b, "true"...) } else { *b = append(*b, "false"...) }`,
		"_jsonWrite_Address(b, v.Address)",
		`if v.Tags == nil { *b = append(*b, "null"...) }`, // nil slice → null
		"for _i0, _e0 := range v.Tags",
		`if v.Nick == nil { *b = append(*b, "null"...) } else { _jsonAppendString(b, string((*v.Nick))) }`,
	}
	for _, want := range wantContains {
		if !strings.Contains(got, want) {
			t.Errorf("encode lines missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "v.Secret") {
		t.Errorf(`json:"-" field Secret should be omitted:\n%s`, got)
	}
	for _, banned := range []string{"reflect", "encoding/json"} {
		if strings.Contains(got, banned) {
			t.Errorf("generated encode lines must not reference %q:\n%s", banned, got)
		}
	}
}

// TestJSONEncode_StringEscaping checks that jsonQuoteString (build-time keys) and
// the emitted _jsonAppendString (runtime values) escape control + quote/backslash
// characters as valid JSON.
func TestJSONEncode_StringEscaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a"b`, `"a\"b"`},
		{"a\\b", `"a\\b"`},
		{"line1\nline2", `"line1\nline2"`},
		{"tab\there", `"tab\there"`},
		{"\x01", "\"\\u0001\""},
	}
	for _, c := range cases {
		if got := jsonQuoteString(c.in); got != c.want {
			t.Errorf("jsonQuoteString(%q): got %s, want %s", c.in, got, c.want)
		}
	}
}

// TestJSONEncode_RoundTrip is a genuine round-trip: it synthesizes a host Go
// program from the generated encoder + the shared helpers, `go run`s it to emit
// JSON for a known value, then unmarshals that JSON with encoding/json (in the
// test only) and asserts the field values survive — including a string that needs
// escaping (`"` and newline). The generated encoder itself uses no reflect /
// encoding/json.
func TestJSONEncode_RoundTrip(t *testing.T) {
	writers := []jsonReaderType{
		{Ident: "User", GoType: "User", Fields: []fieldInfo{
			jsonField("Name", "string", "user_name"),
			jsonField("Age", "int", "age"),
			jsonField("Score", "float64", "score"),
			jsonField("Active", "bool", "active"),
			jsonField("Address", "Address", "address"),
			jsonField("Tags", "[]string", "tags"),
			jsonField("Nick", "*string", "nick"),
		}},
		{Ident: "Address", GoType: "Address", Fields: []fieldInfo{
			jsonField("City", "string", "city"),
			jsonField("Zip", "int", "zip"),
		}},
	}
	roots := []jsonRootRef{{Ident: "User", GoType: "User"}}
	h := DefaultWasmHelper()
	writerData, encoderData := h.buildJSONEncodeData(writers, roots)

	var prog strings.Builder
	prog.WriteString("package main\n\nimport (\n\t\"os\"\n\t\"strconv\"\n)\n\n")
	prog.WriteString("type Address struct { City string; Zip int }\n")
	prog.WriteString("type User struct { Name string; Age int; Score float64; Active bool; Address Address; Tags []string; Nick *string }\n\n")
	prog.WriteString(jsonEncodeHelpersSrc)
	prog.WriteString("\n")
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
	nick := "nick\"y"
	u := User{Name: "a\"b\nc", Age: 42, Score: 3.5, Active: true,
		Address: Address{City: "NYC", Zip: 10001}, Tags: []string{"x", "y"}, Nick: &nick}
	os.Stdout.Write(_jsonEncode_User(u))
}
`)

	dir := t.TempDir()
	progPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(progPath, []byte(prog.String()), 0644); err != nil {
		t.Fatalf("write program: %v", err)
	}
	// Minimal module so `go run` doesn't reach for the workspace.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module rt\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run generated encoder failed: %v\n%s\n---program---\n%s", err, out, prog.String())
	}

	// The generated JSON must round-trip through the field values.
	var got struct {
		Name    string   `json:"user_name"`
		Age     int      `json:"age"`
		Score   float64  `json:"score"`
		Active  bool     `json:"active"`
		Address struct {
			City string `json:"city"`
			Zip  int    `json:"zip"`
		} `json:"address"`
		Tags []string `json:"tags"`
		Nick *string  `json:"nick"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("generated JSON is not valid / does not unmarshal: %v\nJSON: %s", err, out)
	}
	if got.Name != "a\"b\nc" {
		t.Errorf("Name round-trip (escaping): got %q", got.Name)
	}
	if got.Age != 42 || got.Score != 3.5 || !got.Active {
		t.Errorf("scalar round-trip failed: %+v", got)
	}
	if got.Address.City != "NYC" || got.Address.Zip != 10001 {
		t.Errorf("nested round-trip failed: %+v", got.Address)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "x" || got.Tags[1] != "y" {
		t.Errorf("slice round-trip failed: %+v", got.Tags)
	}
	if got.Nick == nil || *got.Nick != "nick\"y" {
		t.Errorf("pointer round-trip failed: %+v", got.Nick)
	}
}
