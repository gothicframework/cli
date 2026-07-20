package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewriteMakefileV3 covers the v3.1.2 fix: the migrator must rename the CLI
// command `gothicframework` -> `gothic` in the makefile, WITHOUT touching a module
// path (…/felipegenef/gothicframework) that happens to appear in a comment.
func TestRewriteMakefileV3(t *testing.T) {
	dir := t.TempDir()
	mk := filepath.Join(dir, "makefile")
	const in = "# from github.com/felipegenef/gothicframework\n" +
		"deploy:\n\tgothicframework deploy --stage $(STAGE)\n" +
		"dev:\n\tgothicframework hot-reload\n"
	if err := os.WriteFile(mk, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := rewriteMakefileV3(mk)
	if err != nil {
		t.Fatalf("rewriteMakefileV3: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	got, _ := os.ReadFile(mk)
	s := string(got)
	if !strings.Contains(s, "\tgothic deploy --stage $(STAGE)") || !strings.Contains(s, "\tgothic hot-reload") {
		t.Errorf("CLI commands not rewritten to `gothic`:\n%s", s)
	}
	// The module path in the comment must be preserved (preceded by '/').
	if !strings.Contains(s, "github.com/felipegenef/gothicframework") {
		t.Errorf("module path in comment was wrongly rewritten:\n%s", s)
	}
	// No bare `gothicframework` command token should survive.
	if strings.Contains(s, "\tgothicframework ") {
		t.Errorf("a bare `gothicframework` command survived:\n%s", s)
	}

	// Idempotent: a second pass changes nothing.
	changed2, err := rewriteMakefileV3(mk)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if changed2 {
		t.Error("second pass should report changed=false")
	}
}

// TestRewriteV2ToV3GoModPinsFrameworkVersions is the regression guard for the exact
// bug that broke a real migration: the migrator must pin each runtime module at the
// SAME version `gothic init` uses (FrameworkModules), so a stale core (v1.0.0 vs the
// Provider-schema v1.1.0) can never ship again.
func TestRewriteV2ToV3GoModPinsFrameworkVersions(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	content := []byte("module example.com/demo\n\ngo 1.23\n\nrequire github.com/felipegenef/gothicframework/v2 v2.17.0\n")
	if err := os.WriteFile(goModPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteV2ToV3GoMod(goModPath, content); err != nil {
		t.Fatalf("rewriteV2ToV3GoMod: %v", err)
	}
	got, _ := os.ReadFile(goModPath)
	s := string(got)

	if len(FrameworkModules) == 0 {
		t.Fatal("FrameworkModules is empty")
	}
	for _, m := range FrameworkModules {
		want := m.Path + " " + m.Version
		if !strings.Contains(s, want) {
			t.Errorf("go.mod must pin %q (the version init uses), got:\n%s", want, s)
		}
	}
}

// TestRenameStaticFilesModeConsts covers the cloud-agnostic StaticFilesMode rename:
// migrate-v3 must rewrite the legacy package-qualified constant names to the current
// ones (ordinals unchanged: HOT_RELOAD_ONLY→CDN, ALL_ENVS→DISK), and must NOT touch a
// bare/unqualified identifier that merely shares the name.
func TestRenameStaticFilesModeConsts(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gothic.HOT_RELOAD_ONLY", "gothic.CDN"},
		{"gothic.ALL_ENVS", "gothic.DISK"},
		{"helpers.ALL_ENVS", "helpers.DISK"},
		{"config.HOT_RELOAD_ONLY", "config.CDN"},
		{"gothic.EMBEDDED", "gothic.EMBEDDED"}, // unchanged
		{"myHOT_RELOAD_ONLY", "myHOT_RELOAD_ONLY"}, // no leading '.', left alone
		{"ALL_ENVS", "ALL_ENVS"},                   // bare identifier, not package-qualified
	}
	for _, c := range cases {
		if got := renameStaticFilesModeConsts(c.in); got != c.want {
			t.Errorf("renameStaticFilesModeConsts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRewriteV2ToV3File_RenamesStaticFilesMode proves the project-wide file walk
// rewrites a legacy StaticFilesMode reference even when the file has no framework
// import line to rewrite (so the enum rename isn't gated on an import change).
func TestRewriteV2ToV3File_RenamesStaticFilesMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gothic.config.go")
	const in = "package main\n\nvar Config = gothic.Config{\n\tRuntime: gothic.RuntimeConfig{\n\t\tServeStaticFiles: gothic.ALL_ENVS,\n\t},\n}\n"
	if err := os.WriteFile(p, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := rewriteV2ToV3File(p)
	if err != nil {
		t.Fatalf("rewriteV2ToV3File: %v", err)
	}
	if !changed {
		t.Fatal("expected the file to be rewritten (ALL_ENVS → DISK)")
	}
	got, _ := os.ReadFile(p)
	if s := string(got); !strings.Contains(s, "gothic.DISK") || strings.Contains(s, "ALL_ENVS") {
		t.Errorf("expected gothic.ALL_ENVS → gothic.DISK, got:\n%s", s)
	}
}
