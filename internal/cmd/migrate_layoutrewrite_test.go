package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempl(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "layout.templ")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// realV2Layout is the actual layout a v2 project ships (matches
// pkg/data/src/layouts/layout.templ at the v2 baseline): styles.css + htmx +
// hx-ext from unpkg, and NO gothic-core script (v2 injects the WASM bootstrap at
// runtime, server-side, before </body>).
const realV2Layout = `package layouts

templ PageLayout() {
	<!DOCTYPE html>
	<html lang="en" data-theme="dark">
		<head>
			<title>GOTHIC APP</title>
			<link rel="icon" type="image/x-icon" href="/public/favicon.ico"/>
			<link rel="shortcut icon" href="/public/favicon.ico"/>
			<meta charset="UTF-8"/>
			<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
			<link rel="stylesheet" href="/public/styles.css"/>
			<script src="https://unpkg.com/htmx.org@2.0.3" integrity="sha384-abc" crossorigin="anonymous"></script>
			<script defer src="https://unpkg.com/hx-ext-amz-content-sha256@1.0.12/min.js"></script>
		</head>
		<body class="bg-black" hx-ext="amz-content-sha256">
			{ children... }
		</body>
	</html>
}
`

func TestRewriteRealV2Layout(t *testing.T) {
	p := writeTempl(t, realV2Layout)
	ok, err := rewriteLayoutTemplV3(p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !ok {
		t.Fatal("expected the real v2 layout to be rewritten")
	}
	s := readTemplFile(t, p)
	for _, want := range []string{
		"@gothicComponents.Styles()",
		"@gothicComponents.RuntimeScripts()",
		`import gothicComponents "github.com/gothicframework/components"`,
		`href="/public/favicon.ico"`,          // preserved
		`hx-ext="amz-content-sha256"`,         // body attribute preserved
		`<title>GOTHIC APP</title>`,           // preserved
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q\n---\n%s", want, s)
		}
	}
	for _, bad := range []string{`<link rel="stylesheet"`, "unpkg.com/htmx", "hx-ext-amz-content-sha256@", "gothic-core"} {
		if strings.Contains(s, bad) {
			t.Errorf("should not still contain %q\n---\n%s", bad, s)
		}
	}
	// Styles must come before RuntimeScripts (styles.css preceded htmx in the source).
	if strings.Index(s, "@gothicComponents.Styles()") > strings.Index(s, "@gothicComponents.RuntimeScripts()") {
		t.Errorf("Styles() should be emitted before RuntimeScripts()\n---\n%s", s)
	}
}

func TestRewriteLayoutIsIdempotent(t *testing.T) {
	p := writeTempl(t, realV2Layout)
	if _, err := rewriteLayoutTemplV3(p); err != nil {
		t.Fatal(err)
	}
	after := readTemplFile(t, p)
	// Second pass must be a no-op (already migrated: no unpkg htmx, RuntimeScripts present).
	ok, err := rewriteLayoutTemplV3(p)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("second pass reported a rewrite; migration should be idempotent")
	}
	if readTemplFile(t, p) != after {
		t.Error("second pass modified an already-migrated layout")
	}
	if strings.Count(after, "@gothicComponents.RuntimeScripts()") != 1 {
		t.Errorf("RuntimeScripts() duplicated:\n%s", after)
	}
}

func TestRewriteLayoutExistingImportBlock(t *testing.T) {
	src := `package layouts

import (
	"demo/src/components"
)

templ X() {
	<head>
		<link rel="stylesheet" href="/public/styles.css"/>
		<script src="https://unpkg.com/htmx.org@2.0.3" crossorigin="anonymous"></script>
		<script defer src="https://unpkg.com/hx-ext-amz-content-sha256@1.0.12/min.js"></script>
	</head>
}
`
	p := writeTempl(t, src)
	ok, _ := rewriteLayoutTemplV3(p)
	if !ok {
		t.Fatal("expected rewrite")
	}
	s := readTemplFile(t, p)
	if !strings.Contains(s, `gothicComponents "github.com/gothicframework/components"`) {
		t.Errorf("components import not added into the block\n---\n%s", s)
	}
	if !strings.Contains(s, `"demo/src/components"`) {
		t.Errorf("existing import dropped\n---\n%s", s)
	}
}

func TestRewriteLayoutSkipsNonGothicTempl(t *testing.T) {
	// A page/component templ with no unpkg htmx must be left untouched — even if it
	// happens to reference styles.css.
	src := `package pages

templ Index() {
	<div>
		<link rel="stylesheet" href="/public/styles.css"/>
		hello
	</div>
}
`
	p := writeTempl(t, src)
	ok, _ := rewriteLayoutTemplV3(p)
	if ok {
		t.Fatal("a templ with no unpkg htmx must be left untouched")
	}
	if readTemplFile(t, p) != src {
		t.Error("file was modified but should not have been")
	}
}

func TestCleanOrphanedRuntimeAssets(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "public")
	os.MkdirAll(filepath.Join(pub, "wasm"), 0o755)
	for _, f := range []string{
		"gothic-core.js", "gothic-core-boot.js", "gothic-core-exec.js",
		"gothic-core.wasm", "wasm_exec.js", "wasm_exec_go.js", "styles.css", "favicon.ico",
	} {
		os.WriteFile(filepath.Join(pub, f), []byte("x"), 0o644)
	}
	removed := cleanOrphanedRuntimeAssets(pub)
	if len(removed) != 5 {
		t.Errorf("removed %d files, want 5: %v", len(removed), removed)
	}
	// Kept.
	for _, keep := range []string{"wasm_exec_go.js", "styles.css", "favicon.ico"} {
		if _, err := os.Stat(filepath.Join(pub, keep)); err != nil {
			t.Errorf("%s should have been kept: %v", keep, err)
		}
	}
	// Gone.
	for _, gone := range []string{"gothic-core.js", "wasm_exec.js"} {
		if _, err := os.Stat(filepath.Join(pub, gone)); err == nil {
			t.Errorf("%s should have been removed", gone)
		}
	}
}

func readTemplFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
