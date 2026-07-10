/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	gothic_config "github.com/gothicframework/core/config"
	"github.com/gothicframework/cli/v3/internal/astconfig"
	"github.com/gothicframework/cli/v3/internal/deploy"

	"github.com/spf13/cobra"
)

// deployCmd represents the deploy command
var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy or remove the application on AWS using OpenTofu.",
	Long: `This command builds and deploys (or removes) the application using OpenTofu.

During deployment, it performs the following steps:
  - Converts template files into Go source files and builds Tailwind + WASM assets
  - Builds an optimized Docker image tailored for AWS Lambda environments
  - Publishes the image to AWS ECR and uses it as the Lambda runtime
  - Bootstraps the OpenTofu remote state backend (S3 + DynamoDB) on first run
  - Applies the OpenTofu stack (Lambda, S3, CloudFront)
  - Syncs the public/ assets to S3 and invalidates the CloudFront cache

This process ensures your application is efficiently built and deployed to AWS.`,
	RunE: newDeployCommand(gothic_cli.NewCli()),
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().StringP("stage", "s", "dev", "Define AWS stage to deploy or delete")
	deployCmd.Flags().StringP("action", "a", "deploy", "Action to be taken, to deploy or delete the api")
}

type DeployCommand struct {
	cli            *gothic_cli.GothicCli
	allowedActions []string
}

func newDeployCommandCli(cli *gothic_cli.GothicCli) DeployCommand {
	return DeployCommand{
		cli:            cli,
		allowedActions: []string{"delete", "deploy"},
	}
}

func newDeployCommand(cli gothic_cli.GothicCli) RunEFunc {
	return func(cmd *cobra.Command, args []string) error {
		command := newDeployCommandCli(&cli)
		stageFlag, err := cmd.Flags().GetString("stage")
		if err != nil {
			return err
		}
		action, err := cmd.Flags().GetString("action")
		if err != nil {
			return err
		}
		if !command.isValidAction(action) {
			return fmt.Errorf("error: invalid action \"%s\". Allowed values: %v", action, command.allowedActions)
		}

		return command.Deploy(stageFlag, action)
	}
}

func (command *DeployCommand) Deploy(stage string, action string) error {
	if err := gothic_cli.ValidateStageName(stage); err != nil {
		return err
	}

	config, err := command.cli.GetConfig()
	if err != nil {
		return err
	}
	if config.Deploy == nil {
		return fmt.Errorf("Deploy configuration missing in gothic.config.go")
	}

	// A stage must be declared in gothic.config.go (Deploy.Stages). Deploying an
	// undeclared stage is almost always a typo (e.g. "de" instead of "dev") that
	// would silently stand up an entire parallel set of AWS resources under the
	// wrong name. Block it. Delete stays permissive so an orphaned or renamed
	// stage can still be torn down.
	if _, ok := config.Deploy.Stages[stage]; !ok {
		if action == "deploy" {
			return fmt.Errorf("%q is not a stage declared in gothic.config.go (Deploy.Stages).%s", stage, declaredStagesHint(config.Deploy.Stages))
		}
		fmt.Println(paint(clrYellow, fmt.Sprintf("⚠ stage %q is not declared in gothic.config.go — proceeding with teardown anyway.", stage)))
	}

	deployBanner(action, stage)

	// Front-end artifacts are compiled into the server binary and uploaded to S3,
	// so they matter only for a deploy — a teardown needs none of them. Build them
	// first (and only here) so a local build failure aborts before we touch AWS.
	if action == "deploy" {
		deployPhase("Building front-end assets (Templ · Tailwind · WASM)")
		if err := command.cli.Templ.Render(); err != nil {
			return err
		}
		if err := command.cli.FileBasedRouter.Render(config.GoModName); err != nil {
			return err
		}
		if err := command.cli.Tailwind.Build(); err != nil {
			return err
		}
		command.cli.Wasm.PregenerateTopicStubs()
		wasmPages, err := command.cli.Wasm.ScanPages("src/pages", "src/components")
		if err != nil {
			return fmt.Errorf("wasm: scan pages: %w", err)
		}
		if len(wasmPages) > 0 {
			if err := command.cli.Wasm.GenerateAll(wasmPages, "public/wasm"); err != nil {
				return fmt.Errorf("wasm: build: %w", err)
			}
		}
	}

	suffix := tofu.ResourceSuffix(config.GoModName, config.ProjectName)
	bucketName := config.ProjectName + "-" + stage + "-" + suffix
	if err := gothic_cli.ValidateBucketName(bucketName); err != nil {
		return err
	}

	ctx := context.Background()
	engine := command.cli.Engine
	cdn := command.cli.CDN
	if engine == nil || cdn == nil {
		return errors.New("deployment engine not initialized (AWS config failed to load)")
	}

	if err := engine.Prepare(ctx, stage); err != nil {
		return err
	}

	switch action {
	case "deploy":
		// Build the lifecycle-hook context from the resolved config + stage.
		gctx := &gothic_config.GothicContext{
			Stage:       stage,
			ProjectName: config.ProjectName,
			Suffix:      suffix,
			Region:      config.Deploy.Region,
			Outputs:     map[string]string{},
		}
		if stageCfg, ok := config.Deploy.Stages[stage]; ok {
			gctx.Env = stageCfg.ENV
		}
		// BeforeDeploy fires after Prepare but before any image build / apply so a
		// failing hook aborts the deploy without touching ECR or tofu state.
		if gctx, err = runHook(".", "BeforeDeploy", gctx); err != nil {
			return err
		}

		// Build + push the Lambda image — only a deploy needs one; a delete never
		// touches Docker or ECR. The repo name is owned by the engine (computed in
		// Prepare, namespaced per stage), so it is not passed here.
		tag := fmt.Sprintf("%d", time.Now().Unix())
		deployPhase("Building & pushing container image")
		if _, err := engine.Build(ctx, tag); err != nil {
			return err
		}

		// Apply FIRST: the asset S3 bucket (+ Lambda + CloudFront) is created by
		// tofu, so assets can only be uploaded after the bucket exists.
		deployPhase("Provisioning infrastructure")
		outputs, err := engine.Deploy(ctx)
		if err != nil {
			return err
		}
		deployPhase("Uploading static assets to S3")
		if err := cdn.SyncAssets(ctx, bucketName, "public/"); err != nil {
			return fmt.Errorf("error uploading assets to S3: %w", err)
		}
		if distID := outputs["cloudfront_distribution_id"]; distID != "" {
			if err := cdn.InvalidateCache(ctx, distID); err != nil {
				return fmt.Errorf("error invalidating CloudFront cache: %w", err)
			}
		}
		// AfterDeploy receives the stack outputs (CloudFront ID, S3 ARN, etc.).
		gctx.Outputs = outputs
		if _, err := runHook(".", "AfterDeploy", gctx); err != nil {
			return err
		}
		printDeploySummary(stage, outputs)
	case "delete":
		deployPhase("Removing static assets from S3")
		if err := cdn.RemoveAssets(ctx, bucketName); err != nil {
			return err
		}
		deployPhase("Destroying infrastructure")
		if err := engine.Destroy(ctx); err != nil {
			return err
		}
		promptDeleteRemoteState(ctx, command.cli, config, stage, suffix)
		printDeleteSummary(stage)
	}
	return nil
}

