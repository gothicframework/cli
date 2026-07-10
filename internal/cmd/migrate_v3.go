/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/cli/v3/internal/scaffold"
	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"
)

// v2ToV3ImportPattern matches a full legacy framework import — the old
// felipegenef module path at any major version (or the bare path), plus its
// package subpath. migrate-v3 rewrites each match to the corresponding NEW
// gothicframework org path. Because v3 split the monorepo into two modules with
// a different folder layout, this is a per-subpath remap (see mapFrameworkImport),
// NOT a naïve prefix swap: e.g. `.../v2/pkg/config` → `…/core/config` and
// `.../v2/pkg/server` → `…/middlewares`.
//
// The rewrite is idempotent: the emitted paths contain no `felipegenef`, so a
// second run matches nothing.
var v2ToV3ImportPattern = regexp.MustCompile(`github\.com/felipegenef/gothicframework(?:/v[0-9]+)?((?:/[A-Za-z0-9_.\-]+)*)`)

const (
	coreModulePath        = "github.com/gothicframework/core"
	componentsModulePath  = "github.com/gothicframework/components"
	middlewaresModulePath = "github.com/gothicframework/middlewares"
	// runtimeModuleVersion is the published version of the suffixless runtime
	// modules (core/components/middlewares are all v1.0.0; only the cli keeps /v3).
	runtimeModuleVersion = "v1.0.0"
)

// frameworkSubpathMap maps an OLD framework package subpath (the suffix after
// `github.com/felipegenef/gothicframework[/vN]`) to its NEW org import path,
// following the Part III split layout. Longest-prefix-first at lookup time.
var frameworkSubpathMap = []struct{ old, new string }{
	{"/pkg/helpers/routes", coreModulePath + "/router"},
	{"/pkg/helpers/runtimeassets", coreModulePath + "/runtimeassets"},
	{"/pkg/helpers/gothiccore", coreModulePath + "/gothiccore"},
	{"/pkg/helpers/corewasm", coreModulePath + "/corewasm"},
	{"/pkg/helpers/astconfig", coreModulePath + "/internal/astconfig"},
	{"/pkg/helpers/proxy", coreModulePath + "/internal/proxy"},
	{"/pkg/helpers/termcolor", coreModulePath + "/internal/termcolor"},
	{"/pkg/helpers/tofu/docker", coreModulePath + "/internal/deploy/docker"},
	{"/pkg/helpers/tofu/tfgen", coreModulePath + "/internal/deploy/tfgen"},
	{"/pkg/helpers/tofu", coreModulePath + "/internal/deploy"},
	{"/pkg/helpers/wasm/astx", coreModulePath + "/internal/build/astx"},
	{"/pkg/helpers/wasm", coreModulePath + "/internal/build"},
	{"/pkg/helpers/templ", coreModulePath + "/render"},
	{"/pkg/helpers", coreModulePath + "/render"},
	{"/pkg/wasm/core-runtime/protocol", coreModulePath + "/wasm/core-runtime/protocol"},
	{"/pkg/wasm/core-runtime", coreModulePath + "/wasm/core-runtime"},
	{"/pkg/wasm/wasm-runtime/runtime", coreModulePath + "/wasm/wasm-runtime/runtime"},
	{"/pkg/wasm/internal/parity", coreModulePath + "/wasm/internal/parity"},
	{"/pkg/wasm", coreModulePath + "/wasm"},
	{"/pkg/config", coreModulePath + "/config"},
	{"/pkg/data/wasm_exec", coreModulePath + "/wasmexec"},
	{"/pkg/data", coreModulePath + "/internal/scaffold"},
	{"/pkg/cli", coreModulePath + "/internal/cli"},
	{"/pkg/server", middlewaresModulePath},
	{"/components", componentsModulePath},
}

// mapFrameworkImport rewrites one full matched legacy framework import to its new
// org path. suffix is the package subpath (may be empty for a bare module ref).
func mapFrameworkImport(full string) string {
	m := v2ToV3ImportPattern.FindStringSubmatch(full)
	suffix := ""
	if len(m) > 1 {
		suffix = m[1]
	}
	if suffix == "" {
		return coreModulePath // bare module reference
	}
	for _, e := range frameworkSubpathMap {
		if suffix == e.old {
			return e.new
		}
		if strings.HasPrefix(suffix, e.old+"/") {
			return e.new + suffix[len(e.old):]
		}
	}
	// Unknown subpath — fall back to core so we never emit a felipegenef path.
	return coreModulePath + suffix
}

