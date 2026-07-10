package cmd

import (
	"os"
	"path/filepath"
	"strings"
)

// rewriteLayoutTemplV3 rewrites a .templ layout in place to the v3 runtime-asset
// components. A v2 Gothic layout loads htmx from unpkg in its <head>; that <script>
// is the anchor we swap for @gothicComponents.RuntimeScripts() (which now bundles
// gothic-core, gothic-core-boot, htmx and the hx-ext extension). The
// /public/styles.css <link> becomes @gothicComponents.Styles(), and the now-bundled
// hx-ext <script> — plus any gothic-core* <script> from a partially-migrated project
// — are dropped. Everything else (favicon links, the body's hx-ext attribute, the
// file's own markup) is left intact, and the components import is added when missing.
//
// Returns false (file untouched) when the file is not a Gothic layout (no unpkg htmx
// reference) or has already been migrated (RuntimeScripts already present), which
// keeps the migration idempotent.
func rewriteLayoutTemplV3(path string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	s := string(src)
	if strings.Contains(s, "gothicComponents.RuntimeScripts()") {
		return false, nil // already migrated
	}
	if !strings.Contains(s, "unpkg.com/htmx") {
		return false, nil // not a Gothic layout <head>
	}

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		switch {
		case strings.Contains(ln, "<link") && strings.Contains(ln, "styles.css"):
			out = append(out, indent+"@gothicComponents.Styles()")
		case strings.Contains(ln, "<script") && strings.Contains(ln, "unpkg.com/htmx"):
			out = append(out, indent+"@gothicComponents.RuntimeScripts()")
		case strings.Contains(ln, "<script") && (strings.Contains(ln, "hx-ext-amz-content-sha256") ||
			strings.Contains(ln, "gothic-core")):
			// Bundled into RuntimeScripts now — drop the line.
			continue
		default:
			out = append(out, ln)
		}
	}
	result := ensureLayoutComponentsImport(strings.Join(out, "\n"))
	return true, os.WriteFile(path, []byte(result), 0o644)
}

// ensureLayoutComponentsImport adds the gothicComponents import to a .templ file
// when it is not already imported — into an existing `import (` block, or as a new
// single import after the `package` line.
func ensureLayoutComponentsImport(content string) string {
	if strings.Contains(content, componentsImportPath) {
		return content
	}
	const full = "github.com/gothicframework/components"
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "import (" {
			ins := "\tgothicComponents \"" + full + "\""
			lines = append(lines[:i+1], append([]string{ins}, lines[i+1:]...)...)
			return strings.Join(lines, "\n")
		}
	}
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "package ") {
			ins := []string{"", "import gothicComponents \"" + full + "\""}
			lines = append(lines[:i+1], append(ins, lines[i+1:]...)...)
			return strings.Join(lines, "\n")
		}
	}
	return content
}

// cleanOrphanedRuntimeAssets removes the framework runtime assets that v3 now serves
// from /_gothic/ rather than public/. wasm_exec_go.js is kept — it is version-tied
// to the user's Go toolchain and is still generated for standard-Go pages.
func cleanOrphanedRuntimeAssets(publicDir string) []string {
	orphans := []string{
		"gothic-core.js", "gothic-core-boot.js", "gothic-core-exec.js",
		"gothic-core.wasm", "wasm_exec.js",
	}
	var removed []string
	for _, name := range orphans {
		if err := os.Remove(filepath.Join(publicDir, name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}
