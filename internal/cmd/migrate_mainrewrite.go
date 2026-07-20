package cmd

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

const (
	routesImportPath     = "gothicframework/core/router"
	componentsImportPath = "gothicframework/components"
	serverImportLine     = "\tgothicServer \"github.com/gothicframework/middlewares\""
)

// rewriteMainGoV3 rewrites main.go in place to the v3 middleware form, surgically
// replacing ONLY the two Gothic constructs — gothicRoutes.Setup(...) and the
// OptimizedImage route registration — with a single gothicServer.Middleware call,
// while preserving every other statement the user wrote. It returns rewritten=false
// (leaving the file untouched) when main.go isn't the recognized shape, so a
// heavily-customized or already-migrated main.go is never clobbered.
//
// runtimeLiteral is the old AppConfig literal converted to a gothic.RuntimeConfig
// literal (e.g. "gothic.RuntimeConfig{CacheStrategy: gothic.REDIS}"), for the caller
// to inject into gothic.config.go's Runtime field so the user's cache settings are
// migrated automatically. It is "" when the Setup call had no inline AppConfig.
func rewriteMainGoV3(mainPath string) (rewritten bool, runtimeLiteral string, err error) {
	src, err := os.ReadFile(mainPath)
	if err != nil {
		return false, "", err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, src, parser.ParseComments)
	if err != nil {
		return false, "", fmt.Errorf("parse: %w", err)
	}

	var mainFn *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	if mainFn == nil || mainFn.Body == nil {
		return false, "", nil
	}

	var setupStmt, optImgStmt ast.Stmt
	var routerText, registerText string
	var appCfg *ast.CompositeLit
	for _, stmt := range mainFn.Body.List {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		switch sel.Sel.Name {
		case "Setup":
			// gothicRoutes.Setup(router, gothicRoutes.AppConfig{...}, registerFn)
			if len(call.Args) == 3 {
				setupStmt = stmt
				routerText = nodeText(fset, src, call.Args[0])
				registerText = nodeText(fset, src, call.Args[2])
				if cl, ok := call.Args[1].(*ast.CompositeLit); ok {
					appCfg = cl
				}
			}
		case "RegisterRoute":
			// gothicComponents.OptimizedImageConfig.RegisterRoute(...)
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "OptimizedImageConfig" {
				optImgStmt = stmt
			}
		}
	}
	if setupStmt == nil || optImgStmt == nil {
		// Not the standard scaffold shape (e.g. a hand-customized main.go). Fall back
		// to a purely additive inject: mount the runtime middleware without touching
		// any other statement the user wrote. Returns rewritten=false when it isn't a
		// recognizable Gothic main, so nothing is clobbered.
		return injectMiddlewareV3(mainPath, src, fset, file, mainFn)
	}

	routesAlias := importAlias(file, routesImportPath)
	componentsAlias := importAlias(file, componentsImportPath)

	// Replace the two constructs by whole-line edits so all other lines keep their
	// exact text. The Setup call becomes the middleware + route-registration pair;
	// the OptimizedImage registration is dropped (the middleware owns it now).
	lines := strings.Split(string(src), "\n")
	indent := lineIndent(lines, fset.Position(setupStmt.Pos()).Line)

	type edit struct {
		start, end int // 1-based inclusive line range
		repl       []string
	}
	edits := []edit{
		{
			start: startLineWithComment(file, fset, setupStmt),
			end:   fset.Position(setupStmt.End()).Line,
			repl: []string{
				indent + routerText + ".Use(gothicServer.Middleware(Config.Runtime))",
				indent + registerText + "(" + routerText + ")",
			},
		},
		{
			start: startLineWithComment(file, fset, optImgStmt),
			end:   fset.Position(optImgStmt.End()).Line,
			repl:  nil,
		},
	}
	// Apply bottom-up so earlier line numbers stay valid.
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		next := append([]string{}, lines[:e.start-1]...)
		next = append(next, e.repl...)
		next = append(next, lines[e.end:]...)
		lines = next
	}
	out := strings.Join(lines, "\n")

	// Fix imports: add gothicServer; drop the framework routes/components imports
	// only when nothing in the rewritten file references them anymore.
	out = ensureServerImport(out)
	if routesAlias != "" && !strings.Contains(out, routesAlias+".") {
		out = removeImportLine(out, routesImportPath)
	}
	if componentsAlias != "" && !strings.Contains(out, componentsAlias+".") {
		// Match the closing quote so the bare components import (…/components")
		// is dropped without also nuking the server import (…/middlewares") — the
		// server now lives in its own module, so no subpath overlap remains.
		out = removeImportLine(out, componentsImportPath+"\"")
	}

	// gofmt + validate. A parse error here means our edit produced invalid Go, so we
	// abort rather than write a broken main.go.
	formatted, ferr := format.Source([]byte(out))
	if ferr != nil {
		return false, "", fmt.Errorf("rewritten main.go is invalid (left unchanged): %w", ferr)
	}
	if werr := os.WriteFile(mainPath, formatted, 0o644); werr != nil {
		return false, "", werr
	}

	if appCfg != nil && routesAlias != "" {
		runtimeLiteral = toRuntimeLiteral(fset, src, appCfg, routesAlias)
	}
	return true, runtimeLiteral, nil
}

