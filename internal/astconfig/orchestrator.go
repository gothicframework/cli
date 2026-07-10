package astconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	config "github.com/gothicframework/core/config"
)

// frameworkModulePath is the import path of the gothicframework module that the
// generated orchestrator program imports for its config types.
const frameworkModulePath = "github.com/gothicframework/core"

// frameworkRequireVersion is the placeholder version used in the generated
// orchestrator go.mod's require directive. It must carry a major version
// compatible with the module path: core is now suffixless (no /vN), so the Go
// tool requires v0 or v1 — v1.0.0 here. The actual source is supplied by the
// replace directive pointing at the on-disk framework root, so the patch level
// is irrelevant — only the major must be valid for the path.
const frameworkRequireVersion = "v1.0.0"

// packageClauseRE matches the leading `package <name>` clause of a Go file so we
// can rewrite the user's gothic.config.go (typically `package main`) into a
// dedicated package the orchestrator can import.
var packageClauseRE = regexp.MustCompile(`(?m)^package\s+\S+`)

// mainTemplate is the generated entry point. {{HOOK}} is replaced with the
// concrete hook name (BeforeDeploy / AfterDeploy) before writing to disk.
const mainTemplate = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	gothic "github.com/gothicframework/core/config"
	hook "gothicorchestrator/hook"
)

