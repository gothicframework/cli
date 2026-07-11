package tofu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/cli/v3/internal/deploy/docker"
	"github.com/gothicframework/cli/v3/internal/deploy/tfgen"
	"github.com/gothicframework/core/config"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// TofuAwsEngine implements DeploymentEngine for AWS. It wires together the
// OpenTofu binary manager, the .tf.json generator, the Docker engine, and the
// terraform-exec runner (which is OpenTofu-compatible). All resource names are
// computed deterministically from the module name + project name via
// ResourceSuffix, so no per-machine state file is required.
type TofuAwsEngine struct {
	config *cli.Config
	binary BinaryManager
	tfgen  *tfgen.Generator
	docker *docker.DockerEngine
	awsCfg aws.Config

	workDir  string
	tf       *tfexec.Terraform // set lazily in Deploy/Destroy
	tfStdout io.Writer         // colorizing wrapper for tofu's streamed stdout

	// Client seams: nil in production (lazily constructed from awsCfg), set by
	// tests to inject mocks. Use s3Client()/ddbClient() rather than these directly.
	s3  bootstrapS3Iface
	ddb bootstrapDDBIface

	// computed in Prepare
	stage       string
	suffix      string
	ecrRepo     string
	ecrImageURI string
	bucketName  string
	lambdaName  string
	stateBucket string
	lockTable   string
}

// bootstrapS3Iface is the subset of the S3 SDK client used while bootstrapping
// the remote state backend. *s3.Client satisfies it; tests inject a mock.
type bootstrapS3Iface interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutBucketVersioning(ctx context.Context, params *s3.PutBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error)
	PutBucketEncryption(ctx context.Context, params *s3.PutBucketEncryptionInput, optFns ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
}

// bootstrapDDBIface is the subset of the DynamoDB SDK client used while
// bootstrapping the lock table, plus the waiter dependency. *dynamodb.Client
// satisfies it; tests inject a mock.
type bootstrapDDBIface interface {
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
}

// s3Client returns the injected S3 seam or lazily constructs a real client.
func (e *TofuAwsEngine) s3Client() bootstrapS3Iface {
	if e.s3 != nil {
		return e.s3
	}
	return s3.NewFromConfig(e.awsCfg)
}

// ddbClient returns the injected DynamoDB seam or lazily constructs a real client.
func (e *TofuAwsEngine) ddbClient() bootstrapDDBIface {
	if e.ddb != nil {
		return e.ddb
	}
	return dynamodb.NewFromConfig(e.awsCfg)
}

// NewTofuAwsEngine constructs a TofuAwsEngine bound to the resolved config and a
// loaded aws.Config. The Docker engine is created eagerly so a missing Docker
// daemon surfaces a clear error at Build time, not at construction.
func NewTofuAwsEngine(c *cli.Config, awsCfg aws.Config) (*TofuAwsEngine, error) {
	if c == nil {
		return nil, errors.New("nil config passed to NewTofuAwsEngine")
	}
	dockerEngine, err := docker.NewDockerEngine()
	if err != nil {
		return nil, err
	}
	return &TofuAwsEngine{
		config: c,
		binary: NewBinaryManager(c),
		tfgen:  tfgen.NewGenerator(),
		docker: dockerEngine,
		awsCfg: awsCfg,
	}, nil
}

// Prepare computes all resource names, bootstraps the remote state backend (S3
// state bucket + DynamoDB lock table) when absent, and generates the OpenTofu
// working directory for the given stage.
func (e *TofuAwsEngine) Prepare(ctx context.Context, stage string) error {
	c := e.config
	e.stage = stage
	e.suffix = ResourceSuffix(c.GoModName, c.ProjectName)
	// The ECR repo is namespaced per stage (like the bucket and lambda) so tearing
	// down one stage force-deletes only its own images — a shared repo would let a
	// `dev` teardown wipe the images every other stage's Lambda still pulls.
	e.ecrRepo = c.ProjectName + "-" + stage + "-" + e.suffix
	e.bucketName = c.ProjectName + "-" + stage + "-" + e.suffix
	e.lambdaName = c.ProjectName + "-" + stage + "-" + e.suffix
	e.stateBucket = c.ProjectName + "-state-" + e.suffix
	e.lockTable = c.ProjectName + "-lock-" + e.suffix

	if err := e.bootstrapStateBucket(ctx); err != nil {
		return err
	}
	if err := e.bootstrapLockTable(ctx); err != nil {
		return err
	}

	e.workDir = filepath.Join(".gothicCli", "tofu", stage)

	params := e.buildTfGenParams()
	if err := e.tfgen.Prepare(e.workDir, params); err != nil {
		return fmt.Errorf("generating tofu working directory: %w", err)
	}
	return nil
}

