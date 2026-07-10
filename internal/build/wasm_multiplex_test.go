package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	hh "github.com/gothicframework/core/render"
)

// renderPageMain renders wasm_page_main.go.tmpl with the given Multiplexed flag
// and a small ClientSideState body, returning the generated source.
func renderPageMain(t *testing.T, multiplexed bool) string {
	t.Helper()
	th := hh.NewTemplateHelper()
	out := filepath.Join(t.TempDir(), "main.go")
	data := WasmPageMainData{
		SourceFile:  "src/pages/index_templ.go",
		Body:        "\tcount := CreateObservable(0)\n\t_ = count\n",
		Multiplexed: multiplexed,
	}
	if err := th.UpdateFromTemplateFS(WasmTemplateFS, EmbeddedTmplWasmPageMain, out, data); err != nil {
		t.Fatalf("render page main: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered main: %v", err)
	}
	return string(b)
}

// TestPageMainMultiplexedWrapper verifies the registration wrapper is emitted
// ONLY when Multiplexed is true.
func TestPageMainMultiplexedWrapper(t *testing.T) {
	mux := renderPageMain(t, true)
	if !strings.Contains(mux, "GothicRegisterScope(func() {") {
		t.Errorf("multiplexed main missing GothicRegisterScope wrapper:\n%s", mux)
	}
	// The body must still be present inside the wrapper.
	if !strings.Contains(mux, "count := CreateObservable(0)") {
		t.Errorf("multiplexed main dropped the ClientSideState body:\n%s", mux)
	}

	non := renderPageMain(t, false)
	if strings.Contains(non, "GothicRegisterScope") {
		t.Errorf("non-multiplexed main must NOT contain GothicRegisterScope:\n%s", non)
	}
}

// TestPageMainNonMultiplexedByteLayout is a regression guard for the
// byte-identical invariant: the non-multiplexed main() body must sit directly
// between `func main() {` and the haltable keep-alive, exactly as before Phase 14.
func TestPageMainNonMultiplexedByteLayout(t *testing.T) {
	body := "\tcount := CreateObservable(0)\n\t_ = count\n"
	non := renderPageMain(t, false)
	// Old layout: "func main() {\n" + body + "\n\t// Haltable keep-alive:"
	want := "func main() {\n" + body + "\n\t// Haltable keep-alive:"
	if !strings.Contains(non, want) {
		t.Errorf("non-multiplexed main() layout changed; expected the body directly followed by the keep-alive comment.\nGot:\n%s", non)
	}
	// Both variants must keep the haltable keep-alive select.
	if !strings.Contains(non, "case <-GothicHaltChan():") {
		t.Errorf("non-multiplexed main() lost its haltable keep-alive")
	}
}
