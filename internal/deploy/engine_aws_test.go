package tofu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/core/config"
	"github.com/gothicframework/cli/v3/internal/deploy/tfgen"
)

// --- S3 bootstrap mock ---

type mockBootstrapS3 struct {
	headErr        error
	createCalled   bool
	versioningCall bool
	encryptionCall bool
}

func (m *mockBootstrapS3) HeadBucket(ctx context.Context, _ *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	return &s3.HeadBucketOutput{}, nil
}
func (m *mockBootstrapS3) CreateBucket(ctx context.Context, _ *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	m.createCalled = true
	return &s3.CreateBucketOutput{}, nil
}
func (m *mockBootstrapS3) PutBucketVersioning(ctx context.Context, _ *s3.PutBucketVersioningInput, _ ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error) {
	m.versioningCall = true
	return &s3.PutBucketVersioningOutput{}, nil
}
func (m *mockBootstrapS3) PutBucketEncryption(ctx context.Context, _ *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	m.encryptionCall = true
	return &s3.PutBucketEncryptionOutput{}, nil
}

// --- DynamoDB bootstrap mock ---

type mockBootstrapDDB struct {
	// describeErrFirst is returned by the first DescribeTable call; subsequent
	// calls (the waiter) return an ACTIVE table so the waiter resolves at once.
	describeErrFirst error
	describeCalls    int
	createCalled     bool
}

func (m *mockBootstrapDDB) DescribeTable(ctx context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	m.describeCalls++
	if m.describeCalls == 1 && m.describeErrFirst != nil {
		return nil, m.describeErrFirst
	}
	return &dynamodb.DescribeTableOutput{
		Table: &ddbtypes.TableDescription{TableStatus: ddbtypes.TableStatusActive},
	}, nil
}
func (m *mockBootstrapDDB) CreateTable(ctx context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	m.createCalled = true
	return &dynamodb.CreateTableOutput{}, nil
}

func newTestEngine(s3m bootstrapS3Iface, ddbm bootstrapDDBIface) *TofuAwsEngine {
	return &TofuAwsEngine{
		config: &cli.Config{
			ProjectName: "demo",
			GoModName:   "example.com/demo",
			Deploy: &cli.DeployConfig{Provider: cli.AWS, Providers: cli.Providers{AWS: cli.AWSProvider{
				Region:  "us-east-1",
				Profile: "default",
			}}},
		},
		stateBucket: "demo-state-x",
		lockTable:   "demo-lock-x",
		s3:          s3m,
		ddb:         ddbm,
	}
}

func TestBootstrapStateBucketCreatesWhenAbsent(t *testing.T) {
	s3m := &mockBootstrapS3{headErr: &s3types.NotFound{}}
	e := newTestEngine(s3m, nil)
	if err := e.bootstrapStateBucket(context.Background()); err != nil {
		t.Fatalf("bootstrapStateBucket: %v", err)
	}
	if !s3m.createCalled {
		t.Error("CreateBucket was not called for an absent bucket")
	}
	if !s3m.versioningCall || !s3m.encryptionCall {
		t.Error("expected versioning + encryption to be enabled on a new bucket")
	}
}

func TestBootstrapStateBucketSkipsWhenPresent(t *testing.T) {
	s3m := &mockBootstrapS3{headErr: nil} // HeadBucket succeeds → bucket exists
	e := newTestEngine(s3m, nil)
	if err := e.bootstrapStateBucket(context.Background()); err != nil {
		t.Fatalf("bootstrapStateBucket: %v", err)
	}
	if s3m.createCalled {
		t.Error("CreateBucket should NOT be called when the bucket already exists")
	}
}

func TestBootstrapLockTableCreatesWhenAbsent(t *testing.T) {
	ddbm := &mockBootstrapDDB{describeErrFirst: &ddbtypes.ResourceNotFoundException{}}
	e := newTestEngine(nil, ddbm)
	if err := e.bootstrapLockTable(context.Background()); err != nil {
		t.Fatalf("bootstrapLockTable: %v", err)
	}
	if !ddbm.createCalled {
		t.Error("CreateTable was not called for an absent lock table")
	}
}

func TestBootstrapLockTableSkipsWhenPresent(t *testing.T) {
	ddbm := &mockBootstrapDDB{describeErrFirst: nil} // DescribeTable succeeds
	e := newTestEngine(nil, ddbm)
	if err := e.bootstrapLockTable(context.Background()); err != nil {
		t.Fatalf("bootstrapLockTable: %v", err)
	}
	if ddbm.createCalled {
		t.Error("CreateTable should NOT be called when the lock table already exists")
	}
}

// mockEmptyBucketS3 returns a single page of versions + delete markers.
type mockEmptyBucketS3 struct {
	versions     int
	deleteMarker int
	deleteCalls  int
	deletedIDs   int
}

func (m *mockEmptyBucketS3) ListObjectVersions(ctx context.Context, _ *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	out := &s3.ListObjectVersionsOutput{}
	for i := 0; i < m.versions; i++ {
		out.Versions = append(out.Versions, s3types.ObjectVersion{
			Key:       awsString("k"),
			VersionId: awsString("v"),
		})
	}
	for i := 0; i < m.deleteMarker; i++ {
		out.DeleteMarkers = append(out.DeleteMarkers, s3types.DeleteMarkerEntry{
			Key:       awsString("k"),
			VersionId: awsString("dm"),
		})
	}
	return out, nil
}

func (m *mockEmptyBucketS3) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	m.deleteCalls++
	m.deletedIDs += len(in.Delete.Objects)
	return &s3.DeleteObjectsOutput{}, nil
}

func awsString(s string) *string { return &s }