// injectMiddlewareV3 is the additive fallback used when main.go is not the standard
// scaffold shape. When main.go creates a chi router and registers Gothic's
// file-based routes but does not yet mount the v3 runtime middleware, it inserts a
// single `<router>.Use(gothicServer.Middleware(Config.Runtime))` line — after the
// user's own router.Use calls, mirroring the scaffold order — and adds the
// middlewares import. It removes nothing, so a hand-customized main.go (e.g. a
// bespoke LOCAL_SERVE block) is preserved. It is idempotent (a main.go already
// mounting the middleware is left untouched) and a no-op (rewritten=false) when no
// single chi router + RegisterFileBasedRoutes pair is found.
func injectMiddlewareV3(mainPath string, src []byte, fset *token.FileSet, file *ast.File, mainFn *ast.FuncDecl) (bool, string, error) {
	s := string(src)
	// Already wired — also the idempotency guard for a second migrate run.
	if strings.Contains(s, ".Middleware(Config.Runtime)") {
		return false, "", nil
	}
	// Only touch a genuine Gothic main — one that registers the file-based routes.
	if !strings.Contains(s, "RegisterFileBasedRoutes") {
		return false, "", nil
	}

	routerName, newMuxLine := findChiRouter(mainFn, fset)
	if routerName == "" {
		return false, "", nil
	}

	// Insert after the last existing <router>.Use(...) so the Gothic middleware lands
	// after the user's own (e.g. the logger), else right after the router is created.
	insertAfter := newMuxLine
	for _, stmt := range mainFn.Body.List {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Use" {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == routerName {
			if ln := fset.Position(stmt.End()).Line; ln > insertAfter {
				insertAfter = ln
			}
		}
	}

	lines := strings.Split(s, "\n")
	if insertAfter < 1 || insertAfter > len(lines) {
		return false, "", nil
	}
	indent := lineIndent(lines, insertAfter)
	injected := indent + routerName + ".Use(gothicServer.Middleware(Config.Runtime))"
	next := append([]string{}, lines[:insertAfter]...)
	next = append(next, injected)
	next = append(next, lines[insertAfter:]...)
	out := ensureServerImport(strings.Join(next, "\n"))

	formatted, ferr := format.Source([]byte(out))
	if ferr != nil {
		return false, "", fmt.Errorf("rewritten main.go is invalid (left unchanged): %w", ferr)
	}
	if werr := os.WriteFile(mainPath, formatted, 0o644); werr != nil {
		return false, "", werr
	}
	return true, "", nil
}

// findChiRouter returns the variable name and 1-based line of the sole
// `<name> := chi.NewMux()` / `chi.NewRouter()` assignment in fn. It returns ("", 0)
// unless there is exactly one such assignment, so the injector never guesses between
// multiple routers.
func findChiRouter(fn *ast.FuncDecl, fset *token.FileSet) (string, int) {
	var name string
	var line, count int
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "NewMux" && sel.Sel.Name != "NewRouter") {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "chi" {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		count++
		name = id.Name
		line = fset.Position(as.Pos()).Line
		return true
	})
	if count != 1 {
		return "", 0
	}
	return name, line
}

// toRuntimeLiteral converts an old gothicRoutes.AppConfig{...} literal into the
// equivalent gothic.RuntimeConfig{...} literal (AppConfig and RuntimeConfig are the
// same type via aliases; only the package-qualified names differ).
func toRuntimeLiteral(fset *token.FileSet, src []byte, appCfg *ast.CompositeLit, routesAlias string) string {
	lit := nodeText(fset, src, appCfg)
	lit = strings.ReplaceAll(lit, routesAlias+".", "gothic.")
	lit = strings.Replace(lit, "gothic.AppConfig", "gothic.RuntimeConfig", 1)
	return renameStaticFilesModeConsts(lit)
}

