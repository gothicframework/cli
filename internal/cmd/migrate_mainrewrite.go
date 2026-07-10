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
		return false, "", nil // not the recognized shape — leave main.go alone
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

// toRuntimeLiteral converts an old gothicRoutes.AppConfig{...} literal into the
// equivalent gothic.RuntimeConfig{...} literal (AppConfig and RuntimeConfig are the
// same type via aliases; only the package-qualified names differ).
func toRuntimeLiteral(fset *token.FileSet, src []byte, appCfg *ast.CompositeLit, routesAlias string) string {
	lit := nodeText(fset, src, appCfg)
	lit = strings.ReplaceAll(lit, routesAlias+".", "gothic.")
	return strings.Replace(lit, "gothic.AppConfig", "gothic.RuntimeConfig", 1)
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
