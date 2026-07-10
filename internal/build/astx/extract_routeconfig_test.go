package astx

import (
	"strings"
	"testing"
)

// TestExtractClientSideStateBody_Inline exercises the inline FuncLit path plus
// WasmCompression/WasmCompiler field extraction.
func TestExtractClientSideStateBody_Inline(t *testing.T) {
	entry := loadTestdata(t, "route_config_inline")

	res, found, err := ExtractClientSideStateBody(entry)
	if err != nil {
		t.Fatalf("ExtractClientSideStateBody: %v", err)
	}
	if !found {
		t.Fatalf("expected to find ClientSideState body")
	}
	if res.Body == nil {
		t.Fatalf("expected non-nil body")
	}
	if res.Compression != "BROTLI" {
		t.Errorf("Compression = %q, want BROTLI", res.Compression)
	}
	if res.Compiler != "tinygo" {
		t.Errorf("Compiler = %q, want tinygo", res.Compiler)
	}

	// Body should contain the inline statement `x := 1`.
	src, err := FormatNode(res.Body, entry.Pkg.Fset)
	if err != nil {
		t.Fatalf("FormatNode: %v", err)
	}
	if !strings.Contains(src, "x := 1") {
		t.Errorf("body missing inline stmt; got:\n%s", src)
	}
}

// TestExtractClientSideStateBody_Named exercises the named-ident path, which
// resolves through findFuncDeclForObj.
func TestExtractClientSideStateBody_Named(t *testing.T) {
	entry := loadTestdata(t, "route_config_named")

	res, found, err := ExtractClientSideStateBody(entry)
	if err != nil {
		t.Fatalf("ExtractClientSideStateBody: %v", err)
	}
	if !found {
		t.Fatalf("expected to find ClientSideState body")
	}
	if res.Body == nil {
		t.Fatalf("expected non-nil body resolved from named func")
	}
	if res.Compression != "GZIP" {
		t.Errorf("Compression = %q, want GZIP", res.Compression)
	}

	src, err := FormatNode(res.Body, entry.Pkg.Fset)
	if err != nil {
		t.Fatalf("FormatNode: %v", err)
	}
	if !strings.Contains(src, "fmt.Println") {
		t.Errorf("resolved body missing fmt.Println; got:\n%s", src)
	}
}

// TestExtractClientSideStateBody_Multiplexed verifies the additive Multiplexed
// field is read as true when the RouteConfig sets `Multiplexed: true`.
func TestExtractClientSideStateBody_Multiplexed(t *testing.T) {
	entry := loadTestdata(t, "route_config_multiplexed")

	res, found, err := ExtractClientSideStateBody(entry)
	if err != nil {
		t.Fatalf("ExtractClientSideStateBody: %v", err)
	}
	if !found {
		t.Fatalf("expected to find ClientSideState body")
	}
	if !res.Multiplexed {
		t.Errorf("Multiplexed = false, want true")
	}
}

// TestExtractClientSideStateBody_MultiplexedDefaultFalse verifies that a
// RouteConfig without a Multiplexed field yields Multiplexed=false (the
// non-multiplexed default), so existing routes keep their byte-identical path.
func TestExtractClientSideStateBody_MultiplexedDefaultFalse(t *testing.T) {
	entry := loadTestdata(t, "route_config_inline")

	res, found, err := ExtractClientSideStateBody(entry)
	if err != nil {
		t.Fatalf("ExtractClientSideStateBody: %v", err)
	}
	if !found {
		t.Fatalf("expected to find ClientSideState body")
	}
	if res.Multiplexed {
		t.Errorf("Multiplexed = true, want false (no field set)")
	}
}

// TestExtractClientSideStateBody_NonFunc exercises the error branch where the
// ClientSideState identifier resolves to a non-function object.
func TestExtractClientSideStateBody_NonFunc(t *testing.T) {
	entry := loadTestdata(t, "route_config_nonfunc")

	_, found, err := ExtractClientSideStateBody(entry)
	if err == nil {
		t.Fatalf("expected error for non-function ClientSideState, got found=%v", found)
	}
	if !strings.Contains(err.Error(), "non-function") {
		t.Errorf("expected 'non-function' error, got: %v", err)
	}
}

// TestExtractClientSideStateBody_NilGuards covers the early-return guards.
func TestExtractClientSideStateBody_NilGuards(t *testing.T) {
	_, found, err := ExtractClientSideStateBody(Entry{})
	if err != nil || found {
		t.Errorf("nil entry: want (false,nil), got (found=%v, err=%v)", found, err)
	}
}
