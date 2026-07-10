package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/spf13/cobra"
)

func newTestDeployCommand() DeployCommand {
	cli := gothic_cli.NewCli()
	return newDeployCommandCli(&cli)
}

func TestNewDeployCommandCli(t *testing.T) {
	cmd := newTestDeployCommand()
	if cmd.cli == nil {
		t.Fatal("expected cli set")
	}
	if len(cmd.allowedActions) != 2 {
		t.Errorf("expected 2 allowed actions, got %v", cmd.allowedActions)
	}
}

func TestIsValidAction(t *testing.T) {
	cmd := newTestDeployCommand()
	cases := map[string]bool{
		"deploy":  true,
		"delete":  true,
		"destroy": false,
		"":        false,
		"DEPLOY":  false,
	}
	for action, want := range cases {
		if got := cmd.isValidAction(action); got != want {
			t.Errorf("isValidAction(%q) = %v, want %v", action, got, want)
		}
	}
}

// TestDeployCleanupRemovesFiles and TestDeployCleanupTolerantOfMissingFiles
// covered the v2 SAM cleanup() method which was removed in Phase 6.
// Replacement tests are in Phase 9.

func TestDeployRunEInvalidAction(t *testing.T) {
	chdirTemp(t)
	runE := newDeployCommand(gothic_cli.NewCli())
	c := &cobra.Command{}
	c.Flags().StringP("stage", "s", "dev", "")
	c.Flags().StringP("action", "a", "destroy", "")
	if err := runE(c, nil); err == nil {
		t.Fatal("expected error for invalid action")
	}
}

func TestDeployRunEInvalidStage(t *testing.T) {
	chdirTemp(t)
	runE := newDeployCommand(gothic_cli.NewCli())
	c := &cobra.Command{}
	c.Flags().StringP("stage", "s", "bad stage!", "")
	c.Flags().StringP("action", "a", "deploy", "")
	if err := runE(c, nil); err == nil {
		t.Fatal("expected error for invalid stage name")
	}
}

func TestDeployInvalidStageName(t *testing.T) {
	chdirTemp(t)
	cmd := newTestDeployCommand()
	if err := cmd.Deploy("bad stage!", "deploy"); err == nil {
		t.Fatal("expected error for invalid stage name")
	}
}

// TestDeploySetupSucceeds, TestDeploySetupFailsWithoutDeployConfig,
// TestDeploySetupCustomDomainRequiresFields covered the v2 setup() method
// which was removed in Phase 6. Replacement tests are in Phase 9.

func TestDeployProceedsUntilWasmScan(t *testing.T) {
	bin := writeFakeTailwind(t, true)
	chdirTemp(t)
	scaffoldSrc(t)
	writeConfig(t, `{
		"projectName":"demo","goModuleName":"demo","tailwindBinary":"`+bin+`",
		"deploy":{"region":"us-east-1","profile":"default","stages":{"dev":{}}}
	}`)

	// setup + Templ.Render + Router.Render + Tailwind.Build (fake) all succeed,
	// then Wasm.ScanPages fails outside a real Go module. This exercises the
	// bulk of Deploy() without ever reaching AWS/SAM. cleanup() also runs via
	// the deferred call.
	cmd := newTestDeployCommand()
	err := cmd.Deploy("dev", "deploy")
	if err == nil {
		t.Fatal("expected Deploy to fail at wasm scan stage")
	}
	// Assert the failure originates from the wasm-scan stage specifically, not
	// from some earlier step. Deploy() wraps the scan error with "wasm:".
	if !strings.Contains(err.Error(), "wasm") {
		t.Fatalf("expected wasm-scan error, got: %v", err)
	}
}

