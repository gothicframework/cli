package docker

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// mockECR implements ecrClientIface.
type mockECR struct {
	describeErr  error
	describeURI  string
	createURI    string
	createCalled bool
	authToken    string // base64 of "user:pass"
	proxy        string
	authErr      error
}

func (m *mockECR) DescribeRepositories(ctx context.Context, _ *ecr.DescribeRepositoriesInput, _ ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	return &ecr.DescribeRepositoriesOutput{
		Repositories: []ecrtypes.Repository{{RepositoryUri: aws.String(m.describeURI)}},
	}, nil
}

func (m *mockECR) CreateRepository(ctx context.Context, _ *ecr.CreateRepositoryInput, _ ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	m.createCalled = true
	return &ecr.CreateRepositoryOutput{
		Repository: &ecrtypes.Repository{RepositoryUri: aws.String(m.createURI)},
	}, nil
}

func (m *mockECR) GetAuthorizationToken(ctx context.Context, _ *ecr.GetAuthorizationTokenInput, _ ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	if m.authErr != nil {
		return nil, m.authErr
	}
	return &ecr.GetAuthorizationTokenOutput{
		AuthorizationData: []ecrtypes.AuthorizationData{{
			AuthorizationToken: aws.String(m.authToken),
			ProxyEndpoint:      aws.String(m.proxy),
		}},
	}, nil
}

// withMockECR swaps newECRClient for the duration of the test.
func withMockECR(t *testing.T, m ecrClientIface) {
	t.Helper()
	orig := newECRClient
	newECRClient = func(cfg aws.Config) ecrClientIface { return m }
	t.Cleanup(func() { newECRClient = orig })
}

func TestEnsureECRRepoReturnsExisting(t *testing.T) {
	m := &mockECR{describeURI: "123.dkr.ecr/demo"}
	withMockECR(t, m)
	e := &DockerEngine{cli: &mockDockerClient{}}
	uri, err := e.EnsureECRRepo(context.Background(), aws.Config{}, "demo")
	if err != nil {
		t.Fatalf("EnsureECRRepo: %v", err)
	}
	if uri != "123.dkr.ecr/demo" {
		t.Errorf("uri = %q", uri)
	}
	if m.createCalled {
		t.Error("CreateRepository should not be called when the repo exists")
	}
}

func TestEnsureECRRepoCreatesWhenAbsent(t *testing.T) {
	m := &mockECR{
		describeErr: &ecrtypes.RepositoryNotFoundException{},
		createURI:   "123.dkr.ecr/demo",
	}
	withMockECR(t, m)
	e := &DockerEngine{cli: &mockDockerClient{}}
	uri, err := e.EnsureECRRepo(context.Background(), aws.Config{}, "demo")
	if err != nil {
		t.Fatalf("EnsureECRRepo: %v", err)
	}
	if !m.createCalled {
		t.Error("CreateRepository should be called when the repo is absent")
	}
	if uri != "123.dkr.ecr/demo" {
		t.Errorf("uri = %q", uri)
	}
}

func TestPushImageSuccess(t *testing.T) {
	m := &mockECR{
		authToken: base64.StdEncoding.EncodeToString([]byte("AWS:secret")),
		proxy:     "https://123.dkr.ecr",
	}
	withMockECR(t, m)
	dc := &mockDockerClient{pushBody: `{"status":"Pushed"}`}
	e := &DockerEngine{cli: dc}
	if err := e.PushImage(context.Background(), aws.Config{}, "123.dkr.ecr/demo"); err != nil {
		t.Fatalf("PushImage: %v", err)
	}
}

func TestPushImageSurfacesStreamError(t *testing.T) {
	m := &mockECR{
		authToken: base64.StdEncoding.EncodeToString([]byte("AWS:secret")),
		proxy:     "https://123.dkr.ecr",
	}
	withMockECR(t, m)
	dc := &mockDockerClient{pushBody: `{"error":"push access denied"}`}
	e := &DockerEngine{cli: dc}
	err := e.PushImage(context.Background(), aws.Config{}, "123.dkr.ecr/demo")
	if err == nil || err.Error() != "push access denied" {
		t.Fatalf("error = %v, want 'push access denied'", err)
	}
}

func TestPushImageMalformedToken(t *testing.T) {
	m := &mockECR{
		authToken: base64.StdEncoding.EncodeToString([]byte("no-colon")),
		proxy:     "https://x",
	}
	withMockECR(t, m)
	e := &DockerEngine{cli: &mockDockerClient{}}
	err := e.PushImage(context.Background(), aws.Config{}, "uri")
	if err == nil {
		t.Fatal("expected malformed-token error")
	}
}

func TestResolveDockerfileCustomMissing(t *testing.T) {
	_, err := resolveDockerfile(filepath.Join(t.TempDir(), "nope"), "aws")
	if err == nil {
		t.Fatal("expected error for missing custom Dockerfile")
	}
}

func TestResolveDockerfileEmbeddedUnknownProvider(t *testing.T) {
	_, err := resolveDockerfile("", "nonexistent-provider")
	if err == nil {
		t.Fatal("expected error for unknown embedded provider")
	}
}

func TestResolveDockerfileCustomUsed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Dockerfile")
	want := []byte("FROM scratch\n")
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := resolveDockerfile(p, "aws")
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
