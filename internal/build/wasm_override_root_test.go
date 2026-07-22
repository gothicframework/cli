package helpers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeTinygo writes an executable shell script at dir/name that answers
// `env TINYGOROOT` with rootReply (empty rootReply → exit non-zero, to exercise
// the grandparent-dir fallback). Returns its path.
func writeFakeTinygo(t *testing.T, dir, rootReply string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake tinygo not portable to windows")
	}
	path := filepath.Join(dir, "tinygo")
	var script string
	if rootReply == "" {
		script = "#!/bin/sh\nexit 1\n"
	} else {
		script = "#!/bin/sh\nif [ \"$1\" = env ]; then echo " + rootReply + "; fi\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tinygo: %v", err)
	}
	return path
}

// When an override binary is active, the env handed to the tinygo exec.Command
// must carry the OVERRIDE's TINYGOROOT (reported by `tinygo env TINYGOROOT`),
// never the managed 0.41.1 root — otherwise the override's codegen is pointed at
// a runtime source tree it doesn't match and the build panics.
func TestEnvironWithWarn_ConfigOverride_UsesOverrideRoot(t *testing.T) {
	t.Setenv("GOTHIC_CLI_CACHE_DIR", t.TempDir())
	h := linuxAmd64Helper()

	// bin/ holds the override binary; its baked-in root is the parent dir.
	overrideRoot := t.TempDir()
	binDir := filepath.Join(overrideRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.ConfigOverride = writeFakeTinygo(t, binDir, overrideRoot)

	// EnsureBinary precomputes overrideRoot on the single build thread.
	if err := h.EnsureBinary(); err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if h.overrideRoot != overrideRoot {
		t.Fatalf("overrideRoot cache: got %q, want %q", h.overrideRoot, overrideRoot)
	}

	env := h.EnvironWithWarn(nil)
	got := tinygoRootFromEnv(t, env)
	if got != overrideRoot {
		t.Errorf("TINYGOROOT: got %q, want override root %q", got, overrideRoot)
	}
	if managed := h.TinyGoRoot(); got == managed {
		t.Errorf("TINYGOROOT still points at managed root %q despite override", managed)
	}
}

// When `tinygo env TINYGOROOT` fails, the root falls back to the override
// binary's grandparent directory (…/tinygo/build/tinygo → …/tinygo).
func TestEnvironWithWarn_ConfigOverride_FallbackToGrandparent(t *testing.T) {
	t.Setenv("GOTHIC_CLI_CACHE_DIR", t.TempDir())
	h := linuxAmd64Helper()

	base := t.TempDir()
	buildDir := filepath.Join(base, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty reply → script exits non-zero → fallback path taken.
	h.ConfigOverride = writeFakeTinygo(t, buildDir, "")

	if err := h.EnsureBinary(); err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	// filepath.Dir(filepath.Dir(base/build/tinygo)) == base.
	if h.overrideRoot != base {
		t.Fatalf("fallback overrideRoot: got %q, want %q", h.overrideRoot, base)
	}
	env := h.EnvironWithWarn(nil)
	if got := tinygoRootFromEnv(t, env); got != base {
		t.Errorf("TINYGOROOT: got %q, want fallback root %q", got, base)
	}
}

// With no override, EnvironWithWarn keeps the managed root untouched.
func TestEnvironWithWarn_NoOverride_KeepsManagedRoot(t *testing.T) {
	t.Setenv("GOTHIC_CLI_CACHE_DIR", t.TempDir())
	h := linuxAmd64Helper()
	env := h.EnvironWithWarn(nil)
	if got, want := tinygoRootFromEnv(t, env), h.TinyGoRoot(); got != want {
		t.Errorf("TINYGOROOT: got %q, want managed root %q", got, want)
	}
}

func tinygoRootFromEnv(t *testing.T, env []string) string {
	t.Helper()
	const pfx = "TINYGOROOT="
	root := ""
	for _, e := range env {
		if strings.HasPrefix(e, pfx) {
			root = strings.TrimPrefix(e, pfx) // last one wins, mirroring exec semantics
		}
	}
	if root == "" {
		t.Fatalf("no TINYGOROOT in env: %v", env)
	}
	return root
}