// buildTfGenParams maps the resolved config + computed names into TfGenParams.
// The ECR image URI is left empty here; it is supplied to OpenTofu via the
// per-deploy var after Build (see Deploy, which re-runs Prepare's var write is
// not needed because the image var is passed at apply time through the regenerated
// vars file — here we simply pass what is known at Prepare time).
func (e *TofuAwsEngine) buildTfGenParams() tfgen.TfGenParams {
	c := e.config
	aws := c.Deploy.Providers.AWS
	stageCfg := aws.Stages[e.stage]

	params := tfgen.TfGenParams{
		ProjectName:   c.ProjectName,
		Stage:         e.stage,
		Suffix:        e.suffix,
		Region:        aws.Region,
		Profile:       aws.Profile,
		ServerMemory:  aws.ServerMemory,
		ServerTimeout: aws.ServerTimeout,
		BucketName:    e.bucketName,
		LambdaName:    e.lambdaName,
		StateBucket:   e.stateBucket,
		LockTable:     e.lockTable,
		EnvVars:       map[string]config.EnvValue{},
		// CloudFront distribution knobs (allowed query params / cookies / headers) for
		// the dynamic Lambda behavior. Zero value = all query params, no cookies/headers.
		CDN: aws.CDN,
		// Custom user infrastructure lives in a cwd-relative "infra/" dir, mirroring
		// the "public/" asset convention: the deploy runs from the project root, so
		// every *.tf / *.tf.json inside it is merged into the same OpenTofu stack.
		InfraDir: "infra",
	}
	if stageCfg.ENV != nil {
		params.EnvVars = stageCfg.ENV
	}
	// Source-aware domain fields are copied through as-is (nil stays nil); the
	// generator resolves each into a local via raw value / SSM / Secrets Manager.
	params.WafArn = stageCfg.WafArn
	params.CustomDomain = stageCfg.CustomDomain
	params.HostedZoneId = stageCfg.HostedZoneId
	params.CertificateArn = stageCfg.CertificateArn
	return params
}

// bootstrapStateBucket creates the OpenTofu state bucket with versioning and
// SSE-AES256 when it does not already exist. An existing bucket is left as-is.
func (e *TofuAwsEngine) bootstrapStateBucket(ctx context.Context) error {
	s3Client := e.s3Client()

	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(e.stateBucket)})
	if err == nil {
		return nil // already exists and is accessible
	}
	var notFound *s3types.NotFound
	var noSuchBucket *s3types.NoSuchBucket
	if !errors.As(err, &notFound) && !errors.As(err, &noSuchBucket) && !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("checking state bucket %q: %w", e.stateBucket, err)
	}

	createInput := &s3.CreateBucketInput{Bucket: aws.String(e.stateBucket)}
	// us-east-1 must NOT set a LocationConstraint; all other regions must.
	if e.config.Deploy.Providers.AWS.Region != "us-east-1" {
		createInput.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(e.config.Deploy.Providers.AWS.Region),
		}
	}
	if _, err := s3Client.CreateBucket(ctx, createInput); err != nil {
		return fmt.Errorf("creating state bucket %q: %w", e.stateBucket, err)
	}

	if _, err := s3Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(e.stateBucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		return fmt.Errorf("enabling versioning on state bucket %q: %w", e.stateBucket, err)
	}

	if _, err := s3Client.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(e.stateBucket),
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAes256,
				},
			}},
		},
	}); err != nil {
		return fmt.Errorf("enabling encryption on state bucket %q: %w", e.stateBucket, err)
	}

	fmt.Fprintf(os.Stderr, "Created state bucket %s\n", e.stateBucket)
	return nil
}

// bootstrapLockTable creates the DynamoDB lock table with a LockID string hash
// key (the schema OpenTofu's S3 backend expects) when it does not exist, then
// waits for it to become ACTIVE.
func (e *TofuAwsEngine) bootstrapLockTable(ctx context.Context) error {
	ddbClient := e.ddbClient()

	_, err := ddbClient.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(e.lockTable),
	})
	if err == nil {
		return nil // already exists
	}
	var notFound *ddbtypes.ResourceNotFoundException
	if !errors.As(err, &notFound) && !strings.Contains(err.Error(), "ResourceNotFoundException") {
		return fmt.Errorf("checking lock table %q: %w", e.lockTable, err)
	}

	if _, err := ddbClient.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(e.lockTable),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{
			AttributeName: aws.String("LockID"),
			AttributeType: ddbtypes.ScalarAttributeTypeS,
		}},
		KeySchema: []ddbtypes.KeySchemaElement{{
			AttributeName: aws.String("LockID"),
			KeyType:       ddbtypes.KeyTypeHash,
		}},
	}); err != nil {
		return fmt.Errorf("creating lock table %q: %w", e.lockTable, err)
	}

	waiter := dynamodb.NewTableExistsWaiter(ddbClient)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(e.lockTable),
	}, 2*time.Minute); err != nil {
		return fmt.Errorf("waiting for lock table %q to become active: %w", e.lockTable, err)
	}

	fmt.Fprintf(os.Stderr, "Created lock table %s\n", e.lockTable)
	return nil
}

