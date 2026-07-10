package cmd

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// seedTopicFixture writes a v2-style topic definition + a page that mounts it
// into an existing project dir. It exercises every mount-name resolution path
// AND both ComponentFnName literal styles:
//   - CounterState — multiline literal, ComponentFnName explicit ("MountCounterTopic")
//   - ThemeState   — multiline literal, only SubscriberFnName → mount "AddThemeTopic"
//   - InlineState  — SINGLE-LINE literal with ComponentFnName ("MountInline") — the
//     regex-at-line-start approach would MISS this; the AST scope must catch it
//   - Foo          — declaring var name with NO SubscriberFnName/ComponentFnName →
//     accessor falls back to the var name → mount "AddFoo"
//
// An unrelated struct carries its own bare `ComponentFnName:` field that must NOT
// be touched (it is not inside a TopicConfig passed to CreateTopic). The page also
// mounts an unrelated @Sidebar() that must survive.
func seedTopicFixture(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("src/topics/topics.go", `package gothicwasm

import . "github.com/felipegenef/gothicframework/v2/pkg/wasm"

// Unrelated struct — its ComponentFnName field must survive (not a TopicConfig).
type Widget struct {
	ComponentFnName string
}

var _ = Widget{ComponentFnName: "keep-me"}

type CounterState struct {
	Count int
}

type ThemeState struct {
	Dark bool
}

type InlineState struct {
	On bool
}

type FooState struct {
	N int
}

var _ = CreateTopic(CounterState{}, TopicConfig{
	Name:             "counter",
	Compression:      BROTLI,
	SubscriberFnName: "GetCounterTopic",
	ComponentFnName:  "MountCounterTopic",
})

var _ = CreateTopic(ThemeState{}, TopicConfig{
	Name:             "theme",
	SubscriberFnName: "ThemeTopic",
})

var _ = CreateTopic(InlineState{}, TopicConfig{Name: "inline", ComponentFnName: "MountInline"})

var Foo = CreateTopic(FooState{}, TopicConfig{Name: "foo"})
`)

	write("src/pages/counter.templ", `package pages

templ Counter() {
	@MountCounterTopic()
	@AddThemeTopic()
	@MountInline()
	@AddFoo()
	@Sidebar()
	<div>content</div>
}
`)
}

// setupV2Project writes a minimal v2 Gothic project into a fresh temp dir and
// returns its path. It includes gothic-config.json, a v2 go.mod, SAM artifacts,
// and a main.go importing gothicframework/v2.
func setupV2Project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("gothic-config.json", `{
		"projectName":"demo",
		"goModuleName":"example.com/demo",
		"deploy":{"region":"us-east-1","profile":"default","stages":{"dev":{}}}
	}`)
	write("go.mod", "module example.com/demo\n\ngo 1.23\n\nrequire github.com/felipegenef/gothicframework/v2 v2.17.0\n")
	write("template.yaml", "Resources: {}\n")
	write("samconfig.toml", "version = 0.1\n")
	write("Dockerfile", "FROM scratch\n")
	write("main.go", "package main\n\nimport _ \"github.com/felipegenef/gothicframework/v2/pkg/cli\"\n\nfunc main() {}\n")
	return dir
}

// runMigrateV3In invokes runMigrateV3 against dir with the given dry-run flag,
// capturing stdout. It restores the package-level flag vars afterward.
func runMigrateV3In(t *testing.T, dir string, dryRun bool) (string, error) {
	t.Helper()
	origPath, origDry := migrateV3Path, migrateV3DryRun
	t.Cleanup(func() { migrateV3Path, migrateV3DryRun = origPath, origDry })
	migrateV3Path = dir
	migrateV3DryRun = dryRun

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runMigrateV3(cmd, nil)
	return out.String(), err
}

