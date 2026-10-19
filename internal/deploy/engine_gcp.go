package tofu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	cli "github.com/gothicframework/cli/v4/internal/cli"
	"github.com/gothicframework/cli/v4/internal/deploy/docker"
	"github.com/gothicframework/cli/v4/internal/deploy/tfgen"
	"github.com/gothicframework/core/config"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// GcpEngine implements DeploymentEngine for Google Cloud Platform. It wires
// together the OpenTofu binary manager, the .tf.json generator, the Docker engine,
// and the gcloud CLI. All resource names are computed deterministically from the
// module name + project name via ResourceSuffix, so no per-machine state file is
// required.
//
// GCP targets:
//   - Cloud Run service (container image)
//   - Artifact Registry (GAR) repository
//   - GCS bucket for /public/* assets (served via Cloud CDN or direct)
//   - Remote state via GCS bucket
//
// Auth: ADC (application default credentials) + gcloud CLI. The user must have
// gcloud installed and authenticated (gcloud auth application-default login).
type GcpEngine struct {
	config    *cli.Config
	binary    BinaryManager
	tfgen     *tfgen.Generator
	docker    *docker.DockerEngine
	workDir   string
	tf        *tfexec.Terraform // set lazily in Deploy/Destroy
	tfStdout  io.Writer         // colorizing wrapper for tofu's streamed stdout

	// computed in Prepare
	stage      string
	suffix     string
	garRepo    string
	garImageURI string
	bucketName string
	svcName    string
	stateBucket string
}

// NewGcpEngine constructs a GcpEngine bound to the resolved config and the gcloud
// CLI path (loaded from the config or discovered on the PATH). The Docker engine is
// created eagerly so a missing Docker daemon surfaces a clear error at Build time,
// not at construction.
func NewGcpEngine(c *cli.Config) (*GcpEngine, error) {
	if c == nil {
		return nil, fmt.Errorf("nil config passed to NewGcpEngine")
	}
	dockerEngine, err := docker.NewDockerEngine()
	if err != nil {
		return nil, err
	}
	return &GcpEngine{
		config:   c,
		binary:   NewBinaryManager(c),
		tfgen:    tfgen.NewGenerator(),
		docker:   dockerEngine,
	}, nil
}

// Prepare computes all resource names, bootstraps the remote state backend (GCS
// state bucket) when absent, and generates the OpenTofu working directory for the
// given stage.
func (e *GcpEngine) Prepare(ctx context.Context, stage string) error {
	// Fail fast on a missing gcloud before any resource work starts.
	if err := EnsureGCloudCLI(); err != nil {
		return err
	}
	c := e.config
	gcp := c.Deploy.Providers.GCP
	stageCfg := gcp.Stages[stage]

	e.stage = stage
	e.suffix = ResourceSuffix(c.GoModName, c.ProjectName)
	// GAR repo is namespaced per stage so tearing down one stage deletes only its own.
	e.garRepo = c.ProjectName + "-" + stage + "-" + e.suffix
	e.bucketName = c.ProjectName + "-" + stage + "-" + e.suffix
	e.svcName = c.ProjectName + "-" + stage + "-" + e.suffix
	e.stateBucket = c.ProjectName + "-state-" + e.suffix

	if err := e.bootstrapStateBucket(ctx, gcp.Project, gcp.Region); err != nil {
		return err
	}

	e.workDir = filepath.Join(".gothicCli", "tofu", stage)

	if err := EnsureRegion(gcp.Region); err != nil {
		return err
	}

	params := tfgen.TfGenParams{
		Provider:    config.GCP,
		GcpProject:  gcp.Project,
		ProjectName: c.ProjectName,
		Stage:       e.stage,
		Suffix:      e.suffix,
		Region:      gcp.Region,
		// GCP uses ADC + gcloud for auth — no AWS-style profile.
		BucketName:  e.bucketName,
		LambdaName:  e.svcName,
		StateBucket: e.stateBucket,
		EnvVars:     map[string]config.EnvValue{},
		InfraDir:    "infra",
	}
	for k, v := range stageCfg.ENV {
		params.EnvVars[k] = v
	}
	// The stage's custom domain drives the Cloud Run domain mapping; a nil
	// pointer means the stage serves no custom domain.
	params.CustomDomain = stageCfg.CustomDomain
	params.EnvVars["GOTHIC_PROVIDER"] = config.EnvValue{Source: config.RawEnv, Value: "GCP"}

	if err := e.tfgen.Prepare(e.workDir, params); err != nil {
		return fmt.Errorf("generating tofu working directory: %w", err)
	}
	return nil
}

