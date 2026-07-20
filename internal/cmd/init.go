/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/template"

	gothci_cli "github.com/gothicframework/cli/v3/internal/cli"
	cli_data "github.com/gothicframework/cli/v3/internal/scaffold"
	helpers "github.com/gothicframework/core/render"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var initCmd = &cobra.Command{
	Use:   "init [module-path]",
	Short: "Initialize the project structure and configuration files for a Gothic app.",
	Long: `Sets up the initial folder structure and essential files required to start building a Gothic app.

If you pass [module-path] (your Go module path, e.g. github.com/you/my-app), init runs
non-interactively: the module is used as-is and the project name is derived from it (the
last path segment, minus any /vN major-version suffix). Omit it to be prompted instead.

This includes:
  - Auto-download of the Tailwind CSS standalone binary (cached in ~/.cache/gothic-cli/bin/)
  - A gothic.config.go file
  - A basic example app to help you get started
  - A link to the official documentation for further guidance`,
	Args: cobra.MaximumNArgs(1),
	RunE: newInitCommand(gothci_cli.NewCli()),
}

func init() {
	rootCmd.AddCommand(initCmd)
}

type InitCommand struct {
	gothicCliData cli_data.GothicCliData
	cli           *gothci_cli.GothicCli

	// gitRunner is an injectable seam for tests; the default runs the real
	// `git` command exactly as the previous inline code did.
	gitRunner func(args ...string) error
}

func NewInitCommandCli(cli *gothci_cli.GothicCli, gothicCliData cli_data.GothicCliData) InitCommand {
	command := InitCommand{
		cli:           cli,
		gothicCliData: gothicCliData,
	}
	command.gitRunner = defaultGitRunner
	return command
}

// defaultGitRunner runs `git <args...>`, mirroring the original
// exec.Command("git", "init").Run() behavior (errors are ignored by the
// caller, just as before).
func defaultGitRunner(args ...string) error {
	return exec.Command("git", args...).Run()
}

func newInitCommand(cli gothci_cli.GothicCli) RunEFunc {
	return func(cmd *cobra.Command, args []string) error {
		command := NewInitCommandCli(&cli, cli_data.DefaultCLIData)

		var modulePath string
		if len(args) == 1 {
			modulePath = args[0]
		}
		return command.CreateNewGothicApp(cli_data.DefaultCLIData, modulePath)
	}
}

// CreateNewGothicApp scaffolds a new Gothic project. modulePath is the Go module
// path: when non-empty (passed as the positional `init [module-path]` arg) init
// runs non-interactively; when empty it is prompted for. The project name is
// derived from the module path (see deriveProjectName), falling back to an
// interactive prompt only when the module can't be reduced to a valid name.
func (command *InitCommand) CreateNewGothicApp(data cli_data.GothicCliData, modulePath string) error {

	gomodName := modulePath
	if gomodName == "" {
		var err error
		gomodName, err = command.promptForGoModName()
		if err != nil {
			return err
		}
	}
	data.GoModName = gomodName

	// Derive the human-readable project name from the module path. Uniqueness of
	// final resource names comes from the full module path baked into the CRC
	// suffix, so deriving the prefix here is collision-safe. Fall back to the
	// prompt only when the module can't be sanitized to a valid kebab name.
	projectName, ok := deriveProjectName(gomodName)
	if ok {
		fmt.Printf("Project name (from module): %s\n", projectName)
	} else {
		var err error
		projectName, err = command.promptForProjectName()
		if err != nil {
			return err
		}
	}
	data.ProjectName = projectName
	command.gothicCliData = data

	if err := command.initializeProject(); err != nil {
		return err
	}
	// Pre-cache the Tailwind binary during init
	if _, err := command.cli.Tailwind.EnsureBinary(); err != nil {
		return fmt.Errorf("error downloading tailwind binary: %w", err)
	}
	if err := command.cli.InitializeModule(command.gothicCliData.GoModName, FrameworkModules); err != nil {
		return err
	}
	if err := command.cli.Templ.Render(); err != nil {
		return err
	}

	if err := command.cli.FileBasedRouter.Render(gomodName); err != nil {
		return err
	}

	// Keep gothic_embed.go in sync with the scaffolded static-files mode. A fresh
	// project defaults to CDN, so this normally just ensures no stale
	// embed file lingers; it becomes load-bearing only if the config opts into
	// EMBEDDED. GetConfig here only AST-parses gothic.config.go (+ reads go.mod),
	// so it is safe to call before TidyModule populates go.sum.
	if cfg, err := command.cli.GetConfig(); err != nil {
		return err
	} else if err := syncEmbeddedPublicFile(&cfg); err != nil {
		return err
	}

	// Now that all sub-packages have their generated .go files (templ + routes),
	// resolve dependencies and populate go.sum. This must run AFTER codegen — a
	// tidy during InitializeModule would fail on the not-yet-generated packages
	// and leave an empty go.sum.
	if err := command.cli.TidyModule(); err != nil {
		return err
	}

	gitRunner := command.gitRunner
	if gitRunner == nil {
		gitRunner = defaultGitRunner
	}
	gitRunner("init")
	fmt.Println("Project initialized successfully!")
	return nil
}