func main() {
	var gctx gothic.GothicContext
	raw := os.Getenv("GOTHIC_CONTEXT")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "GOTHIC_CONTEXT not set")
		os.Exit(1)
	}
	if err := json.Unmarshal([]byte(raw), &gctx); err != nil {
		fmt.Fprintln(os.Stderr, "GOTHIC_CONTEXT unmarshal:", err)
		os.Exit(1)
	}
	if err := hook.{{HOOK}}(context.Background(), &gctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.Marshal(gctx)
	fmt.Println(string(out))
}
`

// GenerateOrchestrator compiles and runs the user's lifecycle hook (hookName,
// e.g. "BeforeDeploy" or "AfterDeploy") from gothic.config.go in an isolated
// temporary module, passing the GothicContext as JSON over the GOTHIC_CONTEXT
// env var and reading back the (possibly mutated) context from stdout.
//
// The temporary module imports the user's config file as package
// "gothicorchestrator" and links it against the gothicframework module via a
// replace directive pointing at the framework module root on disk (resolved by
// findFrameworkRoot).
func GenerateOrchestrator(projectRoot, hookName string, gctx *config.GothicContext) (*config.GothicContext, error) {
	tempDir, err := os.MkdirTemp("", "gothic-orchestrator-*")
	if err != nil {
		return nil, fmt.Errorf("creating orchestrator temp dir: %w", err)
	}
	// Keep tempDir on error for debugging; only clean up on success.
	cleanup := func() { os.RemoveAll(tempDir) }

	// 1. Copy the user's config file, rewriting its package clause.
	configPath := filepath.Join(projectRoot, "gothic.config.go")
	src, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	// The config is placed in its own subpackage directory so it can be imported
	// by main.go. Go forbids two different package names (main + the config's
	// package) coexisting in a single directory, so they must be split.
	rewritten := packageClauseRE.ReplaceAll(src, []byte("package gothicorchestrator"))
	hookDir := filepath.Join(tempDir, "hook")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		return nil, fmt.Errorf("creating orchestrator hook dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "config.go"), rewritten, 0644); err != nil {
		return nil, fmt.Errorf("writing orchestrator config.go: %w", err)
	}

	// 2. Write the generated main.go.
	mainSrc := strings.ReplaceAll(mainTemplate, "{{HOOK}}", hookName)
	if err := os.WriteFile(filepath.Join(tempDir, "main.go"), []byte(mainSrc), 0644); err != nil {
		return nil, fmt.Errorf("writing orchestrator main.go: %w", err)
	}

	// 3. Write go.mod with a replace directive pointing at the framework root.
	frameworkRoot, err := findFrameworkRoot()
	if err != nil {
		return nil, err
	}
	// The require version must be compatible with the module's major-version
	// suffix: a /vN module path demands a vN.x.y version string, even though the
	// replace directive below makes the actual source come from disk. Using a
	// bare v0.0.0 here is rejected by the Go tool ("should be v3, not v0").
	goMod := fmt.Sprintf(
		"module gothicorchestrator\n\ngo %s\n\nrequire %s %s\n\nreplace %s => %s\n",
		goVersion(), frameworkModulePath, frameworkRequireVersion, frameworkModulePath, frameworkRoot,
	)
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(goMod), 0644); err != nil {
		return nil, fmt.Errorf("writing orchestrator go.mod: %w", err)
	}

	// 4. Marshal the inbound context.
	contextJSON, err := json.Marshal(gctx)
	if err != nil {
		return nil, fmt.Errorf("marshalling GothicContext: %w", err)
	}

	// 5. Resolve dependencies, then run the hook program.
	ctx := context.Background()
	tidy := exec.CommandContext(ctx, "go", "mod", "tidy")
	tidy.Dir = tempDir
	tidy.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return nil, fmt.Errorf("resolving orchestrator dependencies (go mod tidy): %w", err)
	}

	run := exec.CommandContext(ctx, "go", "run", ".")
	run.Dir = tempDir
	run.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTHIC_CONTEXT="+string(contextJSON))
	run.Stderr = os.Stderr
	out, err := run.Output()
	if err != nil {
		return nil, fmt.Errorf("running %s hook: %w", hookName, err)
	}

	// 6. Decode the (possibly mutated) context from stdout.
	var updated config.GothicContext
	out = []byte(strings.TrimSpace(string(out)))
	if err := json.Unmarshal(out, &updated); err != nil {
		return nil, fmt.Errorf("decoding hook output: %w", err)
	}

	cleanup()
	return &updated, nil
}

// findFrameworkRoot resolves the on-disk directory of the core runtime module
// (which provides core/config) so the orchestrator's go.mod can replace it.
//
// After the Part III core/cli split the `gothic` binary is built from the cli/v3
// module, NOT from core, so the running module (debug.BuildInfo.Main / `go env
// GOMOD`) is cli — the WRONG module. The runtime packages live in the separate
// core module. We therefore ask the toolchain where core resolves on disk,
// which is correct across a go.work workspace, a `replace` directive, and the
// module cache alike — for `gothic build` in a user project as well as dev/test
// runs. (`go list -m` resolves via use/replace even when core v3.0.0 is not
// yet published, whereas the build-graph loader would not.)
func findFrameworkRoot() (string, error) {
	// Primary: let the go toolchain report core's resolved directory in the
	// current module context (user project, workspace, or cache).
	out, listErr := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", frameworkModulePath).Output()
	if listErr == nil {
		dir := strings.TrimSpace(string(out))
		if dir != "" {
			if _, err := os.Stat(dir); err == nil {
				return dir, nil
			}
		}
	}

	// Fallback: locate core in the module cache from this binary's build info.
	// core appears as a dependency of the cli/v3 main module; follow an
	// absolute replace if one is recorded, else construct the cache path.
	if info, ok := debug.ReadBuildInfo(); ok {
		var dep *debug.Module
		if info.Main.Path == frameworkModulePath {
			dep = &info.Main
		} else {
			for i := range info.Deps {
				if info.Deps[i].Path == frameworkModulePath {
					dep = info.Deps[i]
					break
				}
			}
		}
		if dep != nil {
			if dep.Replace != nil && dep.Replace.Path != "" && filepath.IsAbs(dep.Replace.Path) {
				if _, err := os.Stat(dep.Replace.Path); err == nil {
					return dep.Replace.Path, nil
				}
			}
			if dep.Version != "" && dep.Version != "(devel)" {
				modCache := os.Getenv("GOMODCACHE")
				if modCache == "" {
					if mcOut, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
						modCache = strings.TrimSpace(string(mcOut))
					}
				}
				if modCache != "" {
					root := filepath.Join(modCache, frameworkModulePath+"@"+dep.Version)
					if _, err := os.Stat(root); err == nil {
						return root, nil
					}
				}
			}
		}
	}

	if listErr != nil {
		return "", fmt.Errorf("locating %s module root via `go list -m`: %w", frameworkModulePath, listErr)
	}
	return "", fmt.Errorf("could not locate the %s module on disk", frameworkModulePath)
}

// goVersion returns the Go toolchain version (e.g. "1.25.0") for the generated
// go.mod's `go` directive, derived from the runtime version string.
func goVersion() string {
	v := strings.TrimPrefix(runtime.Version(), "go")
	// runtime.Version may include suffixes (e.g. "1.25.0 X:..."); keep the head.
	if i := strings.IndexByte(v, ' '); i >= 0 {
		v = v[:i]
	}
	return v
}
