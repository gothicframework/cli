package helpers

// Shared data structs for WASM template rendering.
// These types feed into the .gothicCli/templates/wasm/*.tmpl files via
// TemplateHelper.UpdateFromTemplate.

// FieldCodec holds pre-computed codec lines for a single struct field.
type FieldCodec struct {
	Name    string
	EncLine string
	DecLine string
}

// StructCodecData holds codec render data for one struct type.
type StructCodecData struct {
	Name   string
	Fields []FieldCodec
}

// KeyVarData holds data for a BinaryKey var declaration.
type KeyVarData struct {
	StructName string
	KeyName    string
}

// TopicFieldData holds data for one field in a topic struct.
type TopicFieldData struct {
	Name string
	Type string
}

// TopicTypeData holds data for a topic type struct declaration.
type TopicTypeData struct {
	TypeName string
	Fields   []TopicFieldData
}

// PerFieldCodec carries per-field codec lines for the consumer (page) template.
// Distinct from FieldCodec (which models a single line in a whole-struct codec).
type PerFieldCodec struct {
	FieldName string // Go field name
	FieldType string // Go field type as written in source (e.g. "int", "[]Item")
	EncLines  string // encoder lines (references v.<FieldName>)
	DecLines  string // decoder lines (references v.<FieldName>)
	// ChangedExpr is a Go boolean expression that is true when this field's
	// value differs from the last value SENT for it. It compares the current
	// value `v.<FieldName>` against the holder `c._lastSent<FieldName>` (the
	// exact operand names the consumer template uses). Used by _broadcastAll to
	// skip re-encoding + re-broadcasting an unchanged field — the fix for the
	// per-toggle whole-struct re-encode that ratcheted TinyGo's no-shrink heap.
	// Conservative-correct: a field may be reported changed when it isn't (extra
	// send, harmless) but is never reported unchanged when it changed.
	ChangedExpr string
}

// WasmTopicFuncData holds data for one WASM-side topic constructor + Set method.
type WasmTopicFuncData struct {
	CtorName    string
	TypeName    string
	StructName  string
	KeyName     string
	Fields      []TopicFieldData
	FieldCodecs []PerFieldCodec // one entry per source struct field, in declaration order
	// Schema seam (Phase 15): content-hash id + Go-quoted descriptor literal of
	// this topic's wire shape. Threaded into the consumer's registration via
	// GothicRegisterSchema; interpreted by nothing in v3.0.
	SchemaID            string
	SchemaDescriptorLit string
}

// ServerTopicFuncData holds data for one server-side topic stub.
type ServerTopicFuncData struct {
	CtorName   string
	TypeName   string
	StructName string
	Fields     []TopicFieldData
	// Schema seam (Phase 15): content-hash id + Go-quoted descriptor literal,
	// emitted as a package-level const so the server build carries the same
	// reserved wire descriptor. Interpreted by nothing in v3.0.
	SchemaID            string
	SchemaDescriptorLit string
}

// TopicGenData drives topic_gen.go.tmpl.
type TopicGenData struct {
	PkgName     string
	HasTopics   bool
	HasTime     bool // true when any struct field has type time.Time
	Codecs      []StructCodecData
	KeyVars     []KeyVarData
	TopicTypes  []TopicTypeData
	ServerFuncs []ServerTopicFuncData
}

// WasmPageMainData drives wasm_page_main.go.tmpl.
type WasmPageMainData struct {
	SourceFile    string
	StdImports    []string
	Codecs        []StructCodecData
	KeyVars       []KeyVarData
	TopicTypes    []TopicTypeData
	WasmFuncs     []WasmTopicFuncData
	TopicSnippets []string
	Body          string
	Helpers       []string
	// Multiplexed, when true, makes main() register the ClientSideState body via
	// GothicRegisterScope so ONE instance serves every placement of this route's
	// component. When false the generated main() is byte-identical to before.
	Multiplexed bool
	// JSONReaders / JSONDecoders drive the reflection-free Decode[T] codegen
	// (Phase 6): one _jsonRead_<T> per reachable struct type, one _jsonDecode_<T>
	// entry point per top-level Decode[T] type argument. Both are empty (and the
	// runtime JSON parser is not referenced) for a page that makes no Decode[T]
	// call — tree-shaking: no Decode, no parser cost.
	JSONReaders  []JSONReaderData
	JSONDecoders []JSONDecoderData
	// JSONWriters / JSONEncoders drive the reflection-free Encode[T] codegen
	// (Phase 7): one _jsonWrite_<T> per reachable struct type, one _jsonEncode_<T>
	// entry point per top-level Encode[T] type argument. JSONEncodeHelpers holds
	// the shared append/escape helper source, emitted only when JSONWriters is
	// non-empty. All three are empty for a page that makes no Encode[T] call —
	// tree-shaking: no Encode, no encoder cost.
	JSONWriters       []JSONWriterData
	JSONEncoders      []JSONEncoderData
	JSONEncodeHelpers string
}