// rewriteV2ToV3File rewrites every legacy framework import in the file at path to
// its new org path. Returns whether the file changed.
func rewriteV2ToV3File(path string) (bool, error) {
	original, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	updated := v2ToV3ImportPattern.ReplaceAllFunc(original, func(m []byte) []byte {
		return []byte(mapFrameworkImport(string(m)))
	})
	if bytes.Equal(original, updated) {
		return false, nil
	}
	info, _ := os.Stat(path)
	mode := fs.FileMode(0o644)
	if info != nil {
		mode = info.Mode()
	}
	return true, os.WriteFile(path, updated, mode)
}

// rewriteV2ToV3GoMod drops the legacy felipegenef framework require and adds the
// three new org modules (core + components + middlewares). go mod tidy afterwards prunes whichever
// the project does not actually import.
func rewriteV2ToV3GoMod(path string, content []byte) error {
	mf, err := modfile.Parse(path, content, nil)
	if err != nil {
		return fmt.Errorf("parse go.mod: %w", err)
	}
	legacy := []string{
		"github.com/felipegenef/gothicframework/v2",
		"github.com/felipegenef/gothicframework",
	}
	dropped := false
	for _, old := range legacy {
		for _, r := range mf.Require {
			if r.Mod.Path == old {
				_ = mf.DropRequire(old)
				dropped = true
			}
		}
	}
	if dropped {
		if err := mf.AddRequire(coreModulePath, runtimeModuleVersion); err != nil {
			return err
		}
		if err := mf.AddRequire(componentsModulePath, runtimeModuleVersion); err != nil {
			return err
		}
		if err := mf.AddRequire(middlewaresModulePath, runtimeModuleVersion); err != nil {
			return err
		}
	}
	mf.Cleanup()
	out, err := mf.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

var migrateV3Cmd = &cobra.Command{
	Use:   "migrate-v3",
	Short: "Migrate a v2 Gothic project to v3 (SAM → OpenTofu)",
	Long: `Converts an existing v2 Gothic project to v3: rewrites gothic-config.json
into gothic.config.go, updates the module path from /v2 to /v3, removes SAM
artifacts (template.yaml, samconfig.toml, Dockerfile, embedded SAM templates),
and runs go mod tidy. Idempotent: running it twice is safe.`,
	RunE: runMigrateV3,
}

var (
	migrateV3Path   string
	migrateV3DryRun bool
)

func init() {
	rootCmd.AddCommand(migrateV3Cmd)
	migrateV3Cmd.Flags().StringVar(&migrateV3Path, "path", ".", "Path to the Gothic v2 project root")
	migrateV3Cmd.Flags().BoolVar(&migrateV3DryRun, "dry-run", false, "Print planned changes without modifying any file")
}

// samArtifacts is the list of v2 SAM/Docker on-disk files removed during migration.
var samArtifacts = []string{
	"template.yaml",
	"samconfig.toml",
	"Dockerfile",
	filepath.Join(".gothicCli", "templates", "sam-template.yaml"),
	filepath.Join(".gothicCli", "templates", "samconfig-template.toml"),
	filepath.Join(".gothicCli", "templates", "Dockerfile-template"),
}

func runMigrateV3(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	root := migrateV3Path
	dryRun := migrateV3DryRun

	// Step 1 — pre-flight: require gothic-config.json.
	jsonPath := filepath.Join(root, "gothic-config.json")
	if _, err := os.Stat(jsonPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no gothic-config.json found at " + root + " — not a v2 project")
		}
		return err
	}

	// Step 2 — parse JSON into cli.Config.
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("read gothic-config.json: %w", err)
	}
	var cfg cli.Config
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return fmt.Errorf("parse gothic-config.json: %w", err)
	}

	// Read module name from go.mod for the template.
	goModPath := filepath.Join(root, "go.mod")
	goModBytes, err := os.ReadFile(goModPath)
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	mf, err := modfile.Parse(goModPath, goModBytes, nil)
	if err != nil {
		return fmt.Errorf("parse go.mod: %w", err)
	}
	goModName := mf.Module.Mod.Path

	// Step 3 — write gothic.config.go.
	tmpl, err := template.New("gothic.config.go").Parse(string(data.GothicConfigGoTemplate))
	if err != nil {
		return fmt.Errorf("parse config template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, struct {
		ProjectName string
		GoModName   string
	}{
		ProjectName: cfg.ProjectName,
		GoModName:   goModName,
	}); err != nil {
		return fmt.Errorf("render config template: %w", err)
	}

	bakPath := jsonPath + ".bak"
	if dryRun {
		fmt.Fprintln(out, "[dry-run] Would write gothic.config.go")
	} else {
		// Backup, unless a .bak already exists (idempotency).
		if _, berr := os.Stat(bakPath); errors.Is(berr, os.ErrNotExist) {
			if rerr := os.Rename(jsonPath, bakPath); rerr != nil {
				return fmt.Errorf("backup gothic-config.json: %w", rerr)
			}
		}
		fmt.Fprintln(out, "Writing gothic.config.go")
		if werr := os.WriteFile(filepath.Join(root, "gothic.config.go"), rendered.Bytes(), 0o644); werr != nil {
			return fmt.Errorf("write gothic.config.go: %w", werr)
		}
	}

	// Step 3b — rewrite main.go in place to the v3 middleware form. We surgically
	// replace exactly the two Gothic constructs (gothicRoutes.Setup(...) and the
	// OptimizedImage route registration) with a single gothicServer.Middleware call,
	// preserving every other line the user wrote. If main.go doesn't match the
	// recognized shape (already v3, or heavily customized) it is left untouched — the
	// old form keeps compiling via v3 aliases.
	mainPath := filepath.Join(root, "main.go")
	if dryRun {
		fmt.Fprintln(out, "[dry-run] Would rewrite main.go to the v3 middleware form (preserving custom code)")
	} else if _, serr := os.Stat(mainPath); serr == nil {
		rewritten, runtimeLiteral, rerr := rewriteMainGoV3(mainPath)
		if rerr != nil {
			return fmt.Errorf("rewrite main.go: %w", rerr)
		}
		if rewritten {
			fmt.Fprintln(out, "Rewrote main.go to the v3 middleware form (your other code was preserved)")
			// Carry the old AppConfig cache settings into gothic.config.go's Runtime
			// block automatically — no manual step, no lost config.
			if runtimeLiteral != "" {
				if ierr := injectRuntimeConfig(filepath.Join(root, "gothic.config.go"), runtimeLiteral); ierr != nil {
					return fmt.Errorf("migrate Runtime config into gothic.config.go: %w", ierr)
				}
				fmt.Fprintln(out, "Migrated your cache/static config into gothic.config.go's Runtime block")
			}
		} else {
			fmt.Fprintln(out, "Left main.go unchanged (not the standard shape). To adopt the v3 form, replace")
			fmt.Fprintln(out, "  gothicRoutes.Setup(router, gothicRoutes.AppConfig{...}, routes.RegisterFileBasedRoutes)")
			fmt.Fprintln(out, "  gothicComponents.OptimizedImageConfig.RegisterRoute(...)")
			fmt.Fprintln(out, "with")
			fmt.Fprintln(out, "  router.Use(gothicServer.Middleware(Config.Runtime))")
			fmt.Fprintln(out, "  routes.RegisterFileBasedRoutes(router)")
			fmt.Fprintln(out, "and move the AppConfig cache settings into gothic.config.go's Runtime block.")
		}
	}

	// Step 3c — rewrite layouts to the v3 runtime-asset components and drop the
	// framework runtime JS from public/. In v3 gothic-core*.js / wasm_exec.js are
	// served from the framework embed under /_gothic/ instead of being copied into
	// each project, so the layouts reference @gothicComponents.RuntimeScripts() /
	// @gothicComponents.Styles() and the stale files are removed.
	if dryRun {
		fmt.Fprintln(out, "[dry-run] Would rewrite layouts to @gothicComponents.RuntimeScripts()/Styles() and remove orphaned public/ runtime JS")
	} else {
		var rewrittenLayouts int
		lerr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				switch d.Name() {
				// _local-gothicframework is a dev-time local mirror of the framework
				// source; skip it so the migrator doesn't rewrite the mirror itself.
				case "vendor", ".git", "node_modules", "_local-gothicframework", ".gothicCli":
					return fs.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".templ" {
				return nil
			}
			ok, rerr := rewriteLayoutTemplV3(path)
			if rerr != nil {
				return rerr
			}
			if ok {
				rewrittenLayouts++
			}
			return nil
		})
		if lerr != nil {
			return fmt.Errorf("rewrite layouts: %w", lerr)
		}
		if rewrittenLayouts > 0 {
			fmt.Fprintf(out, "Rewrote %d layout(s) to @gothicComponents.RuntimeScripts()/Styles()\n", rewrittenLayouts)
		}
		if removed := cleanOrphanedRuntimeAssets(filepath.Join(root, "public")); len(removed) > 0 {
			fmt.Fprintf(out, "Removed framework runtime JS now served from /_gothic/: %s\n", strings.Join(removed, ", "))
		}
	}

	// Step 4 — remove SAM artifacts.
	for _, f := range samArtifacts {
		full := filepath.Join(root, f)
		if _, serr := os.Stat(full); errors.Is(serr, os.ErrNotExist) {
			continue
		}
		if dryRun {
			fmt.Fprintf(out, "[dry-run] Would remove %s\n", f)
			continue
		}
		fmt.Fprintf(out, "Removing %s\n", f)
		if rerr := os.Remove(full); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f, rerr)
		}
	}

	// Step 5 — rewrite imports in .go and .templ files.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".templ" {
			return nil
		}
		if dryRun {
			original, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// Every match is a legacy felipegenef framework import that will be
			// remapped to the new org path; already-migrated imports contain no
			// felipegenef and so do not match.
			changes := len(v2ToV3ImportPattern.FindAll(original, -1))
			if changes > 0 {
				rel, _ := filepath.Rel(absRoot, path)
				fmt.Fprintf(out, "[dry-run] Would rewrite %d import(s) in %s\n", changes, rel)
			}
			return nil
		}
		_, rerr := rewriteV2ToV3File(path)
		return rerr
	})
	if walkErr != nil {
		return walkErr
	}

	// Step 6 — remove the v3-removed topic mount surface. In v3 the topic mount
	// (@AddXxxTopic() / TopicConfig.ComponentFnName) is GONE — the always-loaded
	// core owns every topic and a consumer auto-registers just by using the
	// generated accessor in ClientSideState. A migrated v2 project therefore must
	// have (a) ComponentFnName fields stripped from its TopicConfig literals so
	// src/topics/*.go compiles against the field-less v3 TopicConfig, and (b) the
	// @Mount() calls removed from its .templ files so the pages compile.
	if err := cleanTopicMounts(root, dryRun, out); err != nil {
		return err
	}

	// Step 7 — rewrite go.mod (/v2 → /v3).
	if !dryRun {
		if err := rewriteV2ToV3GoMod(goModPath, goModBytes); err != nil {
			return err
		}
	}

	// Step 8 — go mod tidy (non-fatal).
	if !dryRun {
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = root
		tidy.Stderr = os.Stderr
		if terr := tidy.Run(); terr != nil {
			fmt.Fprintf(out, "warning: go mod tidy failed: %v\n", terr)
		}
	}

	// Step 9 — print import playbook.
	fmt.Fprint(out, migrateTofuPlaybook)
	return nil
}

