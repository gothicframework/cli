package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	helpers    "github.com/gothicframework/core/render"
	buildtools "github.com/gothicframework/cli/v3/internal/buildtools"
	proxy      "github.com/gothicframework/cli/v3/internal/proxy"
	routes     "github.com/gothicframework/core/router"
	wasmhelper "github.com/gothicframework/cli/v3/internal/build"
)

// ConfigParser is the indirection that lets pkg/cli call into
// pkg/helpers/astconfig.Parse without creating an import cycle (astconfig
// imports cli for the Config type, so cli must not import astconfig directly).
// It is registered by astconfig's init() once that package is loaded by the
// command layer. astconfig.Parse(projectRoot) populates it.
var ConfigParser func(projectRoot string) (*Config, error)

// BinaryManager resolves the OpenTofu binary path (config override, on-disk
// cache, or download). The concrete implementation lives in
// pkg/helpers/tofu, which already imports pkg/cli for the Config type — so to
// avoid an import cycle the interface is declared here and the constructor is
// injected via NewBinaryManager below, mirroring the ConfigParser indirection.
type BinaryManager interface {
	EnsureBinary(ctx context.Context) (string, error)
}

// NewBinaryManager is registered by pkg/helpers/tofu's init() (loaded by the
// command layer). It builds a BinaryManager bound to the resolved Config.
var NewBinaryManager func(c *Config) BinaryManager

// DeploymentEngine mirrors tofu.DeploymentEngine. It is declared here (rather
// than imported) so pkg/cli does not import pkg/helpers/tofu — which would be an
// import cycle, since tofu imports pkg/cli for the Config type. The concrete
// implementation is constructed via the injected NewDeploymentEngine below.
type DeploymentEngine interface {
	Prepare(ctx context.Context, stage string) error
	Build(ctx context.Context, tag string) (string, error)
	Deploy(ctx context.Context) (map[string]string, error)
	Destroy(ctx context.Context) error
}

// CDNEngine mirrors tofu.CDNEngine (see DeploymentEngine for the cycle rationale).
type CDNEngine interface {
	SyncAssets(ctx context.Context, bucketName string, localDir string) error
	RemoveAssets(ctx context.Context, bucketName string) error
	InvalidateCache(ctx context.Context, distributionID string) error
}

// NewDeploymentEngine and NewCDNEngine are registered by pkg/helpers/tofu's
// init(). They construct the AWS-backed engine and CDN from the resolved config
// and a loaded aws.Config. The aws.Config is passed as `any` to keep the
// aws-sdk type out of this interface boundary; the tofu package type-asserts it.
var NewDeploymentEngine func(c *Config, awsCfg any) (DeploymentEngine, error)
var NewCDNEngine func(c *Config, awsCfg any) CDNEngine

// commandRunner is the DI seam that lets tests intercept Go-toolchain
// invocations made by InitializeModule without shelling out for real.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// cliRunner is overridable in tests to avoid invoking the real Go toolchain.
var cliRunner commandRunner = execRunner{}

type GothicCli struct {
	config  *Config
	Runtime string

	Templates       helpers.TemplateHelper
	Tailwind        buildtools.TailwindHelper
	Templ           buildtools.TemplHelper
	Logger          *slog.Logger
	FileBasedRouter routes.FileBasedRouteHelper
	Proxy           proxy.ProxyHelper
	Wasm            wasmhelper.WasmHelper
	BinaryManager   BinaryManager
	Engine          DeploymentEngine
	CDN             CDNEngine
}

type CliCommands struct {
	Init            *bool
	Build           *string
	Deploy          *bool
	Help            *bool
	ImgOptimization *bool
	HotReload       *bool
	DeployAction    *string
	DeployStage     *string
}

func NewCli() GothicCli {
	cli := GothicCli{
		Runtime:         runtime.GOOS,
		Templates:       helpers.NewTemplateHelper(),
		Tailwind:        buildtools.NewTailwindHelper(runtime.GOOS, runtime.GOARCH),
		Templ:           buildtools.NewTemplHelper(),
		Logger:          helpers.NewLogger("error", false, os.Stdout),
		FileBasedRouter: routes.NewFileBasedRouteHelper(),
		Proxy:           proxy.NewProxyHelper(),
		Wasm:            wasmhelper.NewWasmHelper(runtime.GOOS, runtime.GOARCH),
	}

	return cli
}