func TestEmptyVersionedBucketBatchesDeletes(t *testing.T) {
	// 1500 versions + 600 delete markers = 2100 ids → batches of 1000,1000,100.
	m := &mockEmptyBucketS3{versions: 1500, deleteMarker: 600}
	if err := emptyVersionedBucket(context.Background(), m, "bucket"); err != nil {
		t.Fatalf("emptyVersionedBucket: %v", err)
	}
	if m.deleteCalls != 3 {
		t.Errorf("DeleteObjects called %d times, want 3", m.deleteCalls)
	}
	if m.deletedIDs != 2100 {
		t.Errorf("deleted %d ids, want 2100", m.deletedIDs)
	}
}

func TestEmptyVersionedBucketEmpty(t *testing.T) {
	m := &mockEmptyBucketS3{}
	if err := emptyVersionedBucket(context.Background(), m, "bucket"); err != nil {
		t.Fatalf("emptyVersionedBucket: %v", err)
	}
	if m.deleteCalls != 0 {
		t.Errorf("DeleteObjects should not be called for an empty bucket, got %d", m.deleteCalls)
	}
}

// fakeBinaryManager returns a fixed path from EnsureBinary.
type fakeBinaryManager struct{ path string }

func (f fakeBinaryManager) EnsureBinary(ctx context.Context) (string, error) {
	return f.path, nil
}

func TestConstructorsWireFields(t *testing.T) {
	if NewBinaryManager(&cli.Config{}) == nil {
		t.Error("NewBinaryManager returned nil")
	}
	if NewCloudFrontCDN(aws.Config{}) == nil {
		t.Error("NewCloudFrontCDN returned nil")
	}
}

func TestInitTofuBuildsRunner(t *testing.T) {
	// A real (no-op) executable lets tfexec.NewTerraform validate the path.
	dir := t.TempDir()
	bin := filepath.Join(dir, "tofu")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := &TofuAwsEngine{
		binary:  fakeBinaryManager{path: bin},
		workDir: dir,
	}
	if err := e.initTofu(context.Background()); err != nil {
		t.Fatalf("initTofu: %v", err)
	}
	if e.tf == nil {
		t.Error("tf runner not set after initTofu")
	}
}

func TestNewTofuAwsEngineNilConfig(t *testing.T) {
	if _, err := NewTofuAwsEngine(nil, aws.Config{}); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestBuildTfGenParamsMapsPointerFields(t *testing.T) {
	domain := config.EnvValue{Source: config.RawEnv, Value: "app.example.com"}
	zone := config.EnvValue{Source: config.SSMParamEnv, Value: "/demo/zone"}
	cert := config.EnvValue{Source: config.SecretsManagerEnv, Value: "/demo/cert"}
	waf := config.EnvValue{Source: config.RawEnv, Value: "arn:waf"}
	e := &TofuAwsEngine{
		stage:       "prod",
		suffix:      "sfx",
		bucketName:  "demo-prod-sfx",
		lambdaName:  "demo-prod-sfx",
		stateBucket: "demo-state-sfx",
		lockTable:   "demo-lock-sfx",
		config: &cli.Config{
			ProjectName: "demo",
			Deploy: &cli.DeployConfig{
				Provider: cli.AWS,
				Providers: cli.Providers{AWS: cli.AWSProvider{
					Region:        "us-east-1",
					Profile:       "default",
					ServerMemory:  512,
					ServerTimeout: 30,
					Stages: map[string]cli.EnvVariables{
						"prod": {
							CustomDomain:   &domain,
							HostedZoneId:   &zone,
							CertificateArn: &cert,
							WafArn:         &waf,
						},
					},
				}},
			},
		},
	}
	p := e.buildTfGenParams()
	if p.CustomDomain == nil || *p.CustomDomain != domain ||
		p.HostedZoneId == nil || *p.HostedZoneId != zone ||
		p.CertificateArn == nil || *p.CertificateArn != cert ||
		p.WafArn == nil || *p.WafArn != waf {
		t.Errorf("source-aware domain fields not mapped: %+v", p)
	}
	if p.BucketName != "demo-prod-sfx" || p.Suffix != "sfx" {
		t.Errorf("computed names not mapped: %+v", p)
	}
}

func TestPrepareComputesDeterministicNames(t *testing.T) {
	// Prepare wires bootstrap + tfgen; with both backend resources reported
	// present, no AWS mutation happens and we can assert the computed names and
	// that a working directory was generated.
	s3m := &mockBootstrapS3{headErr: nil}
	ddbm := &mockBootstrapDDB{describeErrFirst: nil}
	e := &TofuAwsEngine{
		config: &cli.Config{
			ProjectName: "demo",
			GoModName:   "example.com/demo",
			Deploy: &cli.DeployConfig{
				Provider: cli.AWS,
				Providers: cli.Providers{AWS: cli.AWSProvider{
					Region:  "us-east-1",
					Profile: "default",
					Stages:  map[string]cli.EnvVariables{"dev": {}},
				}},
			},
		},
		tfgen: tfgen.NewGenerator(),
		s3:    s3m,
		ddb:   ddbm,
	}
	dir := t.TempDir()
	t.Chdir(dir)

	if err := e.Prepare(context.Background(), "dev"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantSuffix := ResourceSuffix("example.com/demo", "demo")
	if e.suffix != wantSuffix {
		t.Errorf("suffix = %q, want %q", e.suffix, wantSuffix)
	}
	if e.bucketName != "demo-dev-"+wantSuffix {
		t.Errorf("bucketName = %q", e.bucketName)
	}
	// The ECR repo is namespaced per stage so a stage teardown never force-deletes
	// another stage's images.
	if e.ecrRepo != "demo-dev-"+wantSuffix {
		t.Errorf("ecrRepo = %q, want %q", e.ecrRepo, "demo-dev-"+wantSuffix)
	}
}