// cleanTopicMounts removes the v3-removed topic-mount surface from a migrated
// project:
//
//  1. It strips the `ComponentFnName` field from the TopicConfig literals passed
//     to CreateTopic in src/topics/*.go, so the source compiles against the
//     field-less v3 TopicConfig.
//  2. It removes the generated mount calls (@Mount() / @wasm.Mount()) from the
//     project's .templ files, so pages that mounted a topic still compile.
//
// The set of mount names to strip is derived by scanning each CreateTopic call
// and reproducing the v2 CLI's name resolution: the mount name is the
// ComponentFnName value if set, otherwise "Add" + accessor, where accessor is
// SubscriberFnName if set, else the declaring var name, else <Struct>Topic.
//
// Pass 1 is AST-scoped: it removes ONLY a `ComponentFnName` key that sits inside
// a TopicConfig literal actually passed to CreateTopic — never a bare
// `ComponentFnName:` line belonging to some unrelated struct, and it handles both
// multiline and single-line (inline) literals. Both passes honor dryRun (print,
// don't write). A project with no src/topics/ directory is a no-op. The function
// is idempotent: a second run finds nothing to change.
func cleanTopicMounts(root string, dryRun bool, out io.Writer) error {
	topicsDir := filepath.Join(root, "src", "topics")
	entries, err := os.ReadDir(topicsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no topics — nothing to clean
		}
		return fmt.Errorf("read src/topics: %w", err)
	}

	mountNames := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "topic_gen.go" {
			continue
		}
		path := filepath.Join(topicsDir, e.Name())
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", path, rerr)
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, content, 0)
		if perr != nil {
			// Unparseable source — we cannot safely locate the field, so skip this
			// file rather than risk a bad byte edit. Migration continues.
			continue
		}

		// Scan CreateTopic calls: collect mount names AND the exact ComponentFnName
		// field nodes to excise. Names are derived from the ORIGINAL AST, so a
		// ComponentFnName value still drives its own mount name before removal.
		topics := scanMigrateTopics(f)
		var kvs []*ast.KeyValueExpr
		for _, ti := range topics {
			if ti.mountName != "" {
				mountNames[ti.mountName] = struct{}{}
			}
			if ti.componentFnKV != nil {
				kvs = append(kvs, ti.componentFnKV)
			}
		}
		if len(kvs) == 0 {
			continue
		}

		stripped := removeCompositeFields(content, fset, kvs)
		if bytes.Equal(stripped, content) {
			continue
		}
		// Defense-in-depth: never write source we just broke.
		if _, verr := parser.ParseFile(token.NewFileSet(), path, stripped, 0); verr != nil {
			return fmt.Errorf("internal: stripping ComponentFnName from %s produced invalid Go: %w", relOrPath(root, path), verr)
		}
		rel := relOrPath(root, path)
		if dryRun {
			fmt.Fprintf(out, "[dry-run] Would remove ComponentFnName field from %s\n", rel)
			continue
		}
		fmt.Fprintf(out, "Removing ComponentFnName field from %s\n", rel)
		if werr := os.WriteFile(path, stripped, filePerm(path)); werr != nil {
			return fmt.Errorf("rewrite %s: %w", rel, werr)
		}
	}

	if len(mountNames) == 0 {
		return nil
	}
	return stripTopicMountCalls(root, mountNames, dryRun, out)
}

