package cmd

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	gothic_config "github.com/gothicframework/core/config"
)

func embeddedConfig(mode gothic_config.StaticFilesMode) *gothic_cli.Config {
	return &gothic_cli.Config{
		Runtime: gothic_cli.RuntimeConfig{ServeStaticFiles: mode},
	}
}

// TestSyncEmbeddedPublicFile_WhenEmbedded: an EMBEDDED config generates a valid
// gothic_embed_gen.go carrying //go:embed all:public and the SetEmbeddedPublicFS init
// call, and the generated file must parse as valid Go.
func TestSyncEmbeddedPublicFile_WhenEmbedded(t *testing.T) {
	chdirTemp(t)

	if err := syncEmbeddedPublicFile(embeddedConfig(gothic_config.EMBEDDED)); err != nil {
		t.Fatalf("syncEmbeddedPublicFile: %v", err)
	}

	data, err := os.ReadFile(gothicEmbedFileName)
	if err != nil {
		t.Fatalf("expected %s to be generated: %v", gothicEmbedFileName, err)
	}
	content := string(data)
	if !strings.Contains(content, "//go:embed all:public") {
		t.Errorf("generated file missing //go:embed all:public directive; got:\n%s", content)
	}
	if !strings.Contains(content, "middlewares.SetEmbeddedPublicFS(sub)") {
		t.Errorf("generated file missing SetEmbeddedPublicFS init call; got:\n%s", content)
	}

	// The generated file must be syntactically valid Go.
	if _, err := parser.ParseFile(token.NewFileSet(), gothicEmbedFileName, data, parser.ParseComments); err != nil {
		t.Fatalf("generated %s does not parse as Go: %v", gothicEmbedFileName, err)
	}

	// public/.gitkeep must exist so //go:embed all:public compiles.
	if _, err := os.Stat(filepath.Join("public", ".gitkeep")); err != nil {
		t.Errorf("expected public/.gitkeep to exist: %v", err)
	}
}

// TestSyncEmbeddedPublicFile_DeletedWhenNotEmbedded: a pre-existing embed file is
// removed when the mode is not EMBEDDED.
func TestSyncEmbeddedPublicFile_DeletedWhenNotEmbedded(t *testing.T) {
	chdirTemp(t)

	if err := os.WriteFile(gothicEmbedFileName, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("seed embed file: %v", err)
	}

	if err := syncEmbeddedPublicFile(embeddedConfig(gothic_config.CDN)); err != nil {
		t.Fatalf("syncEmbeddedPublicFile: %v", err)
	}

	if _, err := os.Stat(gothicEmbedFileName); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", gothicEmbedFileName, err)
	}
}

// TestSyncEmbeddedPublicFile_Idempotent: running twice under EMBEDDED yields
// byte-identical output and no error.
func TestSyncEmbeddedPublicFile_Idempotent(t *testing.T) {
	chdirTemp(t)
	cfg := embeddedConfig(gothic_config.EMBEDDED)

	if err := syncEmbeddedPublicFile(cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, err := os.ReadFile(gothicEmbedFileName)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}

	if err := syncEmbeddedPublicFile(cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, err := os.ReadFile(gothicEmbedFileName)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("embed file not byte-stable across runs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestSyncEmbeddedPublicFile_MissingPublicDir: with no ./public present, EMBEDDED
// mode creates the directory and its .gitkeep sentinel.
func TestSyncEmbeddedPublicFile_MissingPublicDir(t *testing.T) {
	chdirTemp(t)

	if _, err := os.Stat("public"); !os.IsNotExist(err) {
		t.Fatalf("precondition: public/ should not exist, stat err = %v", err)
	}

	if err := syncEmbeddedPublicFile(embeddedConfig(gothic_config.EMBEDDED)); err != nil {
		t.Fatalf("syncEmbeddedPublicFile: %v", err)
	}

	info, err := os.Stat("public")
	if err != nil || !info.IsDir() {
		t.Fatalf("expected public/ directory to be created: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join("public", ".gitkeep")); err != nil {
		t.Errorf("expected public/.gitkeep to be created: %v", err)
	}
}

// TestSyncEmbeddedPublicFile_RemovesLegacyFile covers the rename upgrade path: the
// pre-rename gothic_embed.go must be deleted on sync (in either mode) so a project
// never ends up with two //go:embed all:public files fighting over ./public.
func TestSyncEmbeddedPublicFile_RemovesLegacyFile(t *testing.T) {
	for _, mode := range []gothic_config.StaticFilesMode{gothic_config.EMBEDDED, gothic_config.CDN} {
		chdirTemp(t)
		if err := os.WriteFile(legacyEmbedFileName, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syncEmbeddedPublicFile(embeddedConfig(mode)); err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
		if _, err := os.Stat(legacyEmbedFileName); !os.IsNotExist(err) {
			t.Errorf("mode %v: legacy %s should have been removed (err=%v)", mode, legacyEmbedFileName, err)
		}
	}
}