// TestMapFrameworkImportSubpaths pins the full user-facing v2→v3 import remap
// table. v3 split the monorepo into two org modules (core + components) with a
// different folder layout, so this is a per-subpath remap, NOT a naïve prefix
// swap. Every legacy felipegenef import a real project can hold must land on the
// exact new org path, must never leave a dead `core/pkg/...` fragment, and
// must never still contain `felipegenef`.
func TestMapFrameworkImportSubpaths(t *testing.T) {
	const legacy = "github.com/felipegenef/gothicframework/v2"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"config", legacy + "/pkg/config", "github.com/gothicframework/core/config"},
		{"routes->router", legacy + "/pkg/helpers/routes", "github.com/gothicframework/core/router"},
		{"wasm", legacy + "/pkg/wasm", "github.com/gothicframework/core/wasm"},
		{"runtimeassets", legacy + "/pkg/helpers/runtimeassets", "github.com/gothicframework/core/runtimeassets"},
		{"gothiccore", legacy + "/pkg/helpers/gothiccore", "github.com/gothicframework/core/gothiccore"},
		{"corewasm", legacy + "/pkg/helpers/corewasm", "github.com/gothicframework/core/corewasm"},
		{"wasm_exec->wasmexec", legacy + "/pkg/data/wasm_exec", "github.com/gothicframework/core/wasmexec"},
		{"server->components/server", legacy + "/pkg/server", "github.com/gothicframework/middlewares"},
		{"components", legacy + "/components", "github.com/gothicframework/components"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapFrameworkImport(tc.in)
			if got != tc.want {
				t.Errorf("mapFrameworkImport(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// A subpath-aware remap must never produce a dead naïve-prefix path...
			if strings.Contains(got, "core/pkg/") {
				t.Errorf("output %q contains a dead naïve-prefix-swap fragment core/pkg/", got)
			}
			// ...and must never still reference the legacy org.
			if strings.Contains(got, "felipegenef") {
				t.Errorf("output %q still references the legacy felipegenef org", got)
			}
		})
	}
}

// TestRewriteV2ToV3GoModSplitsModules verifies the go.mod rewrite drops the
// single legacy felipegenef framework require and adds all three new org modules
// (core + components + middlewares), since a v3 project may import any. go mod tidy
// later prunes whichever is unused, but the rewrite itself must offer all three.
func TestRewriteV2ToV3GoModSplitsModules(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	content := []byte("module example.com/demo\n\ngo 1.23\n\nrequire github.com/felipegenef/gothicframework/v2 v2.17.0\n")
	if err := os.WriteFile(goModPath, content, 0o644); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}

	if err := rewriteV2ToV3GoMod(goModPath, content); err != nil {
		t.Fatalf("rewriteV2ToV3GoMod: %v", err)
	}

	out, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read rewritten go.mod: %v", err)
	}
	got := string(out)

	for _, want := range []string{
		"github.com/gothicframework/core",
		"github.com/gothicframework/components",
		"github.com/gothicframework/middlewares",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten go.mod missing require %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "felipegenef/gothicframework") {
		t.Errorf("rewritten go.mod must drop the legacy felipegenef require:\n%s", got)
	}
}

func TestMigrateTofuFailsWhenJSONAbsent(t *testing.T) {
	dir := t.TempDir()
	_, err := runMigrateV3In(t, dir, false)
	if err == nil {
		t.Fatal("expected error when gothic-config.json is absent")
	}
	if !strings.Contains(err.Error(), "not a v2 project") {
		t.Errorf("error = %q, want it to mention 'not a v2 project'", err.Error())
	}
}

func TestMigrateTofuDryRunWritesNothing(t *testing.T) {
	dir := setupV2Project(t)
	out, err := runMigrateV3In(t, dir, true)
	if err != nil {
		t.Fatalf("dry-run migrate: %v", err)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("expected dry-run output, got: %q", out)
	}
	// No gothic.config.go, no .bak, SAM files untouched.
	if _, err := os.Stat(filepath.Join(dir, "gothic.config.go")); err == nil {
		t.Error("gothic.config.go should NOT be created in dry-run")
	}
	if _, err := os.Stat(filepath.Join(dir, "gothic-config.json.bak")); err == nil {
		t.Error(".bak should NOT be created in dry-run")
	}
	if _, err := os.Stat(filepath.Join(dir, "template.yaml")); err != nil {
		t.Error("template.yaml should still exist after dry-run")
	}
}

