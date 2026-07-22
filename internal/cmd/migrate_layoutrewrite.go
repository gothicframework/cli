package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// rewriteLayoutTemplV3 rewrites a .templ layout in place to the v3 runtime-asset
// components. A v2 Gothic layout loads htmx from unpkg in its <head>; that <script>
// is the anchor we swap for @gothicComponents.RuntimeScripts() (which emits
// gothic-core and gothic-core-boot; htmx now ships inside gothic-core.wasm, so the
// separate htmx <script> is dropped, not replaced). The /public/styles.css <link> becomes
// @gothicComponents.Styles(), and any leftover hx-ext-amz-content-sha256 <script> —
// plus any gothic-core* <script> from a partially-migrated project — are dropped.
//
// In v3 AWS request-signing is performed automatically by the Gothic core WASM
// runtime (activated when GOTHIC_PROVIDER=AWS), so the old manual opt-in is obsolete:
// the body's hx-ext="amz-content-sha256" attribute is STRIPPED. When hx-ext carries
// only that token the whole attribute is removed; when it is combined with other
// extensions (e.g. hx-ext="preload,amz-content-sha256") only the amz token is removed
// and the remaining value is left well-formed (hx-ext="preload"). Every other
// attribute (hx-boost, class, …), the favicon links and the file's own markup are
// left intact, and the components import is added when missing.
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
	joined := stripAmzHxExt(strings.Join(out, "\n"))
	result := ensureLayoutComponentsImport(joined)
	return true, os.WriteFile(path, []byte(result), 0o644)
}

// amzHxExtRe matches a double-quoted hx-ext attribute plus any whitespace preceding
// it, so that dropping the whole attribute does not leave a dangling space between
// the previous attribute and the next one. templ layouts always double-quote
// attribute values (Go's RE2 has no backreferences, so a single pattern for both
// quote styles is not possible — double quotes are what v2 layouts ship).
var amzHxExtRe = regexp.MustCompile(`\s*hx-ext\s*=\s*"([^"]*)"`)

// stripAmzHxExt removes the "amz-content-sha256" token from every hx-ext attribute:
//   - hx-ext="amz-content-sha256"            → attribute removed entirely
//   - hx-ext="preload,amz-content-sha256"    → hx-ext="preload"
//   - hx-ext="amz-content-sha256,preload"    → hx-ext="preload"
//
// An hx-ext with no amz token is rewritten to an equivalent value (tokens trimmed),
// so it is effectively left intact. Other attributes are untouched.
func stripAmzHxExt(s string) string {
	return amzHxExtRe.ReplaceAllStringFunc(s, func(m string) string {
		value := amzHxExtRe.FindStringSubmatch(m)[1]
		kept := make([]string, 0, 4)
		for _, tok := range strings.Split(value, ",") {
			if t := strings.TrimSpace(tok); t != "" && t != "amz-content-sha256" {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return "" // only amz (or empty) — drop the whole attribute + its leading space
		}
		return ` hx-ext="` + strings.Join(kept, ",") + `"`
	})
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
