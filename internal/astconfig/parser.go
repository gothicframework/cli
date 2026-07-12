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
					switch identName(ekv.Key) {
					case "LowResolutionRate":
						if n, ok := intLit(ekv.Value); ok {
							cfg.OptimizeImages.LowResolutionRate = n
						}
					case "Quality":
						if n, ok := intLit(ekv.Value); ok {
							cfg.OptimizeImages.Quality = n
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
	// Deploy is &gothic.DeployConfig{Provider: ..., Providers: gothic.Providers{...}}
	lit, ok := compositeLit(v)
	if !ok {
		return nil, nil
	}
	// Provider defaults to AWS (the zero value) when the field is omitted.
	dc := &cli.DeployConfig{Provider: cli.AWS}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch identName(kv.Key) {
		case "Provider":
			p, err := parseProvider(fset, kv.Value)
			if err != nil {
				return nil, err
			}
			dc.Provider = p
		case "Providers":
			providers, err := parseProviders(fset, kv.Value)
			if err != nil {
				return nil, err
			}
			dc.Providers = providers
		}
	}
	return dc, nil
}

// parseProvider maps a Provider selector (e.g. gothic.AWS) or bare identifier
// (AWS) to the internal cli.Provider. Because there is no type-checker, it keys
// off the trailing identifier NAME. Unknown names are rejected with a clear error.
func parseProvider(fset *token.FileSet, v ast.Expr) (cli.Provider, error) {
	name := providerIdentName(v)
	if name == "" {
		// Non-identifier expression (e.g. a computed value we can't read
		// statically). Fall back to the default provider rather than erroring.
		return cli.AWS, nil
	}
	switch name {
	case "AWS":
		return cli.AWS, nil
	default:
		return cli.AWS, fmt.Errorf("unknown deploy provider %q at %s: valid providers are gothic.AWS", name, fset.Position(v.Pos()))
	}
}

// providerIdentName returns the trailing identifier of a provider expression,
// handling both gothic.AWS (SelectorExpr) and a bare AWS (Ident).
func providerIdentName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// parseProviders walks Providers{AWS: gothic.AWSProvider{...}} and extracts the
// AWS-specific deploy settings (memory, timeout, region, profile, stages).
func parseProviders(fset *token.FileSet, v ast.Expr) (cli.Providers, error) {
	providers := cli.Providers{}
	lit, ok := compositeLit(v)
	if !ok {
		return providers, nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if identName(kv.Key) == "AWS" {
			aws, err := parseAWSProvider(fset, kv.Value)
			if err != nil {
				return providers, err
			}
			providers.AWS = aws
		}
	}
	return providers, nil
}

// parseAWSProvider extracts the AWS deploy settings from a gothic.AWSProvider
// composite literal. This is the former Deploy-level extraction, moved one
// nesting level down.
func parseAWSProvider(fset *token.FileSet, v ast.Expr) (cli.AWSProvider, error) {
	aws := cli.AWSProvider{}
	lit, ok := compositeLit(v)
	if !ok {
		return aws, nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch identName(kv.Key) {
		case "ServerMemory":
			if n, ok := intLit(kv.Value); ok {
				aws.ServerMemory = n
			}
		case "ServerTimeout":
			if n, ok := intLit(kv.Value); ok {
				aws.ServerTimeout = n
			}
		case "Region":
			if s, ok := stringLit(kv.Value); ok {
				aws.Region = s
			}
		case "Profile":
			if s, ok := stringLit(kv.Value); ok {
				aws.Profile = s
			}
		case "Stages":
			stages, err := parseStages(fset, kv.Value)
			if err != nil {
				return aws, err
			}
			aws.Stages = stages
		case "CDN":
			aws.CDN = parseCDN(kv.Value)
		}
	}
	return aws, nil
}

// parseCDN reads a gothic.CDNConfig literal into config.CDNConfig. Each sub-field
// (QueryParams/Cookies/Headers) is one of the gothic.Allow* builder calls
// (AllowAll / AllowNone / Allow(...) / AllowAllExcept(...)). Unrecognized fields
// are ignored, mirroring the rest of the static-literal parse contract.
func parseCDN(v ast.Expr) config.CDNConfig {
	cdn := config.CDNConfig{}
	lit, ok := compositeLit(v)
	if !ok {
		return cdn
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		rule, ok := parseAllowRule(kv.Value)
		if !ok {
			continue
		}
		switch identName(kv.Key) {
		case "QueryParams":
			cdn.QueryParams = rule
		case "Cookies":
			cdn.Cookies = rule
		case "Headers":
			cdn.Headers = rule
		}
	}
	return cdn
}

// parseAllowRule recognizes a gothic.Allow* builder call and reconstructs the
// AllowRule by invoking the real builder with the parsed string-literal args — so
// the config package stays the single source of truth for what each builder means.
// A non-call or unrecognized builder returns ok=false (silently ignored, like a
// dynamic expression elsewhere in the parse contract).
func parseAllowRule(expr ast.Expr) (config.AllowRule, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return config.AllowRule{}, false
	}
	switch selectorName(call.Fun) {
	case "AllowAll":
		return config.AllowAll(), true
	case "AllowNone":
		return config.AllowNone(), true
	case "Allow":
		return config.Allow(stringArgs(call)...), true
	case "AllowAllExcept":
		return config.AllowAllExcept(stringArgs(call)...), true
	default:
		return config.AllowRule{}, false
	}
}

// stringArgs collects every string-literal argument of a call, skipping any
// non-literal (dynamic) argument — consistent with the static-only parse contract.
func stringArgs(call *ast.CallExpr) []string {
	var out []string
	for _, a := range call.Args {
		if s, ok := stringLit(a); ok {
			out = append(out, s)
		}
	}
	return out
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