// stripTopicMountCalls removes any @Mount() / @qualifier.Mount() invocation of a
// known topic-mount name from every .templ file under src/. Matching is line-
// scoped and exact on the mount name (an optional identifier qualifier is
// allowed, e.g. @topics.AddPageTopic()), so no unrelated component call is
// touched.
func stripTopicMountCalls(root string, mountNames map[string]struct{}, dryRun bool, out io.Writer) error {
	alts := make([]string, 0, len(mountNames))
	for n := range mountNames {
		alts = append(alts, regexp.QuoteMeta(n))
	}
	sort.Strings(alts) // deterministic regex
	// ^<indent>@[qualifier.]Name()<trailing-space>\n — the whole line is removed.
	re := regexp.MustCompile(`(?m)^[ \t]*@(?:[A-Za-z_]\w*\.)?(?:` + strings.Join(alts, "|") + `)\(\)[ \t]*\r?\n`)

	srcDir := filepath.Join(root, "src")
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if errors.Is(werr, os.ErrNotExist) {
				return nil
			}
			return werr
		}
		if d.IsDir() || filepath.Ext(path) != ".templ" {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", path, rerr)
		}
		matches := re.FindAll(content, -1)
		if len(matches) == 0 {
			return nil
		}
		rel := relOrPath(root, path)
		verb := "Removed"
		if dryRun {
			verb = "Would remove"
			fmt.Fprintf(out, "[dry-run] %s %d topic-mount call(s) from %s — review the diff; a same-named self-closing component (if any) would also be removed.\n", verb, len(matches), rel)
			return nil
		}
		fmt.Fprintf(out, "%s %d topic-mount call(s) from %s — review the diff; a same-named self-closing component (if any) would also be removed.\n", verb, len(matches), rel)
		return os.WriteFile(path, re.ReplaceAll(content, nil), filePerm(path))
	})
}