// bootstrapStateBucket creates a GCS bucket for OpenTofu remote state when it does
// not already exist. An existing bucket is left as-is.
func (e *GcpEngine) bootstrapStateBucket(ctx context.Context, project, region string) error {
	// gcloud storage uses the gs:// URI scheme — the GCS backend in main.tf.json
	// expects the bucket to already exist before tofu init.
	bucket := "gs://" + e.stateBucket
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "buckets", "describe", bucket,
		"--project", project, "--format", "value(name)")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Bucket doesn't exist — create it
		cmd = exec.CommandContext(ctx, "gcloud", "storage", "buckets", "create", bucket,
			"--project", project, "--location", region)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("creating GCS state bucket %q: %w", e.stateBucket, err)
		}
		fmt.Fprintf(os.Stderr, "Created state bucket %s\n", e.stateBucket)
	}
	return nil
}

// Build checks the Docker daemon, ensures the GAR repository exists, builds the
// Cloud Run container image, pushes it, and returns the fully-qualified image
// URI (<region>-docker.pkg.dev/<project>/<repo>:<tag>).
func (e *GcpEngine) Build(ctx context.Context, tag string) (string, error) {
	if err := e.docker.CheckDaemon(ctx); err != nil {
		return "", err
	}

	gcp := e.config.Deploy.Providers.GCP

	// Check if GAR repo exists, create if not
	cmd := exec.CommandContext(ctx, "gcloud", "artifacts", "repositories", "describe",
		e.garRepo, "--project", gcp.Project, "--location", gcp.Region, "--format", "value(name)")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Repository doesn't exist — create it
		cmd = exec.CommandContext(ctx, "gcloud", "artifacts", "repositories", "create",
			e.garRepo, "--project", gcp.Project, "--repository-format", "docker",
			"--location", gcp.Region)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("creating GAR repository %q: %w", e.garRepo, err)
		}
		fmt.Fprintf(os.Stderr, "Created GAR repository %s\n", e.garRepo)
	}

	// Configure the Docker credential helper for the GAR hostname so `docker
	// push` authenticates with the user's gcloud credentials (ADC). Idempotent.
	registry := fmt.Sprintf("%s-docker.pkg.dev", gcp.Region)
	authCmd := exec.CommandContext(ctx, "gcloud", "auth", "configure-docker", registry, "--quiet")
	authCmd.Stderr = os.Stderr
	if err := authCmd.Run(); err != nil {
		return "", fmt.Errorf("configuring docker credential helper for %s: %w", registry, err)
	}

	uri := fmt.Sprintf("%s/%s/%s", registry, gcp.Project, e.garRepo)
	e.garImageURI = uri + ":" + tag

	dockerfilePath := e.config.DockerfilePath
	if err := e.docker.BuildImage(ctx, ".", dockerfilePath, uri, tag, "gcp"); err != nil {
		return "", err
	}

	// Push to GAR via the credential helper configured above.
	pushCmd := exec.CommandContext(ctx, "docker", "push", uri+":"+tag)
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return "", fmt.Errorf("pushing image to GAR %q: %w", uri, err)
	}

	return uri + ":" + tag, nil
}

// Deploy runs tofu init + apply, reads the stack outputs, and returns them as a
// string map. For GCP, the tofu stack creates Cloud Run service, GCS bucket, and
// optionally Cloud CDN.
func (e *GcpEngine) Deploy(ctx context.Context) (map[string]string, error) {
	if err := e.initTofu(ctx); err != nil {
		return nil, err
	}

	if err := e.tf.Init(ctx, tfexec.Reconfigure(true)); err != nil {
		return nil, fmt.Errorf("tofu init: %w", err)
	}

	// Supply the Cloud Run image URI as an auto-loaded tfvar.
	if e.garImageURI != "" {
		type tfvar struct {
			ImageURI string `json:"gcp_image_uri"`
		}
		data, err := json.Marshal(tfvar{ImageURI: e.garImageURI})
		if err != nil {
			return nil, fmt.Errorf("marshaling image tfvar: %w", err)
		}
		if err := os.WriteFile(filepath.Join(e.workDir, "gothic_image.auto.tfvars.json"), data, 0644); err != nil {
			return nil, fmt.Errorf("writing image tfvar: %w", err)
		}
	}

	// Apply auto-approves: Gothic owns the deploy lifecycle end-to-end.
	if err := e.tf.Apply(ctx); err != nil {
		return nil, fmt.Errorf("tofu apply: %w", err)
	}

	// Query outputs quietly.
	e.tf.SetStdout(io.Discard)
	rawOutputs, err := e.tf.Output(ctx)
	e.tf.SetStdout(e.tfStdout)
	if err != nil {
		return nil, fmt.Errorf("tofu output: %w", err)
	}
	outputs := make(map[string]string, len(rawOutputs))
	for k, v := range rawOutputs {
		outputs[k] = strings.Trim(string(v.Value), "\"")
	}

	return outputs, nil
}

// Destroy runs tofu destroy (auto-approved). It lazily initializes the tofu
// runner if Deploy was not called in this process.
func (e *GcpEngine) Destroy(ctx context.Context) error {
	if e.tf == nil {
		if err := e.initTofu(ctx); err != nil {
			return err
		}
		if err := e.tf.Init(ctx, tfexec.Reconfigure(true)); err != nil {
			return fmt.Errorf("tofu init: %w", err)
		}
	}
	if err := e.tf.Destroy(ctx); err != nil {
		return fmt.Errorf("tofu destroy: %w", err)
	}
	return nil
}

