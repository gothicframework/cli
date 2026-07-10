package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/cli/v3/internal/scaffold"
	"github.com/gothicframework/cli/v3/internal/astconfig"
)

func runInitInDir(t *testing.T, dir string) error {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(orig)

	cli := gothic_cli.NewCli()
	cliData := data.DefaultCLIData
	cliData.ProjectName = "test-project"
	cliData.GoModName = "testmod"

	cmd := &InitCommand{cli: &cli, gothicCliData: cliData}
	return cmd.initializeProject()
}

func TestInitCreatesAllTemplateFiles(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	// As of v3 (OpenTofu migration) the SAM/Dockerfile deployment templates are
	// gone — the .tf.json stack files and provider Dockerfiles are embedded in
	// the CLI binary and never seeded to the user's project tree. These four
	// WASM/routes templates have shipped embedded since v2.17 for the same
	// reason (historic on-disk drift caused silent breakage).
	mustNotExist := []string{
		".gothicCli/templates/Dockerfile-template",
		".gothicCli/templates/samconfig-template.toml",
		".gothicCli/templates/sam-template.yaml",
		".gothicCli/templates/wasm/topic_gen.go",
		".gothicCli/templates/wasm/wasm_page_main.go",
		".gothicCli/templates/wasm/wasm_topic_manager_main.go",
		".gothicCli/templates/routes_gen.go",
	}
	for _, f := range mustNotExist {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("template %s should be embedded, not written to disk", f)
		}
	}
}

func TestInitCreatesSrcStructure(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	expected := []string{
		"src/routes/routes_gen.go",
		"src/pages/index.templ",
		"src/pages/revalidate.templ",
		"src/layouts/layout.templ",
		"src/components/helloWorld.templ",
		"src/components/lazyLoad.templ",
		"src/css/app.css",
		"src/api/helloWorld.go",
	}
	for _, f := range expected {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing src file: %s", f)
		}
	}
}

func TestInitCreatesPublicAssets(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	expected := []string{
		"public/imageExample/blurred.png",
		"public/imageExample/original.png",
		"public/favicon.ico",
		"public/styles.css",
	}
	for _, f := range expected {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing public asset: %s", f)
		}
	}
}

func TestInitCreatesRootFiles(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	expected := []string{
		"main.go",
		".env",
		".gitignore",
		"gothic.config.go",
		"makefile",
		"tailwind.config.js",
	}
	for _, f := range expected {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing root file: %s", f)
		}
	}
}

func TestInitEmitsGothicConfigGo(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	// The v2 JSON config must be gone; the v3 Go config must be present.
	if _, err := os.Stat(filepath.Join(dir, "gothic-config.json")); err == nil {
		t.Errorf("gothic-config.json should NOT be emitted by v3 init")
	}
	cfgPath := filepath.Join(dir, "gothic.config.go")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read gothic.config.go: %v", err)
	}
	cfg := string(cfgBytes)
	if !strings.Contains(cfg, `ProjectName: "test-project"`) {
		t.Errorf("gothic.config.go missing rendered ProjectName")
	}
	if !strings.Contains(cfg, "BeforeDeploy") || !strings.Contains(cfg, "AfterDeploy") {
		t.Errorf("gothic.config.go missing commented BeforeDeploy/AfterDeploy stubs")
	}

	// The rendered config must parse through the AST parser. astconfig.Parse
	// reads go.mod for GoModName, so seed a minimal module file.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/testproject\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	parsed, err := astconfig.Parse(dir)
	if err != nil {
		t.Fatalf("astconfig.Parse on emitted gothic.config.go: %v", err)
	}
	if parsed.ProjectName != "test-project" {
		t.Errorf("parsed ProjectName = %q, want %q", parsed.ProjectName, "test-project")
	}
}

func TestInitGitignoreV3Entries(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	content := string(gi)
	for _, want := range []string{".gothicCli/bin/", "infra/.tofu/"} {
		if !strings.Contains(content, want) {
			t.Errorf(".gitignore missing v3 entry %q", want)
		}
	}
	for _, forbidden := range []string{"template.yaml", "samconfig.toml", "\nDockerfile"} {
		if strings.Contains(content, forbidden) {
			t.Errorf(".gitignore still contains stale SAM entry %q", forbidden)
		}
	}
}

func TestInitSubstitutesGoModName(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("initializeProject: %v", err)
	}

	mainGo, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainGo), "testmod") {
		t.Errorf("main.go does not contain go module name 'testmod'")
	}
}

func TestInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runInitInDir(t, dir); err != nil {
		t.Fatalf("second run (idempotent): %v", err)
	}
}