// migrateTopic is one scanned CreateTopic call site: the mount name it would have
// generated, plus the ComponentFnName KeyValueExpr node (if any) so the field can
// be excised by exact position.
type migrateTopic struct {
	mountName     string
	componentFnKV *ast.KeyValueExpr
}

// scanMigrateTopics reproduces the v2 CLI's mount-name resolution for every
// CreateTopic(T{}, TopicConfig{...}) call in f, and captures the ComponentFnName
// field node inside each TopicConfig literal for precise removal.
func scanMigrateTopics(f *ast.File) []migrateTopic {
	var out []migrateTopic
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, val := range vs.Values {
				call, ok := val.(*ast.CallExpr)
				if !ok || !isMigrateCreateTopicCall(call.Fun) || len(call.Args) < 2 {
					continue
				}
				structName := migrateTopicStructName(call.Args[0])
				subscriberFnName, componentFnName, componentKV := migrateParseTopicConfig(call.Args[1])

				var varName string
				if i < len(vs.Names) {
					if nm := vs.Names[i].Name; nm != "_" {
						varName = nm
					}
				}
				accessor := subscriberFnName
				if accessor == "" {
					accessor = varName
				}
				if accessor == "" && structName != "" {
					accessor = structName + "Topic"
				}
				mount := componentFnName
				if mount == "" && accessor != "" {
					mount = "Add" + accessor
				}
				out = append(out, migrateTopic{mountName: mount, componentFnKV: componentKV})
			}
		}
		return true
	})
	return out
}

