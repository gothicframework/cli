package cli

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// fakeRunner records the commands InitializeModule issues and returns a
// canned result, so tests never invoke the real Go toolchain.
type fakeRunner struct {
	output []byte
	err    error
	// failOn lets a test fail a specific subcommand by matching args[0] or args[1]
	// (e.g. "init", "edit", "tidy" — all of which are "go mod <verb>").
	failOn string

	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if f.failOn != "" && ((len(args) > 0 && args[0] == f.failOn) || (len(args) > 1 && args[1] == f.failOn)) {
		return f.output, errors.New("boom")
	}
	return f.output, f.err
}

// withFakeRunner swaps the package-level cliRunner for the duration of t.
func withFakeRunner(t *testing.T, f *fakeRunner) {
	t.Helper()
	orig := cliRunner
	cliRunner = f
	t.Cleanup(func() { cliRunner = orig })
}

func TestNewCli(t *testing.T) {
	cli := NewCli()
	if cli.Runtime != runtime.GOOS {
		t.Errorf("Runtime = %q, want %q", cli.Runtime, runtime.GOOS)
	}
	if cli.Logger == nil {
		t.Error("Logger is nil")
	}
	if cli.config != nil {
		t.Error("config should be nil before GetConfig")
	}
}

// withStubParser swaps ConfigParser for the duration of t. pkg/cli cannot
// import pkg/helpers/astconfig (import cycle), so GetConfig tests inject a stub
// parser that mimics astconfig.Parse's behavior.
func withStubParser(t *testing.T, fn func(string) (*Config, error)) {
	t.Helper()
	orig := ConfigParser
	ConfigParser = fn
	t.Cleanup(func() { ConfigParser = orig })
}