// TestDeployRejectsUndeclaredStage locks in that deploying a stage absent from
// gothic.config.go (Deploy.Stages) is refused before any build/AWS work — the
// guard that catches typos like "de" for "dev". The error must name the offending
// stage and list the declared ones.
func TestDeployRejectsUndeclaredStage(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{
		"projectName":"demo","goModuleName":"demo",
		"deploy":{"region":"us-east-1","profile":"default","stages":{"dev":{}}}
	}`)

	cmd := newTestDeployCommand()
	err := cmd.Deploy("de", "deploy") // "de" is not declared; "dev" is
	if err == nil {
		t.Fatal("expected Deploy to reject an undeclared stage")
	}
	if !strings.Contains(err.Error(), `"de"`) || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("error should name the bad stage and list declared ones, got: %v", err)
	}
}

// TestDeploySetupFailsWithBadStageBucketName exercises the bucket-name
// validation path that Deploy() runs at line `originBucketName := ...`.
//
// The bucket name is built as projectName + "-" + stage + "-" + appID. An
// uppercase project name produces an invalid S3 bucket name. In Deploy() this
// validation sits after the wasm/SAM build stages, which require real external
// tooling and so cannot be reached in a unit test. We therefore drive the same
// ValidateBucketName call with an identically-constructed name and assert the
// error is bucket-name-specific.
func TestDeploySetupFailsWithBadStageBucketName(t *testing.T) {
	const projectName = "Demo" // uppercase -> invalid S3 bucket name
	const stage = "dev"
	const appID = "abc123"

	originBucketName := projectName + "-" + stage + "-" + appID
	err := gothic_cli.ValidateBucketName(originBucketName)
	if err == nil {
		t.Fatalf("expected bucket-name validation to fail for %q", originBucketName)
	}
	if !strings.Contains(err.Error(), "bucket name") {
		t.Fatalf("expected a bucket-name-specific error, got: %v", err)
	}
}

func TestDeployFailsWhenNoConfig(t *testing.T) {
	chdirTemp(t) // empty dir: no gothic.config.go, no gothic-config.json
	cmd := newTestDeployCommand()
	err := cmd.Deploy("dev", "deploy")
	if err == nil {
		t.Fatal("expected error when no config file is present")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error = %q, want it to reference the missing config", err.Error())
	}
}

// withStdin replaces os.Stdin with a pipe carrying input for the duration of fn,
// then restores it. Captures stdout produced during fn and returns it.
func withStdinCapture(t *testing.T, input string, fn func()) string {
	t.Helper()
	// stdin pipe
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	// stdout pipe
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()

	go func() {
		_, _ = inW.WriteString(input)
		_ = inW.Close()
	}()

	fn()

	_ = outW.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, outR)
	return buf.String()
}

func newDeleteTestConfig() gothic_cli.Config {
	return gothic_cli.Config{
		ProjectName: "demo",
		GoModName:   "example.com/demo",
		Deploy:      &gothic_cli.DeployConfig{Region: "us-east-1", Profile: "default"},
	}
}

func TestPromptDeleteRemoteStateSkipsOnN(t *testing.T) {
	cli := gothic_cli.NewCli()
	cfg := newDeleteTestConfig()
	out := withStdinCapture(t, "N\n", func() {
		promptDeleteRemoteState(context.Background(), &cli, cfg, "dev", "sfx")
	})
	if !strings.Contains(out, "Skipped") {
		t.Errorf("expected skip message, got: %q", out)
	}
	if !strings.Contains(out, "remote state preserved") {
		t.Errorf("expected preservation message, got: %q", out)
	}
}

func TestPromptDeleteRemoteStateSkipsOnEmpty(t *testing.T) {
	cli := gothic_cli.NewCli()
	cfg := newDeleteTestConfig()
	out := withStdinCapture(t, "\n", func() {
		promptDeleteRemoteState(context.Background(), &cli, cfg, "dev", "sfx")
	})
	if !strings.Contains(out, "Skipped") {
		t.Errorf("default (empty) answer should skip deletion, got: %q", out)
	}
}

func TestPromptDeleteRemoteStatePromptMentionsResources(t *testing.T) {
	cli := gothic_cli.NewCli()
	cfg := newDeleteTestConfig()
	out := withStdinCapture(t, "N\n", func() {
		promptDeleteRemoteState(context.Background(), &cli, cfg, "dev", "sfx")
	})
	// The prompt must name the exact state bucket + lock table so the user knows
	// what is at risk before answering.
	if !strings.Contains(out, "demo-state-sfx") {
		t.Errorf("prompt should name the state bucket, got: %q", out)
	}
	if !strings.Contains(out, "demo-lock-sfx") {
		t.Errorf("prompt should name the lock table, got: %q", out)
	}
}

// TestDeploySetupCustomDomainNonUsEast1RequiresArn covered the v2 setup()
// method removed in Phase 6. Replacement test in Phase 9.