// isMigrateCreateTopicCall reports whether fun names CreateTopic, in bare,
// selector (wasm.CreateTopic), or generic-instantiation form.
func isMigrateCreateTopicCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "CreateTopic"
	case *ast.SelectorExpr:
		return f.Sel != nil && f.Sel.Name == "CreateTopic"
	case *ast.IndexExpr:
		return isMigrateCreateTopicCall(f.X)
	case *ast.IndexListExpr:
		return isMigrateCreateTopicCall(f.X)
	}
	return false
}

// migrateTopicStructName extracts the struct type name from CreateTopic's first
// argument (a T{} composite literal, or a bare T identifier).
func migrateTopicStructName(arg ast.Expr) string {
	switch a := arg.(type) {
	case *ast.CompositeLit:
		switch t := a.Type.(type) {
		case *ast.Ident:
			return t.Name
		case *ast.SelectorExpr:
			if t.Sel != nil {
				return t.Sel.Name
			}
		}
	case *ast.Ident:
		return a.Name
	}
	return ""
}

// migrateParseTopicConfig reads SubscriberFnName and ComponentFnName from a
// TopicConfig composite literal and returns the ComponentFnName KeyValueExpr node
// (nil when absent) so the field can be located and removed by position.
func migrateParseTopicConfig(arg ast.Expr) (subscriberFnName, componentFnName string, componentFnKV *ast.KeyValueExpr) {
	cl, ok := arg.(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "SubscriberFnName":
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if unq, err := strconv.Unquote(bl.Value); err == nil {
					subscriberFnName = unq
				}
			}
		case "ComponentFnName":
			componentFnKV = kv
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if unq, err := strconv.Unquote(bl.Value); err == nil {
					componentFnName = unq
				}
			}
		}
	}
	return
}

// removeCompositeFields deletes each given KeyValueExpr field from src by byte
// position, keeping the surrounding composite literal valid Go. It handles both
// styles:
//
//   - own-line (multiline) field → the whole line, including indent and trailing
//     newline, is removed;
//   - inline field → the field plus one adjacent comma is removed (trailing comma
//     if present, else the preceding comma), so no dangling comma remains.
//
// Removals are applied last-to-first so earlier offsets stay valid.
func removeCompositeFields(src []byte, fset *token.FileSet, kvs []*ast.KeyValueExpr) []byte {
	type span struct{ start, end int }
	spans := make([]span, 0, len(kvs))
	for _, kv := range kvs {
		s := fset.Position(kv.Pos()).Offset
		e := fset.Position(kv.End()).Offset
		start, end := fieldRemovalSpan(src, s, e)
		spans = append(spans, span{start, end})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })

	out := src
	for _, sp := range spans {
		if sp.start < 0 || sp.end > len(out) || sp.start >= sp.end {
			continue
		}
		next := make([]byte, 0, len(out)-(sp.end-sp.start))
		next = append(next, out[:sp.start]...)
		next = append(next, out[sp.end:]...)
		out = next
	}
	return out
}