// Build checks the Docker daemon, ensures the ECR repository exists, builds the
// Lambda image, pushes it, and returns the fully-qualified image URI (uri:tag).
func (e *TofuAwsEngine) Build(ctx context.Context, tag string) (string, error) {
	if err := e.docker.CheckDaemon(ctx); err != nil {
		return "", err
	}
	// e.ecrRepo is the single source of truth for the repo name (set in Prepare and
	// imported into tofu state in Deploy), so Build no longer takes it as a param —
	// that avoided a second computation site that could drift from Prepare's.
	uri, err := e.docker.EnsureECRRepo(ctx, e.awsCfg, e.ecrRepo)
	if err != nil {
		return "", err
	}
	// Record the immutable per-deploy image reference (repo:<tag>) so Deploy wires
	// the Lambda's ecr_image_uri var to exactly the image just pushed. It MUST be
	// the unique tag, not repo:latest: an image-based Lambda pins the digest at
	// apply time and a constant image_uri makes tofu see no diff on redeploy, so
	// a moving :latest would leave the Lambda stuck on the previous image.
	// buildTfGenParams intentionally leaves it empty at Prepare time because the
	// repo URI is only known here.
	e.ecrImageURI = uri + ":" + tag
	if err := e.docker.BuildImage(ctx, ".", e.config.DockerfilePath, uri, tag, "aws"); err != nil {
		return "", err
	}
	if err := e.docker.PushImage(ctx, e.awsCfg, uri+":"+tag); err != nil {
		return "", err
	}
	return uri + ":" + tag, nil
}