func TestMigrateTofuHappyPath(t *testing.T) {
	dir := setupV2Project(t)
	out, err := runMigrateV3In(t, dir, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "gothic.config.go")); err != nil {
		t.Error("gothic.config.go was not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "gothic-config.json.bak")); err != nil {
		t.Error("gothic-config.json.bak was not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "gothic-config.json")); err == nil {
		t.Error("gothic-config.json should have been renamed to .bak")
	}
	for _, f := range []string{"template.yaml", "samconfig.toml", "Dockerfile"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("SAM artifact %s should have been removed", f)
		}
	}
	// main.go import rewritten /v2 → /v3.
	mainBytes, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if bytes.Contains(mainBytes, []byte("felipegenef/gothicframework")) {
		t.Error("main.go still references the legacy felipegenef framework path")
	}
	if !bytes.Contains(mainBytes, []byte("gothicframework/core")) {
		t.Error("main.go was not rewritten to the new gothicframework/core org path")
	}
	// Playbook printed.
	if !strings.Contains(out, "Manual import playbook") {
		t.Error("import playbook was not printed")
	}

	// The remap must be subpath-aware, never a naïve prefix swap that would leave a
	// dead `core/pkg/...` path, and re-running must not double-map.
	for _, f := range []string{"main.go", "gothic.config.go"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if bytes.Contains(b, []byte("core/pkg/")) {
			t.Errorf("%s has a dead naïve-prefix-swap import path (core/pkg/...)", f)
		}
		if bytes.Contains(b, []byte("gothicframework/v3/v3")) {
			t.Errorf("%s has a doubled /v3/v3 import path", f)
		}
	}
}