// fieldRemovalSpan computes the [start,end) byte range to delete for a composite
// field whose key..value occupies src[s:e]. See removeCompositeFields for the
// rules it implements.
func fieldRemovalSpan(src []byte, s, e int) (int, int) {
	n := len(src)
	isHSpace := func(b byte) bool { return b == ' ' || b == '\t' }

	// Consume trailing horizontal whitespace, then an optional single comma, then
	// the horizontal whitespace after that comma.
	eTrail := e
	for eTrail < n && isHSpace(src[eTrail]) {
		eTrail++
	}
	hadTrailingComma := false
	if eTrail < n && src[eTrail] == ',' {
		hadTrailingComma = true
		eTrail++
		for eTrail < n && isHSpace(src[eTrail]) {
			eTrail++
		}
	}

	// Start of the physical line containing s.
	lineStart := s
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	leadingAllWS := true
	for i := lineStart; i < s; i++ {
		if !isHSpace(src[i]) {
			leadingAllWS = false
			break
		}
	}
	atLineEnd := eTrail >= n || src[eTrail] == '\n'

	// Own-line field: remove the whole line plus its trailing newline.
	if leadingAllWS && atLineEnd {
		end := eTrail
		if end < n && src[end] == '\n' {
			end++
		}
		return lineStart, end
	}

	// Inline field with a trailing comma: drop field + that comma (+ following ws).
	if hadTrailingComma {
		return s, eTrail
	}

	// Inline last field (no trailing comma): also swallow the preceding comma so
	// the prior field does not end with a dangling ", ".
	precStart := s
	for precStart > 0 && isHSpace(src[precStart-1]) {
		precStart--
	}
	if precStart > 0 && src[precStart-1] == ',' {
		precStart--
	}
	return precStart, e
}

// relOrPath returns path relative to root for display, falling back to the
// absolute path when it can't be made relative.
func relOrPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

// filePerm returns the file's current mode, defaulting to 0o644 when it can't be
// stat'd, so a rewrite preserves the original permission bits.
func filePerm(path string) fs.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode()
	}
	return 0o644
}

const migrateTofuPlaybook = `Migration complete!

BREAKING CHANGE — topics: v3 removed the topic mount entirely. The
TopicConfig.ComponentFnName field and the generated @AddXxxTopic() mount no
longer exist. migrate-v3 has auto-stripped both for you: ComponentFnName fields
were removed from src/topics/*.go and @Mount() calls were removed from your
.templ files. Topics now work accessor-only — declare CreateTopic in src/topics/
and use the generated accessor in ClientSideState; the always-loaded core does
the rest. If you referenced a mount name anywhere else by hand, remove it.

Manual import playbook (only if you had existing SAM/CloudFormation stacks deployed):

NOTE: v2 resource names used a random app-id from .gothicCli/app-id.txt.
      v3 uses a deterministic suffix derived from your module name instead.
      Your new resource names will differ — you must import the OLD names so
      existing data (S3 contents, CloudFront config) is not destroyed.

1. From the AWS CloudFormation console, note your existing resource physical names
   under stack outputs: CloudFrontId, BucketName, LambdaName.
2. After ` + "`gothic deploy --stage <stage>`" + ` (which initializes the tofu state),
   run from your project root:
     tofu -chdir=.gothicCli/tofu/<stage> import aws_cloudfront_distribution.main <CloudFrontId>
     tofu -chdir=.gothicCli/tofu/<stage> import aws_s3_bucket.main <BucketName>
     tofu -chdir=.gothicCli/tofu/<stage> import aws_lambda_function.main <LambdaName>
3. Run ` + "`tofu -chdir=.gothicCli/tofu/<stage> plan`" + ` and verify no destructive
   changes before applying.
`
