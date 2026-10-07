package buildctl

import (
	"regexp"
	"strings"
)

// The diagnosis translation table: every entry maps a raw compiler/generator
// error signature to a plain-language diagnosis, a concrete fix, and — when
// one exists — the name of the bundled skill document covering the area.
// The table is the ONE place the taxonomy lives; add new entries here and
// they surface everywhere a BuildResult is rendered.
//
// Matching is deliberately tolerant (case-insensitive substrings over the
// whole raw text): a missed match is safe by design — the raw text still
// comes back untranslated.
type diagEntry struct {
	match *regexp.Regexp
	diag  string
	fix   string
	skill string
}

var diagTable = []diagEntry{
	{
		// A templ text node that starts with a statement keyword: the templ
		// parser reads `for`/`if` at that position as the start of a Go
		// statement, and the compile fails at the statement's grammar.
		match: regexp.MustCompile(`(?i)\.templ:[0-9]+:[0-9]+.*\b(syntax|unexpected|invalid|cannot)\b.*\b(for|if)\b|\b(for|if)\b.*\b(syntax|unexpected|invalid|cannot)\b.*\.templ:[0-9]+:[0-9]+`),
		diag: "A text node in a .templ file starts with the keyword `for`/`if`; " +
			"the templ grammar reads that position as the start of a Go statement, not text.",
		fix:   "Write the text as a Go expression instead: { \"for …\" } — or start the text node with something that is not a statement keyword.",
		skill: "gothic-routing",
	},
	{
		// An //go:embed directive authored inside templ text is copied into
		// the generated _templ.go, where the directive is not valid.
		match: regexp.MustCompile(`(?i)go:embed.*\b(misplaced|cannot apply|invalid|directive)\b|\b(misplaced|cannot apply|invalid)\b.*go:embed`),
		diag:  "A `//go:embed` directive was authored inside a .templ file; templ copied the comment into the generated Go code, where the directive does not apply.",
		fix:   "Move the //go:embed to a plain .go file in the same package (the embed.FS var belongs there), and reference the loaded value from the templ component.",
		skill: "gothic-invariants",
	},
	{
		// A TinyGo build failing at net/http: a package pulled into the WASM
		// main (typically a Decode[T]/Encode[T] DTO in a package that imports
		// net/http transitively) drags the unimplemented stdlib in.
		match: regexp.MustCompile(`(?i)net[/\\]http.*(not available|not supported|unsupported|TinyGo)|(not available|not supported|unsupported|TinyGo).*net[/\\]http`),
		diag:  "A package reachable from a ClientSideState — most often a Decode[T]/Encode[T] DTO living in a package that imports net/http — drags net/http into the TinyGo WASM build, which cannot compile it.",
		fix:   "Move the DTO into a leaf package with no net/http dependency (e.g. src/shared); keep the handlers in src/api. Do not switch the page to WasmCompiler: routes.Golang for this — the DTO is the fix.",
		skill: "gothic-typed-fetch-json",
	},
	{
		// A topic accessor used before topic_gen.go exists: topic-stub
		// generation must run before the page scan.
		match: regexp.MustCompile(`(?i)undefined: \w*[Tt]opic\b`),
		diag:  "A topic accessor was referenced before src/topics/topic_gen.go existed: topic-stub generation must run before the page scan.",
		fix:   "Regenerate the topic stubs first — Sync/BuildTopics runs PregenerateTopicStubs before the scan; from the CLI run `gothic wasm`.",
		skill: "gothic-wasm-state-topics",
	},
	{
		// Local import resolution: a missing replace directive, an un-tidied
		// go.mod, or GOWORK=off meeting a workspace-only module.
		match: regexp.MustCompile(`(?i)no required module provides package|missing go\.sum entry|no go\.mod file|updates to go\.mod needed|does not contain modules listed in go\.work|no matching versions of`),
		diag:  "A local project package could not be resolved: the import has no module resolution (missing replace directive), `go mod tidy` has not run for the new import, or the build ran with GOWORK=off against a module only the workspace can resolve.",
		fix:   "Check the module's go.mod: add `replace <module> => ../<module>` for the missing sibling, run `go mod tidy` for new imports, and drop GOWORK=off when building from the dev harness.",
		skill: "gothic-invariants",
	},
	{
		// The tree-shaker's own errors: package-level var and init() cannot
		// be inlined into the generated WASM main.
		match: regexp.MustCompile(`(?i)package-level var|init\(\) functions cannot be inlined`),
		diag:  "ClientSideState references a package-level `var` (or depends on `init()`), which cannot be tree-shaken into the WASM binary: vars may have side effects or depend on package initialization.",
		fix:   "Move the declaration inside the ClientSideState body, or restate it as a `const` / `func` (those are tree-shaken transitively).",
		skill: "gothic-wasm-state-topics",
	},
}

// DiagnoseAll translates raw tool output into diagnostics. The raw text is
// preserved verbatim inside every diagnostic; an output no entry recognizes
// still comes back untranslated, so no signal is ever dropped.
func DiagnoseAll(raw string) []Diagnostic {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []Diagnostic
	seen := make(map[string]bool, len(diagTable))
	for _, e := range diagTable {
		if !e.match.MatchString(raw) || seen[e.diag] {
			continue
		}
		seen[e.diag] = true
		out = append(out, Diagnostic{Raw: raw, Diagnosis: e.diag, Fix: e.fix, Skill: e.skill})
	}
	if len(out) == 0 {
		// Unrecognized output: pass the raw text through untouched.
		out = append(out, Diagnostic{Raw: raw})
	}
	return out
}