// Function to create directories and files
func (command *InitCommand) initializeProject() error {

	command.cli.Templates.InitCmdTemplateInfo = helpers.InitCmdTemplateInfo{
		ProjectName:            command.gothicCliData.ProjectName,
		GoModName:              command.gothicCliData.GoModName,
		MainServerPackageName:  "package main",
		MainServerFunctionName: "main()",
	}

	if err := command.createInitialDirs(); err != nil {
		return err
	}
	// Create dot files (embed api wont let dots on files)
	if err := command.createHiddenFiles(); err != nil {
		return err
	}

	// Create initial file structure
	if err := command.createInitialFileStructure(); err != nil {
		return err
	}
	// create all custom template files
	if err := command.createTemplateBasedFiles(); err != nil {
		return err
	}
	return nil
}

func (command *InitCommand) createInitialDirs() error {
	for _, dir := range command.gothicCliData.InitialDirs {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return fmt.Errorf("error generating initial Directories: %v", err)
		}
	}
	return nil
}

func (command *InitCommand) createHiddenFiles() error {

	g := new(errgroup.Group)

	g.Go(func() error {
		return os.WriteFile(".env", []byte(command.gothicCliData.Env), 0644)
	})

	g.Go(func() error {
		return os.WriteFile(".gitignore", []byte(command.gothicCliData.GitIgnore), 0644)
	})

	return g.Wait()

}

func (command *InitCommand) createInitialFileStructure() error {
	mainServerData, err := fs.ReadFile(command.gothicCliData.ServerFolder, "server/server.go")
	if err != nil {
		return fmt.Errorf("error reading embedded server template: %w", err)
	}
	if err := os.WriteFile("main.go", mainServerData, 0644); err != nil {
		return fmt.Errorf("error creating file %s: %w", "main.go", err)
	}
	command.cli.Templates.UpdateFromTemplate("main.go", "main.go", command.cli.Templates.InitCmdTemplateInfo)

	if err := command.writeGothicConfig(); err != nil {
		return err
	}

	g := new(errgroup.Group)

	for filename, fileContent := range command.gothicCliData.InitialFiles {
		g.Go(func() error {
			if err := command.cli.Templates.CreateFromTemplate(fileContent, filename, filename, command.cli.Templates.InitCmdTemplateInfo); err != nil {
				return fmt.Errorf("error creating file %s: %w", filename, err)
			}
			return nil
		})
	}

	for filename, fileContent := range command.gothicCliData.TemplateFiles {
		g.Go(func() error {
			if err := command.cli.Templates.CopyFromFs(fileContent, filename, filename); err != nil {
				return fmt.Errorf("error copying file %s: %w", filename, err)
			}
			return nil
		})
	}

	for filename, fileContent := range command.gothicCliData.PublicFolderAssets {
		g.Go(func() error {
			data, err := fs.ReadFile(fileContent, filename)
			if err != nil {
				return fmt.Errorf("error reading embedded asset %s: %w", filename, err)
			}

			if err := os.WriteFile(filename, data, 0644); err != nil {
				return fmt.Errorf("error creating file %s: %w", filename, err)
			}
			return nil
		})
	}

	// The shared gothic-core.js runtime and the prebuilt full-Go static
	// core are NO LONGER seeded into public/. They are served straight
	// from the framework embed via the /_gothic/ route (see pkg/helpers/runtimeassets
	// and pkg/server), so the layout's <head> references resolve without any files
	// being copied into the project tree.
	return g.Wait()
}

// writeGothicConfig renders the embedded gothic.config.go.tmpl into the
// project's gothic.config.go, replacing the v2 gothic-config.json scaffold.
func (command *InitCommand) writeGothicConfig() error {
	tmpl, err := template.New("gothic.config.go").Parse(string(cli_data.GothicConfigGoTemplate))
	if err != nil {
		return fmt.Errorf("error parsing gothic.config.go template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		ProjectName string
		GoModName   string
	}{
		ProjectName: command.gothicCliData.ProjectName,
		GoModName:   command.gothicCliData.GoModName,
	}); err != nil {
		return fmt.Errorf("error rendering gothic.config.go: %w", err)
	}

	if err := os.WriteFile("gothic.config.go", buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("error creating file gothic.config.go: %w", err)
	}
	return nil
}