// initTofu resolves the tofu binary and constructs the terraform-exec runner
// bound to the generated work dir, streaming output live to the terminal.
func (e *GcpEngine) initTofu(ctx context.Context) error {
	execPath, err := e.binary.EnsureBinary(ctx)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(execPath)
	if err == nil {
		execPath = abs
	}
	tf, err := tfexec.NewTerraform(e.workDir, execPath)
	if err != nil {
		return fmt.Errorf("initializing tofu runner: %w", err)
	}
	e.tfStdout = newTofuColorWriter(os.Stdout)
	tf.SetStdout(e.tfStdout)
	tf.SetStderr(os.Stderr)
	e.tf = tf
	return nil
}

// DeleteRemoteState empties and deletes the OpenTofu remote state bucket (GCS) and
// the GAR repository when present.
func (e *GcpEngine) DeleteRemoteState(ctx context.Context, region, stateBucket string) error {
	gcp := e.config.Deploy.Providers.GCP
	// Empty the GCS bucket
	listCmd := exec.CommandContext(ctx, "gcloud", "storage", "ls", "--recursive",
		"--project", gcp.Project, "gs://"+stateBucket)
	out, err := listCmd.Output()
	if err != nil {
		// Bucket may not exist — nothing to delete
		return nil
	}
	// Delete each object
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		delCmd := exec.CommandContext(ctx, "gcloud", "storage", "rm", "-q", line)
		delCmd.Stderr = os.Stderr
		delCmd.Run()
	}
	// Delete the bucket itself
	bucketCmd := exec.CommandContext(ctx, "gcloud", "storage", "buckets", "delete",
		"gs://"+stateBucket, "--project", gcp.Project, "--ignore-unhandled-objects")
	bucketCmd.Stderr = os.Stderr
	bucketCmd.Run()
	// Delete GAR repo
	garCmd := exec.CommandContext(ctx, "gcloud", "artifacts", "repositories", "delete",
		e.garRepo, "--project", gcp.Project, "--location", region, "--purge")
	garCmd.Stderr = os.Stderr
	garCmd.Run()
	return nil
}

// CheckAuth verifies that gcloud is installed and application-default credentials
// are configured. It is called by Deploy/Build before touching any GCP APIs.
func (e *GcpEngine) CheckAuth(ctx context.Context) error {
	if err := EnsureGCloudCLI(); err != nil {
		return err
	}
	// print-access-token is the cheapest reliable probe of ADC: it fails when
	// no application-default credentials exist, and its output is discarded.
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud ADC not configured — run `gcloud auth application-default login`")
	}
	return nil
}

// EnsureGCloudCLI returns an error if the gcloud CLI binary is not found on PATH.
func EnsureGCloudCLI() error {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return fmt.Errorf("gcloud CLI not found on PATH — install from https://cloud.google.com/sdk/docs/install")
	}
	return nil
}

// EnsureDocker ensures Docker is installed and running (required for Cloud Run builds).
func EnsureDocker() error {
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Docker daemon not running — start Docker and try again")
	}
	return nil
}

// ValidateRegion checks if the provided GCP region is valid.
func ValidateRegion(region string) error {
	validRegions := map[string]bool{
		// US regions
		"us-central1":           true,
		"us-east1":              true,
		"us-east4":              true,
		"us-east5":              true,
		"us-south1":             true,
		"us-west1":              true,
		"us-west2":              true,
		"us-west3":              true,
		"us-west4":              true,
		"us-west5":              true,
		// Other regions
		"northamerica-northeast1": true,
		"northamerica-northeast2": true,
		"southamerica-east1":      true,
		"europe-west1":            true,
		"europe-west2":            true,
		"europe-west3":            true,
		"europe-west4":            true,
		"europe-west6":            true,
		"europe-west8":            true,
		"europe-west9":            true,
		"europe-north1":           true,
		"europe-southwest1":       true,
		"asia-east1":              true,
		"asia-east2":              true,
		"asia-northeast1":         true,
		"asia-northeast2":         true,
		"asia-northeast3":         true,
		"asia-south1":             true,
		"asia-south2":             true,
		"asia-southeast1":         true,
		"asia-southeast2":         true,
		"australia-southeast1":    true,
		"australia-southeast2":    true,
		"me-central1":             true,
		"me-central2":             true,
		"me-west1":                true,
		"africa-south1":           true,
	}
	if !validRegions[region] {
		return fmt.Errorf("invalid GCP region %q: must be one of the supported regions", region)
	}
	return nil
}

// EnsureRegion validates the GCP region and returns an error if it's not set or invalid.
func EnsureRegion(region string) error {
	if region == "" {
		return fmt.Errorf("GCP region not set — set Deploy.Providers.GCP.Region in gothic.config.go")
	}
	return ValidateRegion(region)
}

// GcloudCmdRunner runs a gcloud command with the given args and returns stdout/stderr.
func GcloudCmdRunner(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