// renameStaticFilesModeConsts rewrites the pre-cloud-agnostic StaticFilesMode
// constant names to their current identifiers. The enum ordinals are unchanged —
// only the names were renamed: HOT_RELOAD_ONLY → CDN, ALL_ENVS → DISK. Matching is
// scoped to a package-qualified reference (a leading '.') so it only touches
// `gothic.HOT_RELOAD_ONLY` / `helpers.ALL_ENVS` / `config.ALL_ENVS`, never an
// unrelated user identifier that happens to share the name. Without this, a v2/older
// project's `ServeStaticFiles: gothic.ALL_ENVS` would migrate to a name that no
// longer exists and fail to compile.
func renameStaticFilesModeConsts(s string) string {
	s = strings.ReplaceAll(s, ".HOT_RELOAD_ONLY", ".CDN")
	s = strings.ReplaceAll(s, ".ALL_ENVS", ".DISK")
	return s
}

// injectRuntimeConfig replaces the value of the Runtime field in gothic.config.go
// with literal, so the cache settings carried over from the old main.go land in the
// config. The surrounding doc comment and every other field are untouched; the file
// is gofmt'd afterwards.
func injectRuntimeConfig(configPath, literal string) error {
	src, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, configPath, src, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse gothic.config.go: %w", err)
	}
	var runtimeVal ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		if runtimeVal != nil {
			return false
		}
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Runtime" {
				runtimeVal = kv.Value
				return false
			}
		}
		return true
	})
	if runtimeVal == nil {
		return nil // no Runtime field (unexpected with our template) — leave as-is
	}
	start := fset.Position(runtimeVal.Pos()).Offset
	end := fset.Position(runtimeVal.End()).Offset
	out := string(src[:start]) + literal + string(src[end:])
	formatted, ferr := format.Source([]byte(out))
	if ferr != nil {
		return fmt.Errorf("injected gothic.config.go is invalid: %w", ferr)
	}
	return os.WriteFile(configPath, formatted, 0o644)
}

func nodeText(fset *token.FileSet, src []byte, n ast.Node) string {
	return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
}

// importAlias returns the identifier a project uses for the framework import whose
// path contains pathSuffix (its explicit alias, or the package's default name).
func importAlias(file *ast.File, pathSuffix string) string {
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, "\"")
		if strings.Contains(p, pathSuffix) {
			if imp.Name != nil {
				return imp.Name.Name
			}
			parts := strings.Split(p, "/")
			return parts[len(parts)-1]
		}
	}
	return ""
}

// startLineWithComment returns the 1-based start line of n, extended upward to
// include a doc/comment group that ends on the line immediately above it.
func startLineWithComment(file *ast.File, fset *token.FileSet, n ast.Node) int {
	start := fset.Position(n.Pos()).Line
	for _, cg := range file.Comments {
		if fset.Position(cg.End()).Line == start-1 {
			if s := fset.Position(cg.Pos()).Line; s < start {
				start = s
			}
		}
	}
	return start
}

func lineIndent(lines []string, lineNum int) string {
	if lineNum < 1 || lineNum > len(lines) {
		return "\t"
	}
	ln := lines[lineNum-1]
	return ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
}

// ensureServerImport inserts the gothicServer import at the end of the import
// block (the third-party group) if absent; gofmt then sorts it into place.
func ensureServerImport(out string) string {
	if strings.Contains(out, "gothicframework/middlewares\"") {
		return out
	}
	lines := strings.Split(out, "\n")
	inImports := false
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "import (" {
			inImports = true
			continue
		}
		if inImports && t == ")" {
			lines = append(lines[:i], append([]string{serverImportLine}, lines[i:]...)...)
			break
		}
	}
	return strings.Join(lines, "\n")
}

// removeImportLine drops the import line referencing pathSuffix from the import block.
func removeImportLine(out, pathSuffix string) string {
	lines := strings.Split(out, "\n")
	res := make([]string, 0, len(lines))
	inImports := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case t == "import (":
			inImports = true
		case inImports && t == ")":
			inImports = false
		case inImports && strings.Contains(ln, pathSuffix):
			continue
		}
		res = append(res, ln)
	}
	return strings.Join(res, "\n")
}