func TestGetConfig(t *testing.T) {
	t.Run("parses gothic.config.go and applies overrides", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := os.WriteFile("gothic.config.go", []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		withStubParser(t, func(string) (*Config, error) {
			c := &Config{
				ProjectName:       "demo",
				GoModName:         "example.com/demo",
				TailwindBinary:    "/bin/tw",
				WasmBinary:        "/bin/wasm",
				WasmTinyGoVersion: "0.31.0",
			}
			c.OptimizeImages.LowResolutionRate = 10
			return c, nil
		})

		cli := NewCli()
		got, err := cli.GetConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ProjectName != "demo" {
			t.Errorf("ProjectName = %q", got.ProjectName)
		}
		if cli.Tailwind.ConfigOverride != "/bin/tw" {
			t.Errorf("Tailwind.ConfigOverride = %q", cli.Tailwind.ConfigOverride)
		}
		if cli.Wasm.Version != "0.31.0" {
			t.Errorf("Wasm.Version = %q", cli.Wasm.Version)
		}
		if cli.Wasm.ConfigOverride != "/bin/wasm" {
			t.Errorf("Wasm.ConfigOverride = %q", cli.Wasm.ConfigOverride)
		}

		// Second call returns the cached config without reparsing.
		if err := os.Remove("gothic.config.go"); err != nil {
			t.Fatal(err)
		}
		got2, err := cli.GetConfig()
		if err != nil {
			t.Fatalf("cached GetConfig errored: %v", err)
		}
		if got2.ProjectName != "demo" {
			t.Errorf("cached ProjectName = %q", got2.ProjectName)
		}
	})

	t.Run("minimal config leaves overrides untouched", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := os.WriteFile("gothic.config.go", []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		withStubParser(t, func(string) (*Config, error) {
			return &Config{ProjectName: "x", GoModName: "y"}, nil
		})
		cli := NewCli()
		if _, err := cli.GetConfig(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cli.Tailwind.ConfigOverride != "" {
			t.Errorf("Tailwind.ConfigOverride should stay empty, got %q", cli.Tailwind.ConfigOverride)
		}
	})

	t.Run("legacy gothic-config.json errors with migrate hint", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := os.WriteFile("gothic-config.json", []byte(`{"projectName":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		cli := GothicCli{}
		_, err := cli.GetConfig()
		if err == nil {
			t.Fatal("expected error for legacy gothic-config.json")
		}
		if !strings.Contains(err.Error(), "Found gothic-config.json (v2 format)") {
			t.Errorf("error = %q, want it to mention v2 format", err.Error())
		}
	})

	t.Run("no config file errors", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		cli := GothicCli{}
		_, err := cli.GetConfig()
		if err == nil {
			t.Fatal("expected error for missing config")
		}
		if !strings.Contains(err.Error(), "No config file found") {
			t.Errorf("error = %q, want it to mention no config file", err.Error())
		}
	})

	t.Run("parser error is propagated", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := os.WriteFile("gothic.config.go", []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		withStubParser(t, func(string) (*Config, error) {
			return nil, errors.New("boom parse")
		})
		cli := GothicCli{}
		if _, err := cli.GetConfig(); err == nil {
			t.Error("expected propagated parser error")
		}
	})
}

func TestInitializeModule(t *testing.T) {
	mods := []FrameworkModule{
		{Path: "github.com/gothicframework/core", Version: "v1.0.0"},
		{Path: "github.com/gothicframework/components", Version: "v1.0.0"},
		{Path: "github.com/gothicframework/middlewares", Version: "v1.0.0"},
	}

	t.Run("runs init, a per-module pin edit, and tidy with correct args", func(t *testing.T) {
		f := &fakeRunner{}
		withFakeRunner(t, f)

		cli := GothicCli{}
		if err := cli.InitializeModule("example.com/demo", mods); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// go mod init + one `go mod edit -require` per module + go mod tidy.
		if want := 2 + len(mods); len(f.calls) != want {
			t.Fatalf("expected %d commands, got %d: %v", want, len(f.calls), f.calls)
		}
		wantInit := []string{"go", "mod", "init", "example.com/demo"}
		if !equalArgs(f.calls[0], wantInit) {
			t.Errorf("init cmd = %v, want %v", f.calls[0], wantInit)
		}
		for i, m := range mods {
			want := []string{"go", "mod", "edit", "-require=" + m.Path + "@" + m.Version}
			if !equalArgs(f.calls[1+i], want) {
				t.Errorf("pin cmd[%d] = %v, want %v", i, f.calls[1+i], want)
			}
		}
		wantTidy := []string{"go", "mod", "tidy"}
		if last := f.calls[len(f.calls)-1]; !equalArgs(last, wantTidy) {
			t.Errorf("tidy cmd = %v, want %v", last, wantTidy)
		}
	})

	t.Run("no pins when module list empty", func(t *testing.T) {
		f := &fakeRunner{}
		withFakeRunner(t, f)
		cli := GothicCli{}
		if err := cli.InitializeModule("example.com/demo", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(f.calls) != 2 {
			t.Fatalf("expected 2 commands (init + tidy, no pins), got %d: %v", len(f.calls), f.calls)
		}
		if f.calls[1][1] != "mod" || f.calls[1][2] != "tidy" {
			t.Errorf("second cmd should be go mod tidy, got %v", f.calls[1])
		}
	})

	t.Run("init failure surfaces error", func(t *testing.T) {
		f := &fakeRunner{failOn: "init"}
		withFakeRunner(t, f)
		cli := GothicCli{}
		if err := cli.InitializeModule("example.com/demo", mods); err == nil {
			t.Fatal("expected error from go mod init")
		}
	})

	t.Run("pin failure surfaces error", func(t *testing.T) {
		f := &fakeRunner{failOn: "edit"}
		withFakeRunner(t, f)
		cli := GothicCli{}
		if err := cli.InitializeModule("example.com/demo", mods); err == nil {
			t.Fatal("expected error from go mod edit pin")
		}
	})

	t.Run("tidy failure is non-fatal warning", func(t *testing.T) {
		// go mod tidy failure is a warning (not fatal) so a project scaffolds
		// correctly — with its pinned require lines already written — even before
		// the framework versions are published to the registry.
		f := &fakeRunner{failOn: "tidy"}
		withFakeRunner(t, f)
		cli := GothicCli{}
		if err := cli.InitializeModule("example.com/demo", mods); err != nil {
			t.Fatalf("tidy failure should be a warning, not an error; got: %v", err)
		}
	})
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// chdir changes into dir and restores the original working directory on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
