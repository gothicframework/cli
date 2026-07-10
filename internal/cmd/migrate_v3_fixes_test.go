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
