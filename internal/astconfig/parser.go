// Package astconfig reads gothic.config.go via go/parser (no type checker) and
// produces the same cli.Config struct that v2 loaded from gothic-config.json.
//
// The parser only understands static composite literals: string literals,
// integer literals, nested struct literals, map literals, and the three ENV
// builder calls (gothic.Env, gothic.SSMParam, gothic.SecretsManager). Any
// unrecognized top-level field or non-literal expression (e.g. os.Getenv(...))
// is silently ignored. The only hard error is an unknown ENV builder function.
//
// GoModName is NOT read from gothic.config.go; it is read from go.mod in
// projectRoot via golang.org/x/mod/modfile.
package astconfig

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/mod/modfile"

	cli "github.com/gothicframework/cli/v3/internal/cli"
	config "github.com/gothicframework/core/config"
)

// init wires Parse into pkg/cli.GetConfig via the ConfigParser indirection,
// avoiding the import cycle that a direct cli -> astconfig edge would create.
func init() {
	cli.ConfigParser = Parse
}

// Parse reads gothic.config.go from projectRoot and returns the parsed config.
func Parse(projectRoot string) (*cli.Config, error) {
	configPath := filepath.Join(projectRoot, "gothic.config.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", configPath, err)
	}

	cfg := &cli.Config{}

	lit := findConfigLiteral(file)
	if lit == nil {
		return nil, fmt.Errorf("no `var Config = ...{}` composite literal found in %s", configPath)
	}

	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key := identName(kv.Key)
		switch key {
		case "ProjectName":
			if s, ok := stringLit(kv.Value); ok {
				cfg.ProjectName = s
			}
		case "TofuBinaryPath":
			if s, ok := stringLit(kv.Value); ok {
				cfg.TofuBinaryPath = s
			}
		case "DockerfilePath":
			if s, ok := stringLit(kv.Value); ok {
				cfg.DockerfilePath = s
			}
		case "WasmBinary":
			if s, ok := stringLit(kv.Value); ok {
				cfg.WasmBinary = s
			}
		case "TailwindBinary":
			if s, ok := stringLit(kv.Value); ok {
				cfg.TailwindBinary = s
			}
		case "OptimizeImages":
			if oi, ok := compositeLit(kv.Value); ok {
				for _, e := range oi.Elts {
					ekv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if identName(ekv.Key) == "LowResolutionRate" {
						if n, ok := intLit(ekv.Value); ok {
							cfg.OptimizeImages.LowResolutionRate = n
						}
					}
				}
			}
		case "Deploy":
			dc, err := parseDeploy(fset, kv.Value)
			if err != nil {
				return nil, err
			}
			cfg.Deploy = dc
		}
	}

	// GoModName from go.mod
	goModPath := filepath.Join(projectRoot, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return nil, fmt.Errorf("error reading %s: %w", goModPath, err)
	}
	mf, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", goModPath, err)
	}
	if mf.Module != nil {
		cfg.GoModName = mf.Module.Mod.Path
	}

	return cfg, nil
}

func parseDeploy(fset *token.FileSet, v ast.Expr) (*cli.DeployConfig, error) {
	// Deploy is &gothic.DeployConfig{...}
	lit, ok := compositeLit(v)
	if !ok {
		return nil, nil
	}
	dc := &cli.DeployConfig{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch identName(kv.Key) {
		case "ServerMemory":
			if n, ok := intLit(kv.Value); ok {
				dc.ServerMemory = n
			}
		case "ServerTimeout":
			if n, ok := intLit(kv.Value); ok {
				dc.ServerTimeout = n
			}
		case "Region":
			if s, ok := stringLit(kv.Value); ok {
				dc.Region = s
			}
		case "Profile":
			if s, ok := stringLit(kv.Value); ok {
				dc.Profile = s
			}
		case "CustomDomain":
			if b, ok := boolLit(kv.Value); ok {
				dc.CustomDomain = b
			}
		case "Stages":
			stages, err := parseStages(fset, kv.Value)
			if err != nil {
				return nil, err
			}
			dc.Stages = stages
		}
	}
	return dc, nil
}

func parseStages(fset *token.FileSet, v ast.Expr) (map[string]cli.EnvVariables, error) {
	lit, ok := compositeLit(v)
	if !ok {
		return nil, nil
	}
	stages := map[string]cli.EnvVariables{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		stageName, ok := stringLit(kv.Key)
		if !ok {
			continue
		}
		ev, err := parseStage(fset, kv.Value)
		if err != nil {
			return nil, err
		}
		stages[stageName] = ev
	}
	return stages, nil
}