func TestMigrateV3StripsTopicMounts(t *testing.T) {
	dir := setupV2Project(t)
	seedTopicFixture(t, dir)

	out, err := runMigrateV3In(t, dir, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// (1) Every ComponentFnName inside a TopicConfig must be stripped — including
	// the single-line (inline) literal that a line-start regex would miss.
	topicBytes, err := os.ReadFile(filepath.Join(dir, "src/topics/topics.go"))
	if err != nil {
		t.Fatalf("read topics.go: %v", err)
	}
	topicSrc := string(topicBytes)
	if strings.Contains(topicSrc, `ComponentFnName:  "MountCounterTopic"`) {
		t.Errorf("multiline ComponentFnName should have been stripped:\n%s", topicSrc)
	}
	if strings.Contains(topicSrc, `ComponentFnName: "MountInline"`) {
		t.Errorf("inline ComponentFnName should have been stripped:\n%s", topicSrc)
	}
	// The stripped source must still be valid Go.
	if _, perr := parser.ParseFile(token.NewFileSet(), "topics.go", topicBytes, 0); perr != nil {
		t.Errorf("stripped topics.go no longer parses: %v\n%s", perr, topicSrc)
	}
	// Surviving fields: SubscriberFnName, both topic names, and the inline literal's
	// own Name.
	for _, keep := range []string{
		`SubscriberFnName: "GetCounterTopic"`,
		`Name: "inline"`,
		`Name: "foo"`,
	} {
		if !strings.Contains(topicSrc, keep) {
			t.Errorf("expected %q to survive stripping:\n%s", keep, topicSrc)
		}
	}
	// The unrelated Widget struct's ComponentFnName field must NOT be touched.
	if !strings.Contains(topicSrc, `type Widget struct {`) || !strings.Contains(topicSrc, "ComponentFnName string") {
		t.Errorf("unrelated Widget.ComponentFnName field must be preserved:\n%s", topicSrc)
	}
	if !strings.Contains(topicSrc, `Widget{ComponentFnName: "keep-me"}`) {
		t.Errorf("unrelated Widget literal must be preserved (only TopicConfig fields are stripped):\n%s", topicSrc)
	}

	// (2) All four mount calls must be gone; the unrelated component + content must
	// remain.
	tmplBytes, err := os.ReadFile(filepath.Join(dir, "src/pages/counter.templ"))
	if err != nil {
		t.Fatalf("read counter.templ: %v", err)
	}
	tmpl := string(tmplBytes)
	for _, gone := range []string{"@MountCounterTopic()", "@AddThemeTopic()", "@MountInline()", "@AddFoo()"} {
		if strings.Contains(tmpl, gone) {
			t.Errorf("mount call %q should have been removed:\n%s", gone, tmpl)
		}
	}
	if !strings.Contains(tmpl, "@Sidebar()") {
		t.Errorf("unrelated component @Sidebar() must NOT be removed:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, "<div>content</div>") {
		t.Errorf("page content must be preserved:\n%s", tmpl)
	}

	// (3) The migration summary must report both cleanups (with the review caveat)
	// + the breaking note.
	if !strings.Contains(out, "Removing ComponentFnName field") {
		t.Errorf("summary missing ComponentFnName removal line:\n%s", out)
	}
	if !strings.Contains(out, "topic-mount call(s)") {
		t.Errorf("summary missing mount-call removal line:\n%s", out)
	}
	if !strings.Contains(out, "review the diff") {
		t.Errorf("summary missing the same-name-collision review caveat:\n%s", out)
	}
	if !strings.Contains(out, "BREAKING CHANGE — topics") {
		t.Errorf("playbook missing the breaking topic-mount note:\n%s", out)
	}
}

// TestCleanTopicMountsIdempotent runs cleanTopicMounts twice on the same tree: the
// second pass must find nothing to change (no output, byte-identical files, no
// error), proving the transform converges.
func TestCleanTopicMountsIdempotent(t *testing.T) {
	dir := setupV2Project(t)
	seedTopicFixture(t, dir)

	var first bytes.Buffer
	if err := cleanTopicMounts(dir, false, &first); err != nil {
		t.Fatalf("first cleanTopicMounts: %v", err)
	}
	if !strings.Contains(first.String(), "Removing ComponentFnName field") {
		t.Fatalf("first pass should have changed something:\n%s", first.String())
	}

	topicPath := filepath.Join(dir, "src/topics/topics.go")
	tmplPath := filepath.Join(dir, "src/pages/counter.templ")
	afterFirstTopic, _ := os.ReadFile(topicPath)
	afterFirstTmpl, _ := os.ReadFile(tmplPath)

	var second bytes.Buffer
	if err := cleanTopicMounts(dir, false, &second); err != nil {
		t.Fatalf("second cleanTopicMounts: %v", err)
	}
	if second.Len() != 0 {
		t.Errorf("second pass should be a no-op, but printed:\n%s", second.String())
	}
	nowTopic, _ := os.ReadFile(topicPath)
	nowTmpl, _ := os.ReadFile(tmplPath)
	if !bytes.Equal(afterFirstTopic, nowTopic) {
		t.Error("second pass mutated topics.go — not idempotent")
	}
	if !bytes.Equal(afterFirstTmpl, nowTmpl) {
		t.Error("second pass mutated counter.templ — not idempotent")
	}
}

func TestMigrateV3TopicMountsDryRunWritesNothing(t *testing.T) {
	dir := setupV2Project(t)
	seedTopicFixture(t, dir)

	topicPath := filepath.Join(dir, "src/topics/topics.go")
	tmplPath := filepath.Join(dir, "src/pages/counter.templ")
	origTopic, _ := os.ReadFile(topicPath)
	origTmpl, _ := os.ReadFile(tmplPath)

	out, err := runMigrateV3In(t, dir, true)
	if err != nil {
		t.Fatalf("dry-run migrate: %v", err)
	}

	// Dry-run announces the changes...
	if !strings.Contains(out, "[dry-run] Would remove ComponentFnName field") {
		t.Errorf("dry-run should announce ComponentFnName removal:\n%s", out)
	}
	if !strings.Contains(out, "[dry-run] Would remove") || !strings.Contains(out, "topic-mount call(s)") {
		t.Errorf("dry-run should announce mount-call removal:\n%s", out)
	}
	// ...but touches nothing on disk.
	nowTopic, _ := os.ReadFile(topicPath)
	nowTmpl, _ := os.ReadFile(tmplPath)
	if !bytes.Equal(origTopic, nowTopic) {
		t.Error("dry-run must not modify src/topics/topics.go")
	}
	if !bytes.Equal(origTmpl, nowTmpl) {
		t.Error("dry-run must not modify counter.templ")
	}
}

func TestMigrateTofuIdempotent(t *testing.T) {
	dir := setupV2Project(t)
	if _, err := runMigrateV3In(t, dir, false); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Second run: gothic-config.json is gone (renamed to .bak), so the pre-flight
	// must fail cleanly rather than producing a .bak.bak.
	_, err := runMigrateV3In(t, dir, false)
	if err == nil {
		t.Fatal("expected second run to fail pre-flight (no gothic-config.json)")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "gothic-config.json.bak.bak")); statErr == nil {
		t.Error("a .bak.bak file should never be created")
	}
}