func (cli *GothicCli) GetConfig() (Config, error) {
	if cli.config != nil {
		return *cli.config, nil
	}

	const goConfig = "gothic.config.go"
	const jsonConfig = "gothic-config.json"

	_, goErr := os.Stat(goConfig)
	hasGo := goErr == nil

	if !hasGo {
		if _, err := os.Stat(jsonConfig); err == nil {
			return Config{}, errors.New("Found gothic-config.json (v2 format). Run `gothic migrate-v3` to generate gothic.config.go from your existing config.")
		}
		return Config{}, errors.New("No config file found. Run `gothic migrate-v3` to migrate from gothic-config.json, or `gothic init` to start fresh.")
	}

	if ConfigParser == nil {
		return Config{}, errors.New("internal error: gothic.config.go parser not registered (missing import of pkg/helpers/astconfig)")
	}

	parsed, err := ConfigParser(".")
	if err != nil {
		return Config{}, err
	}
	config := *parsed

	if config.TailwindBinary != "" {
		cli.Tailwind.ConfigOverride = config.TailwindBinary
	}
	if config.WasmTinyGoVersion != "" {
		cli.Wasm.Version = config.WasmTinyGoVersion
	}
	if config.WasmBinary != "" {
		cli.Wasm.ConfigOverride = config.WasmBinary
	}
	cli.config = &config
	if NewBinaryManager != nil {
		cli.BinaryManager = NewBinaryManager(cli.config)
	}

	// Wire the deployment engine + CDN only when a Deploy block is present. The
	// AWS config (shared profile + region) is loaded once here so deploy-time
	// code paths need not reload it.
	if config.Deploy != nil && NewDeploymentEngine != nil && NewCDNEngine != nil {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(config.Deploy.Region),
			awsconfig.WithSharedConfigProfile(config.Deploy.Profile),
		)
		if err != nil {
			return Config{}, fmt.Errorf("loading AWS config: %w", err)
		}
		engine, err := NewDeploymentEngine(cli.config, awsCfg)
		if err != nil {
			return Config{}, fmt.Errorf("constructing deployment engine: %w", err)
		}
		cli.Engine = engine
		cli.CDN = NewCDNEngine(cli.config, awsCfg)
	}
	return config, nil
}

// FrameworkModule is a published Gothic library together with the version that
// `gothic init` pins it to in a freshly scaffolded project.
type FrameworkModule struct {
	Path    string
	Version string
}

// InitializeModule runs `go mod init` for a new project and pins each framework
// library to the version the scaffolding CLI ships with.
//
// Each pin is a per-module version (core / components / middlewares version
// independently), written with `go mod edit -require` — a purely textual, offline
// edit — so a fresh project records the intended versions even before those
// libraries are published to the module proxy.
//
// It deliberately does NOT run `go mod tidy` here: at init time the generated
// sub-packages (src/layouts, src/pages, …) have no `.go` files yet (templ + route
// codegen runs afterwards), so a tidy would fail to resolve them and leave an
// empty go.sum. `gothic init` calls TidyModule AFTER all codegen instead.
//
// This is the ONLY place the CLI writes framework versions into a user's go.mod.
// No other command rewrites them, so a project may bump any framework library
// independently after `init`.
func (cli *GothicCli) InitializeModule(goModuleName string, modules []FrameworkModule) error {
	ctx := context.Background()
	if _, err := cliRunner.Run(ctx, "go", "mod", "init", goModuleName); err != nil {
		return fmt.Errorf("error running go mod init: %w", err)
	}
	for _, m := range modules {
		if m.Path == "" || m.Version == "" {
			continue
		}
		if _, err := cliRunner.Run(ctx, "go", "mod", "edit", "-require="+m.Path+"@"+m.Version); err != nil {
			return fmt.Errorf("error pinning %s@%s: %w", m.Path, m.Version, err)
		}
	}
	return nil
}

// TidyModule runs `go mod tidy` in the project root to resolve every dependency
// (framework libraries + templ/chi/godotenv) and populate go.sum. It MUST run
// AFTER the templ + route codegen so all generated sub-packages resolve; running
// it before (e.g. inside InitializeModule) fails and leaves an empty go.sum.
//
// Best-effort: it warns rather than aborts when the framework versions aren't yet
// published to the registry (dev/pre-publish), so `gothic init` still completes.
func (cli *GothicCli) TidyModule() error {
	ctx := context.Background()
	if _, err := cliRunner.Run(ctx, "go", "mod", "tidy"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: go mod tidy failed — run it manually after the framework packages are available on the registry.")
	}
	return nil
}