func parseStage(fset *token.FileSet, v ast.Expr) (cli.EnvVariables, error) {
	ev := cli.EnvVariables{}
	lit, ok := compositeLit(v)
	if !ok {
		return ev, nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch identName(kv.Key) {
		case "HostedZoneId":
			if val, ok, err := parseEnvValue(fset, kv.Value); err != nil {
				return ev, err
			} else if ok {
				ev.HostedZoneId = &val
			}
		case "CustomDomain":
			if val, ok, err := parseEnvValue(fset, kv.Value); err != nil {
				return ev, err
			} else if ok {
				ev.CustomDomain = &val
			}
		case "CertificateArn":
			if val, ok, err := parseEnvValue(fset, kv.Value); err != nil {
				return ev, err
			} else if ok {
				ev.CertificateArn = &val
			}
		case "WafArn":
			if val, ok, err := parseEnvValue(fset, kv.Value); err != nil {
				return ev, err
			} else if ok {
				ev.WafArn = &val
			}
		case "ENV":
			env, err := parseEnv(fset, kv.Value)
			if err != nil {
				return ev, err
			}
			ev.ENV = env
		}
	}
	return ev, nil
}

func parseEnv(fset *token.FileSet, v ast.Expr) (map[string]config.EnvValue, error) {
	lit, ok := compositeLit(v)
	if !ok {
		return nil, nil
	}
	env := map[string]config.EnvValue{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		envKey, ok := stringLit(kv.Key)
		if !ok {
			continue
		}
		val, ok, err := parseEnvValue(fset, kv.Value)
		if err != nil {
			return nil, err
		}
		if ok {
			env[envKey] = val
		}
	}
	return env, nil
}

// parseEnvValue interprets a value expression as a source-aware EnvValue by
// recognizing the gothic.Env / gothic.SSMParam / gothic.SecretsManager builder
// calls (used for ENV entries and for the per-stage domain fields alike). It
// returns (value, true, nil) for a recognized builder; (_, false, nil) when the
// expression is not a builder call we understand — a dynamic expression such as a
// bare identifier or os.Getenv("X") — so the caller silently ignores it; and
// (_, false, err) for a call to an unknown builder.
func parseEnvValue(fset *token.FileSet, expr ast.Expr) (config.EnvValue, bool, error) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return config.EnvValue{}, false, nil
	}
	name := selectorName(call.Fun)
	if name == "" {
		return config.EnvValue{}, false, nil
	}
	// Chained .Get("json-key") on a builder, e.g.
	// gothic.SecretsManager("/path").Get("secret-key"): parse the receiver as the
	// base EnvValue, then fold in the JSON key. call.Fun is a SelectorExpr whose X
	// is the inner builder call.
	if name == "Get" {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return config.EnvValue{}, false, nil
		}
		base, recognized, err := parseEnvValue(fset, sel.X)
		if err != nil || !recognized {
			return config.EnvValue{}, recognized, err
		}
		if len(call.Args) == 0 {
			return config.EnvValue{}, false, nil
		}
		key, ok := stringLit(call.Args[0])
		if !ok {
			return config.EnvValue{}, false, nil
		}
		base.JSONKey = key
		return base, true, nil
	}
	var arg string
	if len(call.Args) > 0 {
		s, ok := stringLit(call.Args[0])
		if !ok {
			return config.EnvValue{}, false, nil
		}
		arg = s
	}
	switch name {
	case "Env":
		return config.EnvValue{Source: config.RawEnv, Value: arg}, true, nil
	case "SSMParam":
		return config.EnvValue{Source: config.SSMParamEnv, Value: arg}, true, nil
	case "SecretsManager":
		return config.EnvValue{Source: config.SecretsManagerEnv, Value: arg}, true, nil
	default:
		return config.EnvValue{}, false, fmt.Errorf("unknown builder %q at %s: valid builders are gothic.Env, gothic.SSMParam, gothic.SecretsManager", name, fset.Position(call.Pos()))
	}
}

// HasHook reports whether gothic.config.go declares a top-level function with
// the given name (e.g. "BeforeDeploy", "AfterDeploy").
func HasHook(projectRoot, hookName string) (bool, error) {
	configPath := filepath.Join(projectRoot, "gothic.config.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		return false, fmt.Errorf("error parsing %s: %w", configPath, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil && fn.Name != nil && fn.Name.Name == hookName {
			return true, nil
		}
	}
	return false, nil
}

// --- AST helpers ---

func findConfigLiteral(file *ast.File) *ast.CompositeLit {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			if vs.Names[0].Name != "Config" {
				continue
			}
			if lit, ok := compositeLit(vs.Values[0]); ok {
				return lit
			}
		}
	}
	return nil
}

// compositeLit unwraps an optional unary & (e.g. &gothic.DeployConfig{...}) and
// returns the underlying *ast.CompositeLit.
func compositeLit(e ast.Expr) (*ast.CompositeLit, bool) {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return v, true
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return compositeLit(v.X)
		}
	}
	return nil, false
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// selectorName returns the trailing identifier of a call's Fun, handling both
// `gothic.Env` (SelectorExpr) and bare `Env` (Ident).
func selectorName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func boolLit(e ast.Expr) (bool, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false, false
	}
	switch id.Name {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func intLit(e ast.Expr) (int, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.INT {
		return 0, false
	}
	n64, err := strconv.ParseInt(bl.Value, 0, 64)
	n := int(n64)
	if err != nil {
		return 0, false
	}
	return n, true
}
