package tofu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cli "github.com/gothicframework/cli/v3/internal/cli"
)

// writeExec writes an executable file (mode 0755) and returns its path.
func writeExec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	return p
}

func TestEnsureBinaryOverrideUsedWhenValid(t *testing.T) {
	dir := t.TempDir()
	bin := writeExec(t, dir, "tofu")

	m := &tofudlManager{config: &cli.Config{TofuBinaryPath: bin}}
	got, err := m.EnsureBinary(context.Background())
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != bin {
		t.Errorf("path = %q, want %q", got, bin)
	}
}

func TestEnsureBinaryOverrideErrorsWhenMissing(t *testing.T) {
	m := &tofudlManager{config: &cli.Config{TofuBinaryPath: filepath.Join(t.TempDir(), "nope")}}
	_, err := m.EnsureBinary(context.Background())
	if err == nil {
		t.Fatal("expected error for missing override path")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("error = %q, want it to mention accessibility", err.Error())
	}
}

func TestEnsureBinaryOverrideErrorsWhenNotExecutable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tofu")
	if err := os.WriteFile(p, []byte("not exec"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := &tofudlManager{config: &cli.Config{TofuBinaryPath: p}}
	_, err := m.EnsureBinary(context.Background())
	if err == nil {
		t.Fatal("expected error for non-executable override")
	}
	if !strings.Contains(err.Error(), "executable") {
		t.Errorf("error = %q, want it to mention the executable bit", err.Error())
	}
}

func TestEnsureBinaryOverrideErrorsWhenDir(t *testing.T) {
	dir := t.TempDir()
	m := &tofudlManager{config: &cli.Config{TofuBinaryPath: dir}}
	_, err := m.EnsureBinary(context.Background())
	if err == nil {
		t.Fatal("expected error when override is a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %q, want it to mention directory", err.Error())
	}
}

func TestEnsureBinaryCacheHitShortCircuitsDownload(t *testing.T) {
	// Work in a temp project dir so the relative cachePath resolves under it.
	dir := t.TempDir()
	t.Chdir(dir)

	cacheDir := filepath.Join(dir, ".gothicCli", "bin")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "tofu"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write cache binary: %v", err)
	}

	// Empty config (no override) so resolution falls through to the cache tier.
	// If this hit the download tier it would attempt a network fetch and fail;
	// returning cachePath without error proves the cache short-circuited.
	m := &tofudlManager{config: &cli.Config{}}
	got, err := m.EnsureBinary(context.Background())
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != cachePath {
		t.Errorf("path = %q, want cache path %q", got, cachePath)
	}
}

func TestEnsureBinaryCacheMissNotExecutableFallsThrough(t *testing.T) {
	// A cached file lacking the executable bit must NOT be treated as a cache
	// hit. We can't assert the download succeeds (no network), but we can assert
	// the function does not return the non-executable cache path as a hit.
	dir := t.TempDir()
	t.Chdir(dir)
	cacheDir := filepath.Join(dir, ".gothicCli", "bin")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "tofu"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := &tofudlManager{config: &cli.Config{}}
	got, err := m.EnsureBinary(context.Background())
	// It will attempt a download. In a sandbox without network this errors.
	// Either way, it must not return the non-exec cache path with nil error.
	if err == nil && got == cachePath {
		// download tier may legitimately have re-created an executable cache;
		// confirm it is now executable to distinguish from the bad cache hit.
		info, statErr := os.Stat(got)
		if statErr != nil || !isExecutable(info) {
			t.Fatalf("returned non-executable cache path as a hit: %q", got)
		}
	}
}
