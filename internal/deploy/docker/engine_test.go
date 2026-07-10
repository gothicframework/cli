package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
)

// mockDockerClient implements dockerClientIface for tests.
type mockDockerClient struct {
	pingErr error

	// build
	buildContext []byte // captured tar bytes
	buildBody    string // JSON stream returned by ImageBuild
	buildErr     error

	tagErr   error
	pushBody string
	pushErr  error
}

func (m *mockDockerClient) Ping(ctx context.Context) (types.Ping, error) {
	return types.Ping{}, m.pingErr
}

func (m *mockDockerClient) ImageBuild(ctx context.Context, buildContext io.Reader, _ build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	if m.buildErr != nil {
		return build.ImageBuildResponse{}, m.buildErr
	}
	m.buildContext, _ = io.ReadAll(buildContext)
	body := m.buildBody
	if body == "" {
		body = `{"stream":"ok"}`
	}
	return build.ImageBuildResponse{Body: io.NopCloser(bytes.NewBufferString(body))}, nil
}

func (m *mockDockerClient) ImageTag(ctx context.Context, source, target string) error {
	return m.tagErr
}

func (m *mockDockerClient) ImagePush(ctx context.Context, ref string, _ image.PushOptions) (io.ReadCloser, error) {
	if m.pushErr != nil {
		return nil, m.pushErr
	}
	return io.NopCloser(bytes.NewBufferString(m.pushBody)), nil
}

func (m *mockDockerClient) Close() error { return nil }

// tarEntry returns the bytes of the named entry in a tar archive, or nil.
func tarEntry(t *testing.T, archive []byte, name string) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == name {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read entry: %v", err)
			}
			return b
		}
	}
}

func TestBuildImageUsesEmbeddedDockerfileWhenPathEmpty(t *testing.T) {
	want, err := DockerfileFS.ReadFile("embedded/aws/Dockerfile")
	if err != nil {
		t.Fatalf("read embedded Dockerfile: %v", err)
	}

	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := &mockDockerClient{}
	e := &DockerEngine{cli: m}
	if err := e.BuildImage(context.Background(), projectRoot, "", "img", "tag", "aws"); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	got := tarEntry(t, m.buildContext, "Dockerfile")
	if !bytes.Equal(got, want) {
		t.Errorf("injected Dockerfile does not match the embedded aws Dockerfile")
	}
}

func TestBuildImageUsesCustomDockerfileWhenPathSet(t *testing.T) {
	projectRoot := t.TempDir()
	custom := filepath.Join(t.TempDir(), "Dockerfile.custom")
	want := []byte("FROM scratch\nLABEL custom=yes\n")
	if err := os.WriteFile(custom, want, 0o644); err != nil {
		t.Fatalf("write custom: %v", err)
	}

	m := &mockDockerClient{}
	e := &DockerEngine{cli: m}
	if err := e.BuildImage(context.Background(), projectRoot, custom, "img", "tag", "aws"); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	got := tarEntry(t, m.buildContext, "Dockerfile")
	if !bytes.Equal(got, want) {
		t.Errorf("injected Dockerfile = %q, want custom file bytes %q", got, want)
	}
}

func TestBuildImageSurfacesStreamError(t *testing.T) {
	projectRoot := t.TempDir()
	m := &mockDockerClient{buildBody: `{"error":"push denied"}`}
	e := &DockerEngine{cli: m}
	err := e.BuildImage(context.Background(), projectRoot, "", "img", "tag", "aws")
	if err == nil {
		t.Fatal("expected stream error")
	}
	if err.Error() != "push denied" {
		t.Errorf("error = %q, want %q", err.Error(), "push denied")
	}
}

func TestCheckDaemonErrorMentionsNotRunning(t *testing.T) {
	m := &mockDockerClient{pingErr: errors.New("connection refused")}
	e := &DockerEngine{cli: m}
	err := e.CheckDaemon(context.Background())
	if err == nil {
		t.Fatal("expected error from CheckDaemon")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("Docker daemon is not running")) {
		t.Errorf("error = %q, want it to contain 'Docker daemon is not running'", err.Error())
	}
}

func TestCheckDaemonSucceedsWhenReachable(t *testing.T) {
	e := &DockerEngine{cli: &mockDockerClient{}}
	if err := e.CheckDaemon(context.Background()); err != nil {
		t.Errorf("CheckDaemon: %v", err)
	}
}
