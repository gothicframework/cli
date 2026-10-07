package buildctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v4/internal/cli"

	// Registers the gothic.config.go parser on cli.ConfigParser.
	_ "github.com/gothicframework/cli/v4/internal/astconfig"
)

// ── fixtures ──────────────────────────────────────────────────────────────

// chdirTemp creates a fresh temp directory, chdir's into it for the duration
// of the test, and restores the original working dir on cleanup. Returns the
// temp dir path.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	return dir
}

// repoRoot returns the absolute path to the CORE module root (the directory
// whose go.mod declares github.com/gothicframework/core). The synthetic demo
// projects these tests build point their `replace core => ...` at this path.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	isCoreModule := func(gomod string) bool {
		data, err := os.ReadFile(gomod)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "module github.com/gothicframework/core")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			core := filepath.Join(dir, "core")
			if isCoreModule(filepath.Join(core, "go.mod")) {
				return core
			}
		}
		if isCoreModule(filepath.Join(dir, "go.mod")) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find core module root (go.mod declaring core)")
	return ""
}

// writeGoMod writes a go.mod so astx.NewLoader / packages.Load can run
// against the temp project.
func writeGoMod(t *testing.T, module string) {
	t.Helper()
	root := repoRoot(t)
	contents := "module " + module + "\n\ngo 1.23\n\n" +
		"require github.com/gothicframework/core v1.0.0\n\n" +
		"replace github.com/gothicframework/core => " + root + "\n"
	if err := os.WriteFile("go.mod", []byte(contents), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	_ = os.WriteFile("go.sum", []byte("github.com/test/fake v1.0.0 h1:abcdef=\n"), 0o644)
}

// scaffoldSrc creates the minimal src/ tree the file-based router walks.
func scaffoldSrc(t *testing.T) {
	t.Helper()
	for _, d := range []string{"src/pages", "src/components", "src/api", "src/routes"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// writeConfig writes a gothic.config.go the AST parser can read, with the
// binary overrides pointing at the given executables.
func writeConfig(t *testing.T, tailwindBin, wasmBin string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("package main\n\n")
	b.WriteString("import gothic \"github.com/gothicframework/core/config\"\n\n")
	b.WriteString("var Config = gothic.Config{\n")
	b.WriteString("\tProjectName: \"demo\",\n")
	if tailwindBin != "" {
		fmt.Fprintf(&b, "\tTailwindBinary: %q,\n", tailwindBin)
	}
	if wasmBin != "" {
		fmt.Fprintf(&b, "\tWasmBinary: %q,\n", wasmBin)
	}
	b.WriteString("}\n")
	if err := os.WriteFile("gothic.config.go", []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write gothic.config.go: %v", err)
	}
}

// writeFakeTailwind writes an executable no-op script that reports success
// and writes the CSS output file, exactly as the real Tailwind CLI does, so
// the css freshness gate sees the output it expects. Returns the binary path.
func writeFakeTailwind(t *testing.T, ok bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binary not supported on windows")
	}
	exit := "0"
	mkdir := "mkdir -p public && printf '<css>' > public/styles.css"
	if !ok {
		exit = "1"
		mkdir = ""
	}
	path := filepath.Join(t.TempDir(), "faketailwind")
	script := "#!/bin/sh\n" + mkdir + "\nexit " + exit + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tailwind: %v", err)
	}
	return path
}

// scaffoldPage writes a valid routed .templ page whose generated counterpart
// carries a ClientSideState.
func scaffoldPage(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join("src/pages", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// newTestController builds a Controller over a freshly constructed cli with
// the given options.
func newTestController(t *testing.T, opts Options) *Controller {
	t.Helper()
	cli := execRunnerCLI(t)
	return New(cli, opts)
}

// writeDigestJSON writes a digest document directly, for read-path tests.
func writeDigestJSON(t *testing.T, d DigestData) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(DigestPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(DigestPath, data, 0o644); err != nil {
		t.Fatalf("write digest: %v", err)
	}
}

// execRunnerCLI builds a GothicCli with the production build helpers;
// importing astconfig registers the config parser GetConfig needs.
func execRunnerCLI(t *testing.T) *gothic_cli.GothicCli {
	t.Helper()
	cli := gothic_cli.NewCli()
	return &cli
}