// JSONFieldDecode is the generated decode statement for one struct field inside
// a _jsonRead_<Ident> reader (Phase 6, Decode[T]). It reads the field's JSON
// value out of the reader's `m map[string]any` into `out.<Field>`, coercing per
// D5.
type JSONFieldDecode struct {
	DecLine string
}

// JSONReaderData drives one generated
// `func _jsonRead_<Ident>(m map[string]any) <GoType>` reader. Ident is a
// collision-safe identifier fragment (bare `User` for a page-local type,
// `models_User` for a qualified cross-package one); GoType is the Go type as it
// must be written in the generated main (`User` when the type is inlined /
// dot-imported, `models.User` when it lives in an imported package).
type JSONReaderData struct {
	Ident  string
	GoType string
	Fields []JSONFieldDecode
}

// JSONDecoderData drives one generated
// `func _jsonDecode_<Ident>(r Response) (<GoType>, error)` entry point — the
// target that `Decode[T](resp)` is rewritten to.
type JSONDecoderData struct {
	Ident  string
	GoType string
}

// JSONFieldEncode is the generated serialization for one struct field inside a
// _jsonWrite_<Ident> writer (Phase 7, Encode[T]). KeyPrefixLit is a Go string
// literal appended verbatim — the JSON-quoted key plus ':' (and a leading ','
// for every field after the first) — and ValueLine appends the field's JSON
// value to the shared *[]byte buffer.
type JSONFieldEncode struct {
	KeyPrefixLit string
	ValueLine    string
}

// JSONWriterData drives one generated
// `func _jsonWrite_<Ident>(b *[]byte, v <GoType>)` writer.
type JSONWriterData struct {
	Ident  string
	GoType string
	Fields []JSONFieldEncode
}

// JSONEncoderData drives one generated
// `func _jsonEncode_<Ident>(v <GoType>) []byte` entry point — the target that
// `Encode[T](v)` is rewritten to.
type JSONEncoderData struct {
	Ident  string
	GoType string
}

// ManagerFieldData carries per-field information for the manager template.
// One entry per source-struct field, in declaration order.
type ManagerFieldData struct {
	FieldName   string // Go field name, e.g. "Pings"
	EncodeLines string // body of inline encode snippet referencing v.<FieldName>
	DecodeLines string // body of inline decode snippet referencing v.<FieldName>
	CaptureBody string // body of _capture<FieldName>(d *Decoder) []byte (from Phase 1)
}

// WasmTopicManagerMainData drives wasm_topic_manager_main.go.tmpl.
type WasmTopicManagerMainData struct {
	StructName    string
	KeyName       string
	HasTime       bool // true when any struct field has type time.Time
	Codecs        []StructCodecData
	TopicSnippets []string
	Fields        []ManagerFieldData // one entry per source struct field, in declaration order
	// Schema seam (Phase 15): content-hash id + Go-quoted descriptor literal of
	// this topic's wire shape. Threaded into the manager's registration via
	// GothicRegisterSchema; interpreted by nothing in v3.0.
	SchemaID            string
	SchemaDescriptorLit string
}

// structInfo / fieldInfo are the parsed representation of src/topics/*.go.

type structInfo struct {
	Name         string
	KeyName      string
	Compression  WasmCompression
	Compiler     WasmCompilerChoice
	Fields       []fieldInfo
	AccessorName string // var name from "var X = CreateTopic(...)", falls back to struct-derived name
}

type fieldInfo struct {
	Name      string
	Type      string
	TypeRef   typeRef // populated by parseStructsFromSource via typeRefFromExpr
	GothicTag string
	// JSONTag is the raw value of the field's `json:"..."` struct tag (empty when
	// absent). Populated for the Decode[T] JSON-codegen path (Phase 6) — the
	// reflection-free _jsonRead_<T> reader uses it to pick the JSON object key.
	JSONTag string
}