func (command *InitCommand) createTemplateBasedFiles() error {
	g := new(errgroup.Group)

	// Pages
	for templateFilePath, pageName := range command.gothicCliData.CustomTemplateBasedPages {
		g.Go(func() error {
			if err := command.cli.Templates.CreateFromTemplate(command.gothicCliData.SrcFolder, templateFilePath, templateFilePath, helpers.RouteTemplateInfo{PageName: pageName, GoModName: command.gothicCliData.GoModName}); err != nil {
				return fmt.Errorf("error creating page file %s: %w", templateFilePath, err)
			}
			return nil
		})
	}

	// Components
	for templateFilePath, componentName := range command.gothicCliData.CustomTemplateBasedComponents {
		g.Go(func() error {
			if err := command.cli.Templates.CreateFromTemplate(command.gothicCliData.SrcFolder, templateFilePath, templateFilePath, helpers.RouteTemplateInfo{ComponentName: componentName, GoModName: command.gothicCliData.GoModName}); err != nil {
				return fmt.Errorf("error creating component file %s: %w", templateFilePath, err)
			}
			return nil
		})
	}

	// API Routes
	for templateFilePath, routeName := range command.gothicCliData.CustomTemplateBasedRoutes {
		g.Go(func() error {
			if err := command.cli.Templates.CreateFromTemplate(command.gothicCliData.SrcFolder, templateFilePath, templateFilePath, helpers.RouteTemplateInfo{RouteName: routeName, GoModName: command.gothicCliData.GoModName}); err != nil {
				return fmt.Errorf("error creating api route file %s: %w", templateFilePath, err)
			}
			return nil
		})
	}

	return g.Wait()
}

// kebabNameRegexp is the single source of truth for valid project names:
// lowercase letters/numbers grouped by single dashes (kebab case).
var kebabNameRegexp = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// isValidKebabName reports whether name is a valid kebab-case project name.
func isValidKebabName(name string) bool {
	return kebabNameRegexp.MatchString(name)
}

// majorVersionSuffixRegexp matches a Go module major-version path element such
// as "v2" or "v3" (see https://go.dev/ref/mod#module-path).
var majorVersionSuffixRegexp = regexp.MustCompile(`^v[0-9]+$`)

// dashRunRegexp collapses runs of dashes produced while sanitizing.
var dashRunRegexp = regexp.MustCompile(`-+`)

// deriveProjectName turns a Go module path into a kebab-case project name. It
// takes the last path segment — but skips a Go major-version suffix like /v2 or
// /v3 in favor of the preceding segment (so github.com/you/my-app/v3 yields
// "my-app", not "v3"). The chosen segment is lowercased, every character outside
// [a-z0-9-] is replaced with a dash, dash runs are collapsed, and leading/
// trailing dashes are trimmed. It returns ok=false when the result is empty or
// fails the same kebab validation the interactive prompt enforces, so callers
// can fall back to prompting.
func deriveProjectName(modulePath string) (name string, ok bool) {
	segments := strings.Split(modulePath, "/")
	// Drop trailing empty segments (e.g. a stray trailing slash).
	for len(segments) > 0 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return "", false
	}

	last := segments[len(segments)-1]
	if majorVersionSuffixRegexp.MatchString(last) && len(segments) >= 2 {
		last = segments[len(segments)-2]
	}

	name = sanitizeToKebab(last)
	if name == "" || !isValidKebabName(name) {
		return "", false
	}
	return name, true
}

// sanitizeToKebab lowercases s, replaces every character outside [a-z0-9-] with
// a dash, collapses dash runs, and trims leading/trailing dashes.
func sanitizeToKebab(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(dashRunRegexp.ReplaceAllString(b.String(), "-"), "-")
}

func (command *InitCommand) promptForProjectName() (string, error) {
	var name string
	fmt.Print("Enter your unique stack name in kebab case (e.g., your-unique-stack-name): ")
	fmt.Scanln(&name)

	if name == "" {
		return "", fmt.Errorf("project name cannot be empty")
	}
	// Validate kebab case
	if !isValidKebabName(name) {
		return "", fmt.Errorf("invalid name format. Please use kebab case (lowercase letters and numbers only, with dashes)")
	}
	return name, nil
}

func (command *InitCommand) promptForGoModName() (string, error) {
	var name string
	fmt.Print("Enter your go module name: ")
	fmt.Scanln(&name)
	if name == "" {
		return "", fmt.Errorf("go module name cannot be empty")
	}
	return name, nil
}