// declaredStagesHint renders the " Declared stages: a, b, c." suffix (or a hint
// to add one when none exist) appended to the invalid-stage error message.
func declaredStagesHint(stages map[string]gothic_cli.EnvVariables) string {
	if len(stages) == 0 {
		return " No stages are declared — add one under Deploy.Stages first."
	}
	names := make([]string, 0, len(stages))
	for s := range stages {
		names = append(names, s)
	}
	sort.Strings(names)
	return " Declared stages: " + strings.Join(names, ", ") + "."
}

// runHook runs a lifecycle hook (e.g. "BeforeDeploy", "AfterDeploy") if it is
// declared in gothic.config.go. When the hook is absent it returns the context
// unchanged with no error and spawns no orchestrator subprocess.
func runHook(projectRoot, hookName string, gctx *gothic_config.GothicContext) (*gothic_config.GothicContext, error) {
	has, err := astconfig.HasHook(projectRoot, hookName)
	if err != nil {
		return gctx, err
	}
	if !has {
		return gctx, nil
	}
	return astconfig.GenerateOrchestrator(projectRoot, hookName, gctx)
}

// promptDeleteRemoteState asks the user whether to delete the OpenTofu remote
// state backend (the S3 state bucket + DynamoDB lock table). Because the state
// backend is shared across deploys, it is preserved by default; deletion only
// happens on an explicit "y".
func promptDeleteRemoteState(ctx context.Context, cli *gothic_cli.GothicCli, config gothic_cli.Config, stage, suffix string) {
	stateBucket := config.ProjectName + "-state-" + suffix
	lockTable := config.ProjectName + "-lock-" + suffix

	// The state bucket + lock table are shared across ALL stages of this project
	// (named by suffix, not stage). If any OTHER stage still keeps its state here,
	// deleting the backend would orphan that stage's resources — so refuse outright
	// rather than even offering it. On a check error, preserve state (the safe
	// default); the user can always delete the backend manually.
	others, err := tofu.OtherStageStates(ctx, config.Deploy.Region, config.Deploy.Profile, stateBucket, config.ProjectName, stage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Remote state preserved — could not verify other stages: %v\n", err)
		return
	}
	if len(others) > 0 {
		fmt.Println(paint(clrYellow, fmt.Sprintf("Remote state preserved — the state bucket is shared and still in use by stage(s): %s.", strings.Join(others, ", "))))
		return
	}

	fmt.Printf("Delete remote state (bucket %q, lock table %q)? This cannot be undone. [y/N] ", stateBucket, lockTable)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))

	if !strings.HasPrefix(answer, "y") {
		fmt.Println("Skipped — remote state preserved.")
		return
	}

	if err := tofu.DeleteRemoteState(ctx, config.Deploy.Region, config.Deploy.Profile, stateBucket, lockTable); err != nil {
		fmt.Fprintf(os.Stderr, "error deleting remote state: %v\n", err)
		return
	}
	fmt.Println("Remote state deleted.")
}

func (command *DeployCommand) isValidAction(c string) bool {
	for _, a := range command.allowedActions {
		if a == c {
			return true
		}
	}
	return false
}
