package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"
)

// dockerClientIface is the subset of the Docker SDK client used by DockerEngine.
// It is declared as an interface so tests can inject a mock client; the concrete
// *dockerclient.Client satisfies it.
type dockerClientIface interface {
	Ping(ctx context.Context) (types.Ping, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error)
	ImageTag(ctx context.Context, source, target string) error
	ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error)
	Close() error
}

// ecrClientIface is the subset of the ECR SDK client used by DockerEngine.
type ecrClientIface interface {
	DescribeRepositories(ctx context.Context, params *ecr.DescribeRepositoriesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
	CreateRepository(ctx context.Context, params *ecr.CreateRepositoryInput, optFns ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
	GetAuthorizationToken(ctx context.Context, params *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// newECRClient is overridable in tests so ECR calls can be mocked.
var newECRClient = func(cfg aws.Config) ecrClientIface {
	return ecr.NewFromConfig(cfg)
}

// buildContextSkipDirs are top-level names (directories or files) excluded from
// the tar build context sent to the Docker daemon: large dev-only directories,
// plus the Go workspace files. A go.work points at absolute host paths that do
// not exist inside the image, so shipping it breaks `go build` in the container
// ("cannot load module … listed in go.work file"). The walk below treats a
// matched non-dir entry as a plain skip, so listing the files here works.
var buildContextSkipDirs = []string{".gothicCli", "optimize", "node_modules", ".git", "go.work", "go.work.sum"}

// DockerEngine builds the Gothic Lambda image and pushes it to ECR.
type DockerEngine struct {
	cli dockerClientIface
}

// NewDockerEngine constructs a DockerEngine connected to the local Docker daemon
// via environment configuration, negotiating the API version with the daemon.
func NewDockerEngine() (*DockerEngine, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating Docker client: %w", err)
	}
	return &DockerEngine{cli: c}, nil
}

// Close releases the underlying Docker client resources.
func (e *DockerEngine) Close() error {
	if e.cli == nil {
		return nil
	}
	return e.cli.Close()
}

// CheckDaemon verifies the Docker daemon is reachable. The returned error always
// contains the substring "Docker daemon is not running" so callers and tests can
// assert on it.
func (e *DockerEngine) CheckDaemon(ctx context.Context) error {
	if _, err := e.cli.Ping(ctx); err != nil {
		return fmt.Errorf("Docker daemon is not running or unreachable: %w", err)
	}
	return nil
}

// resolveDockerfile returns the Dockerfile bytes. When dockerfilePath is set the
// file at that path is used; otherwise the embedded Dockerfile for the provider
// is returned.
func resolveDockerfile(dockerfilePath, provider string) ([]byte, error) {
	if dockerfilePath != "" {
		b, err := os.ReadFile(dockerfilePath)
		if err != nil {
			return nil, fmt.Errorf("reading custom Dockerfile %q: %w", dockerfilePath, err)
		}
		return b, nil
	}
	embedded := filepath.ToSlash(filepath.Join("embedded", provider, "Dockerfile"))
	b, err := DockerfileFS.ReadFile(embedded)
	if err != nil {
		return nil, fmt.Errorf("reading embedded Dockerfile for provider %q: %w", provider, err)
	}
	return b, nil
}

// buildTarContext walks projectRoot and produces a tar archive suitable as a
// Docker build context. The resolved Dockerfile bytes are injected as a
// top-level entry named "Dockerfile", overriding any Dockerfile on disk.
func buildTarContext(projectRoot string, dockerfile []byte) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)

	walkErr := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		// Skip excluded directories entirely.
		top := strings.SplitN(relSlash, "/", 2)[0]
		for _, skip := range buildContextSkipDirs {
			if top == skip {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		// The Dockerfile entry is injected separately below; never tar a file
		// that would collide with the injected name.
		if relSlash == "Dockerfile" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Only regular files are added; symlinks/devices are skipped.
		if !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		return nil, fmt.Errorf("building tar context: %w", walkErr)
	}

	// Inject the resolved Dockerfile.
	hdr := &tar.Header{
		Name: "Dockerfile",
		Mode: 0o644,
		Size: int64(len(dockerfile)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		_ = tw.Close()
		return nil, fmt.Errorf("writing Dockerfile tar header: %w", err)
	}
	if _, err := tw.Write(dockerfile); err != nil {
		_ = tw.Close()
		return nil, fmt.Errorf("writing Dockerfile to tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar context: %w", err)
	}
	return buf, nil
}

// BuildImage builds the container image for the given provider. dockerfilePath,
// when non-empty, overrides the embedded provider Dockerfile. The image is
// tagged as imageName:tag.
func (e *DockerEngine) BuildImage(ctx context.Context, projectRoot, dockerfilePath, imageName, tag, provider string) error {
	dockerfile, err := resolveDockerfile(dockerfilePath, provider)
	if err != nil {
		return err
	}
	buf, err := buildTarContext(projectRoot, dockerfile)
	if err != nil {
		return err
	}
	resp, err := e.cli.ImageBuild(ctx, buf, build.ImageBuildOptions{
		Tags:       []string{imageName + ":" + tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("starting image build: %w", err)
	}
	defer resp.Body.Close()
	return streamDockerJSON(resp.Body, os.Stdout)
}

// EnsureECRRepo returns the URI of the ECR repository named repoName, creating it
// if it does not already exist.
func (e *DockerEngine) EnsureECRRepo(ctx context.Context, awsCfg aws.Config, repoName string) (string, error) {
	client := newECRClient(awsCfg)
	out, err := client.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{repoName},
	})
	if err == nil {
		if len(out.Repositories) == 0 || out.Repositories[0].RepositoryUri == nil {
			return "", fmt.Errorf("ECR repository %q returned no URI", repoName)
		}
		return *out.Repositories[0].RepositoryUri, nil
	}

	var notFound *ecrtypes.RepositoryNotFoundException
	if !errors.As(err, &notFound) && !strings.Contains(err.Error(), "RepositoryNotFoundException") {
		return "", fmt.Errorf("describing ECR repository %q: %w", repoName, err)
	}

	created, err := client.CreateRepository(ctx, &ecr.CreateRepositoryInput{
		RepositoryName: aws.String(repoName),
	})
	if err != nil {
		return "", fmt.Errorf("creating ECR repository %q: %w", repoName, err)
	}
	if created.Repository == nil || created.Repository.RepositoryUri == nil {
		return "", fmt.Errorf("created ECR repository %q returned no URI", repoName)
	}
	return *created.Repository.RepositoryUri, nil
}

// PushImage authenticates against ECR and pushes the exact image reference given
// (repo:<immutable-tag>). Mid-stream push failures (which the Docker SDK reports
// inside the JSON stream rather than as a Go error) are surfaced as errors.
//
// The reference MUST be immutable (a unique per-deploy tag), never repo:latest: an
// image-based Lambda pins to the digest resolved at apply time and does not re-pull
// a moving tag, and a constant image_uri string makes tofu see no diff on redeploy —
// so the Lambda would keep running the previous image forever.
func (e *DockerEngine) PushImage(ctx context.Context, awsCfg aws.Config, imageURI string) error {
	client := newECRClient(awsCfg)
	tok, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return fmt.Errorf("getting ECR authorization token: %w", err)
	}
	if len(tok.AuthorizationData) == 0 || tok.AuthorizationData[0].AuthorizationToken == nil {
		return errors.New("ECR returned no authorization data")
	}
	authData := tok.AuthorizationData[0]
	decoded, err := base64.StdEncoding.DecodeString(*authData.AuthorizationToken)
	if err != nil {
		return fmt.Errorf("decoding ECR auth token: %w", err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return errors.New("malformed ECR auth token")
	}
	serverAddress := ""
	if authData.ProxyEndpoint != nil {
		serverAddress = *authData.ProxyEndpoint
	}
	authConfig := registry.AuthConfig{
		Username:      parts[0],
		Password:      parts[1],
		ServerAddress: serverAddress,
	}
	authJSON, err := json.Marshal(authConfig)
	if err != nil {
		return fmt.Errorf("marshaling registry auth: %w", err)
	}
	encodedAuth := base64.URLEncoding.EncodeToString(authJSON)

	// imageURI is the freshly-built immutable reference (repo:<timestamp>) — the
	// same tag the Lambda's ecr_image_uri var points at. Push it as-is so each
	// deploy publishes a distinct tag and tofu updates the Lambda to the new image.
	body, err := e.cli.ImagePush(ctx, imageURI, image.PushOptions{RegistryAuth: encodedAuth})
	if err != nil {
		return fmt.Errorf("starting image push: %w", err)
	}
	defer body.Close()
	return streamDockerJSON(body, os.Stdout)
}

// streamDockerJSON drains a Docker SDK JSON progress stream, writing the Stream
// field of each line to out, and returns an error if any line reports one.
func streamDockerJSON(r io.Reader, out io.Writer) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decoding Docker stream: %w", err)
		}
		if msg.Stream != "" {
			_, _ = io.WriteString(out, msg.Stream)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}
