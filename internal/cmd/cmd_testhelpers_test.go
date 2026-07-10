package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cli "github.com/gothicframework/cli/v3/internal/cli"
)

// chdirTemp creates a fresh temp directory, chdir's into it for the duration of
// the test, and restores the original working dir on cleanup. Returns the temp
// dir path.
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

// writeConfig translates a legacy gothic-config.json document (the form the cmd
// tests were originally written against) into an equivalent gothic.config.go +
// go.mod pair in the current working dir, so the v3 AST-based GetConfig can read
// it. This keeps every existing call site working unchanged.
func writeConfig(t *testing.T, contents string) {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal([]byte(contents), &raw); err != nil {
		t.Fatalf("writeConfig: invalid JSON: %v", err)
	}

	str := func(m map[string]any, k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(m map[string]any, k string) (float64, bool) {
		v, ok := m[k].(float64)
		return v, ok
	}

	goMod := str(raw, "goModuleName")
	if goMod == "" {
		goMod = "demo"
	}
	writeGoMod(t, goMod)

	var b strings.Builder
	b.WriteString("package main\n\n")
	b.WriteString("import gothic \"github.com/gothicframework/core/config\"\n\n")
	b.WriteString("var Config = gothic.Config{\n")
	fmt.Fprintf(&b, "\tProjectName: %q,\n", str(raw, "projectName"))
	if v := str(raw, "tailwindBinary"); v != "" {
		fmt.Fprintf(&b, "\tTailwindBinary: %q,\n", v)
	}
	if v := str(raw, "wasmBinary"); v != "" {
		fmt.Fprintf(&b, "\tWasmBinary: %q,\n", v)
	}
	if v := str(raw, "tofuBinaryPath"); v != "" {
		fmt.Fprintf(&b, "\tTofuBinaryPath: %q,\n", v)
	}
	if oi, ok := raw["optimizeImages"].(map[string]any); ok {
		if r, ok := num(oi, "lowResolutionRate"); ok {
			b.WriteString("\tOptimizeImages: gothic.OptimizeImagesConfig{\n")
			fmt.Fprintf(&b, "\t\tLowResolutionRate: %d,\n", int(r))
			b.WriteString("\t},\n")
		}
	}
	if dep, ok := raw["deploy"].(map[string]any); ok {
		b.WriteString("\tDeploy: &gothic.DeployConfig{\n")
		if v, ok := num(dep, "serverMemory"); ok {
			fmt.Fprintf(&b, "\t\tServerMemory: %d,\n", int(v))
		}
		if v, ok := num(dep, "serverTimeout"); ok {
			fmt.Fprintf(&b, "\t\tServerTimeout: %d,\n", int(v))
		}
		if v := str(dep, "region"); v != "" {
			fmt.Fprintf(&b, "\t\tRegion: %q,\n", v)
		}
		if v := str(dep, "profile"); v != "" {
			fmt.Fprintf(&b, "\t\tProfile: %q,\n", v)
		}
		if v, ok := dep["customDomain"].(bool); ok {
			fmt.Fprintf(&b, "\t\tCustomDomain: %t,\n", v)
		}
		if stages, ok := dep["stages"].(map[string]any); ok {
			b.WriteString("\t\tStages: map[string]gothic.Stage{\n")
			for name, sv := range stages {
				stage, _ := sv.(map[string]any)
				fmt.Fprintf(&b, "\t\t\t%q: {\n", name)
				if v := str(stage, "customDomain"); v != "" {
					fmt.Fprintf(&b, "\t\t\t\tCustomDomain: %q,\n", v)
				}
				if v := str(stage, "hostedZoneId"); v != "" {
					fmt.Fprintf(&b, "\t\t\t\tHostedZoneId: %q,\n", v)
				}
				if v := str(stage, "certificateArn"); v != "" {
					fmt.Fprintf(&b, "\t\t\t\tCertificateArn: %q,\n", v)
				}
				if v := str(stage, "wafArn"); v != "" {
					fmt.Fprintf(&b, "\t\t\t\tWafArn: %q,\n", v)
				}
				if env, ok := stage["env"].(map[string]any); ok && len(env) > 0 {
					b.WriteString("\t\t\t\tENV: map[string]gothic.EnvValue{\n")
					for ek, ev := range env {
						fmt.Fprintf(&b, "\t\t\t\t\t%q: gothic.Env(%q),\n", ek, fmt.Sprint(ev))
					}
					b.WriteString("\t\t\t\t},\n")
				}
				b.WriteString("\t\t\t},\n")
			}
			b.WriteString("\t\t},\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")

	if err := os.WriteFile("gothic.config.go", []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write gothic.config.go: %v", err)
	}
}

// ensure cli import is referenced even if a future edit drops its only use.
var _ = cli.Config{}

// tidyModule runs `go mod tidy` in the current working dir. Needed for tests
// that drive code paths which type-check the project via packages.Load (e.g.
// the wasm scanner), so the temp module's transitive requirements for the
// replaced framework package are filled in.
func tidyModule(t *testing.T) {
	t.Helper()
	cmd := exec.Command("go", "mod", "tidy")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
}

// repoRoot returns the absolute path to the CORE module root (the directory
// whose go.mod declares github.com/gothicframework/core). The synthetic demo
// projects these tests build point their `replace core => ...` at this path,
// so it must resolve to core (which provides core/config, /router, /wasm),
// NOT cli.
//
// After the Part III core/cli split this test file sits at cli/internal/cmd/, so
// the nearest go.mod walking up is cli/go.mod — the wrong module. Instead we
// walk up to the workspace root (the dir holding go.work) and return its core/
// subdirectory. Fallback: if an ancestor's own go.mod already declares the core
// module (a published/standalone checkout of core), return that ancestor.
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
		// Workspace layout: <root>/go.work with a sibling core/ module.
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			core := filepath.Join(dir, "core")
			if isCoreModule(filepath.Join(core, "go.mod")) {
				return core
			}
		}
		// Standalone layout: this ancestor IS the core module.
		if isCoreModule(filepath.Join(dir, "go.mod")) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find core module root (go.mod declaring core)")
	return ""
}

// writeGoMod writes a go.mod so astx.NewLoader / packages.Load can run against
// the temp project. It adds a replace directive pointing at the real repo so the
// generated gothic.config.go's import of pkg/config resolves during package
// loading. With no page .go files, ScanPages returns zero pages without error.
func writeGoMod(t *testing.T, module string) {
	t.Helper()
	root := repoRoot(t)
	contents := "module " + module + "\n\ngo 1.23\n\n" +
		"require github.com/gothicframework/core v1.0.0\n\n" +
		"replace github.com/gothicframework/core => " + root + "\n"
	if err := os.WriteFile("go.mod", []byte(contents), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

// writeFakeTailwind writes an executable no-op script that any TailwindHelper
// pointed at it (via the tailwindBinary config override) will treat as the
// Tailwind CLI. It simply exits 0, so Build()/EnsureBinary() succeed without a
// real download or compile. Skips the test on Windows where shell scripts are
// not executable as-is. Returns the absolute binary path.
func writeFakeTailwind(t *testing.T, ok bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binary not supported on windows")
	}
	exit := "0"
	if !ok {
		exit = "1"
	}
	path := filepath.Join(t.TempDir(), "faketailwind")
	script := "#!/bin/sh\nexit " + exit + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tailwind: %v", err)
	}
	return path
}