// Deploy runs tofu init + apply, reads the stack outputs, and returns them as a
// string map. The outputs are NOT written to disk: they can carry sensitive
// values (ARNs, domains) and were previously persisted to gothic_outputs.json at
// the project root, which risked leaking into git. cmd/deploy.go prints them in a
// clean summary instead, and nothing in the codebase reads them back from a file.
func (e *TofuAwsEngine) Deploy(ctx context.Context) (map[string]string, error) {
	if err := e.initTofu(ctx); err != nil {
		return nil, err
	}
	// -reconfigure: the working dir (.gothicCli/tofu/<stage>) is reused across
	// deploys and caches the previous backend config in .terraform/. If the project
	// name, region or state bucket changed, tofu otherwise aborts with "Backend
	// configuration changed". The generated backend config is our source of truth,
	// so reconfigure to it without attempting to migrate old state.
	if err := e.tf.Init(ctx, tfexec.Reconfigure(true)); err != nil {
		return nil, fmt.Errorf("tofu init: %w", err)
	}
	// Supply the Lambda image URI (known only after Build) as an auto-loaded tfvar
	// in the tofu working dir — completing the wiring buildTfGenParams defers.
	if e.ecrImageURI != "" {
		data, err := json.Marshal(map[string]string{"ecr_image_uri": e.ecrImageURI})
		if err != nil {
			return nil, fmt.Errorf("marshaling image tfvar: %w", err)
		}
		if err := os.WriteFile(filepath.Join(e.workDir, "gothic_image.auto.tfvars.json"), data, 0644); err != nil {
			return nil, fmt.Errorf("writing image tfvar: %w", err)
		}
	}
	// EnsureECRRepo (in Build) creates the ECR repo OUTSIDE tofu so the image can
	// be pushed before apply. Adopt it into tofu state so apply doesn't try to
	// re-create the existing repo (RepositoryAlreadyExistsException) and so a later
	// destroy removes it. Idempotent: a repo already in state imports as a no-op.
	if e.ecrRepo != "" {
		if err := e.tf.Import(ctx, "aws_ecr_repository.main", e.ecrRepo); err != nil {
			if m := err.Error(); !strings.Contains(m, "already managed") && !strings.Contains(m, "Resource already managed") {
				return nil, fmt.Errorf("adopting ECR repo into tofu state: %w", err)
			}
		}
	}

	// Apply auto-approves: Gothic owns the deploy lifecycle end-to-end (the user
	// already opted in by running `gothic deploy`), so an interactive approval
	// prompt would only get in the way. terraform-exec's Apply always runs with
	// -auto-approve.
	if err := e.tf.Apply(ctx); err != nil {
		return nil, fmt.Errorf("tofu apply: %w", err)
	}

	// Query outputs quietly: terraform-exec echoes `tofu output -json` to the
	// configured stdout, dumping a noisy JSON blob right after apply's own
	// "Outputs:" section. Silence just this read; cmd/deploy.go then prints a
	// clean, structured summary from the returned map.
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
func (e *TofuAwsEngine) Destroy(ctx context.Context) error {
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
func (e *TofuAwsEngine) initTofu(ctx context.Context) error {
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
	// OpenTofu emits no color through terraform-exec's piped stdout; recolor it.
	e.tfStdout = newTofuColorWriter(os.Stdout)
	tf.SetStdout(e.tfStdout)
	tf.SetStderr(os.Stderr)
	e.tf = tf
	return nil
}

// OtherStageStates returns the stages — other than currentStage — that still have
// an OpenTofu state file in the shared state bucket. State keys are laid out as
// "gothic/<project>/<stage>/terraform.tfstate" (see main.tf.json backend), so a
// delimited list of "gothic/<project>/" yields one common prefix per stage. It is
// used to refuse deleting the shared state backend while other stages depend on it
// (deleting it would orphan their resources). An absent bucket yields no stages.
func OtherStageStates(ctx context.Context, region, profile, stateBucket, projectName, currentStage string) ([]string, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithSharedConfigProfile(profile),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	s3Client := s3.NewFromConfig(awsCfg)

	prefix := fmt.Sprintf("gothic/%s/", projectName)
	out, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(stateBucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		// A missing bucket means there is no shared state at all — no other stages.
		var noBucket *s3types.NoSuchBucket
		var notFound *s3types.NotFound
		if errors.As(err, &noBucket) || errors.As(err, &notFound) || strings.Contains(err.Error(), "NoSuchBucket") || strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing state prefixes in %q: %w", stateBucket, err)
	}

	var others []string
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix == nil {
			continue
		}
		stage := strings.TrimSuffix(strings.TrimPrefix(*cp.Prefix, prefix), "/")
		if stage != "" && stage != currentStage {
			others = append(others, stage)
		}
	}
	sort.Strings(others)
	return others, nil
}

// DeleteRemoteState empties and deletes the OpenTofu remote state bucket
// (including all object versions and delete markers) and deletes the DynamoDB
// lock table. It loads its own aws.Config from the given region + profile so it
// can be called from cmd/deploy.go without an engine instance.
func DeleteRemoteState(ctx context.Context, region, profile, stateBucket, lockTable string) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithSharedConfigProfile(profile),
	)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg)
	if err := emptyVersionedBucket(ctx, s3Client, stateBucket); err != nil {
		return err
	}
	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(stateBucket),
	}); err != nil {
		return fmt.Errorf("deleting state bucket %q: %w", stateBucket, err)
	}

	ddbClient := dynamodb.NewFromConfig(awsCfg)
	if _, err := ddbClient.DeleteTable(ctx, &dynamodb.DeleteTableInput{
		TableName: aws.String(lockTable),
	}); err != nil {
		return fmt.Errorf("deleting lock table %q: %w", lockTable, err)
	}
	return nil
}

// emptyBucketS3Iface is the subset of the S3 client used by emptyVersionedBucket.
// *s3.Client satisfies it; tests inject a mock. It exposes exactly the methods
// the S3 ListObjectVersions paginator and the batched delete require.
type emptyBucketS3Iface interface {
	ListObjectVersions(ctx context.Context, params *s3.ListObjectVersionsInput, optFns ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// emptyVersionedBucket removes every object version and delete marker from a
// versioning-enabled bucket so it can be deleted.
func emptyVersionedBucket(ctx context.Context, s3Client emptyBucketS3Iface, bucket string) error {
	paginator := s3.NewListObjectVersionsPaginator(s3Client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing object versions in %q: %w", bucket, err)
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range page.Versions {
			ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		for len(ids) > 0 {
			n := len(ids)
			if n > 1000 {
				n = 1000
			}
			if _, err := s3Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &s3types.Delete{Objects: ids[:n]},
			}); err != nil {
				return fmt.Errorf("deleting object versions from %q: %w", bucket, err)
			}
			ids = ids[n:]
		}
	}
	return nil
}
