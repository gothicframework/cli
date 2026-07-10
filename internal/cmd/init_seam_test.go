package cmd

import (
	"os"
	"path/filepath"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	cli_data "github.com/gothicframework/cli/v3/internal/scaffold"
)

// newInitCommandForTest builds an InitCommand with the git seam stubbed so no
// real `git init` runs during CreateNewGothicApp tests.
func newInitCommandForTest(t *testing.T, gitCalls *[]string) InitCommand {
	t.Helper()
	cli := gothic_cli.NewCli()
	cmd := NewInitCommandCli(&cli, cli_data.DefaultCLIData)
	cmd.gitRunner = func(args ...string) error {
		if gitCalls != nil {
			*gitCalls = append(*gitCalls, args...)
		}
		return nil
	}
	return cmd
}

// TestCreateNewGothicAppInvalidProjectName covers the fallback branch: when the
// module path can't be reduced to a valid name (deriveProjectName fails) init
// prompts for a project name, and a non-kebab value there is rejected.
func TestCreateNewGothicAppInvalidProjectName(t *testing.T) {
	chdirTemp(t)
	cmd := newInitCommandForTest(t, nil)
	var err error
	// "___" sanitizes to empty -> deriveProjectName ok=false -> prompt fallback.
	withStdin(t, "Invalid_Name\n", func() {
		err = cmd.CreateNewGothicApp(cli_data.DefaultCLIData, "___")
	})
	if err == nil {
		t.Fatal("expected CreateNewGothicApp to fail on invalid fallback project name")
	}
}

// TestCreateNewGothicAppEmptyGoMod covers the early-return when no module arg is
// given and the go-module prompt is left empty.
func TestCreateNewGothicAppEmptyGoMod(t *testing.T) {
	chdirTemp(t)
	cmd := newInitCommandForTest(t, nil)
	var err error
	withStdin(t, "\n", func() {
		err = cmd.CreateNewGothicApp(cli_data.DefaultCLIData, "")
	})
	if err == nil {
		t.Fatal("expected CreateNewGothicApp to fail on empty go module name")
	}
}

// TestCreateNewGothicAppFailsAtTailwind drives CreateNewGothicApp fully
// non-interactively via the positional module arg (name derived, no prompts)
// and the full initializeProject scaffolding, then fails at
// Tailwind.EnsureBinary (bad override). This covers the arg handling, data
// wiring, and the entire initializeProject path without invoking the Go
// toolchain or git.
func TestCreateNewGothicAppFailsAtTailwind(t *testing.T) {
	dir := chdirTemp(t)

	cli := gothic_cli.NewCli()
	// Force the tailwind binary resolution to fail after scaffolding.
	cli.Tailwind.ConfigOverride = "/nonexistent/tailwind-binary"
	cmd := NewInitCommandCli(&cli, cli_data.DefaultCLIData)
	gitCalled := false
	cmd.gitRunner = func(args ...string) error { gitCalled = true; return nil }

	err := cmd.CreateNewGothicApp(cli_data.DefaultCLIData, "example.com/mymod")
	if err == nil {
		t.Fatal("expected CreateNewGothicApp to fail resolving tailwind binary")
	}
	if gitCalled {
		t.Error("git should not be invoked when init fails before completion")
	}
	// initializeProject ran fully, so scaffolding must exist on disk.
	if _, statErr := os.Stat(filepath.Join(dir, "main.go")); statErr != nil {
		t.Errorf("expected main.go scaffolded before the tailwind failure: %v", statErr)
	}
}

// TestDefaultGitRunnerInit confirms the default git seam runs `git init` in a
// temp dir without error (git is available in the test environment) and creates
// the .git directory, proving the default behavior matches the original
// exec.Command("git","init") call.
func TestDefaultGitRunnerInit(t *testing.T) {
	dir := chdirTemp(t)
	if err := defaultGitRunner("init"); err != nil {
		// Some CI images lack git; treat as skip rather than failure.
		t.Skipf("git not available: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("expected .git created by defaultGitRunner: %v", err)
	}
}
