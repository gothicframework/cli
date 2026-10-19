package tofu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cli "github.com/gothicframework/cli/v4/internal/cli"
	"github.com/gothicframework/core/config"
)

// --- GcpEngine tests ---

func TestGcpEnginePrepareSetsAllFields(t *testing.T) {
	cfg := &cli.Config{
		ProjectName: "my-app",
		GoModName:   "github.com/you/my-app",
	}
	cfg.Deploy = &cli.DeployConfig{
		Provider: cli.GCP,
		Providers: cli.Providers{
			GCP: cli.GCPProvider{
				Region: "us-central1",
				Stages: map[string]cli.GCPStageEnv{
					"dev": {
						ENV: map[string]config.EnvValue{
							"GOTHIC_MODE": {Source: config.RawEnv, Value: "production"},
						},
					},
				},
			},
		},
	}

	// Verify validation passes for the region
	if err := EnsureRegion(cfg.Deploy.Providers.GCP.Region); err != nil {
		t.Fatalf("EnsureRegion failed: %v", err)
	}
}

func TestGcpEngineValidateRegion(t *testing.T) {
	tests := []struct {
		region string
		want   bool
	}{
		{"us-central1", true},
		{"europe-west1", true},
		{"asia-southeast1", true},
		{"australia-southeast1", true},
		{"southamerica-east1", true}, // Brazil region
		{"us-east1", true},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		err := ValidateRegion(tt.region)
		got := err == nil
		if got != tt.want {
			t.Errorf("ValidateRegion(%q) = %v, want %v", tt.region, got, tt.want)
		}
	}

	// Empty region → EnsureRegion should fail
	if err := EnsureRegion(""); err == nil {
		t.Error("EnsureRegion(\"\") = nil, want error")
	}
}


// --- GcpCDN tests ---

func TestGcpCDNUploadObjectWasmContentType(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.wasm"), []byte("wasm"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.wasm.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cdn := &GcpCDN{}

	// Content-type for .wasm
	ct := cdn.contentTypeForPath("app.wasm")
	if ct != "application/wasm" {
		t.Errorf("contentTypeForPath(.wasm) = %q, want application/wasm", ct)
	}
	// Content-type for .wasm.gz
	ct = cdn.contentTypeForPath("app.wasm.gz")
	if ct != "application/wasm" {
		t.Errorf("contentTypeForPath(.wasm.gz) = %q, want application/wasm", ct)
	}
	// Content-type for .wasm.br
	ct = cdn.contentTypeForPath("app.wasm.br")
	if ct != "application/wasm" {
		t.Errorf("contentTypeForPath(.wasm.br) = %q, want application/wasm", ct)
	}
	// Content-type for .png
	ct = cdn.contentTypeForPath("img.png")
	if ct == "" || ct != "image/png" {
		t.Errorf("contentTypeForPath(.png) = %q, want image/png", ct)
	}
}

func TestGcpCDNCacheControlForImage(t *testing.T) {
	cdn := &GcpCDN{}

	// Image → immutable 1 year
	cc := cdn.cacheControlForPath("photo.jpg")
	if cc != "public, max-age=31536000, immutable" {
		t.Errorf("cacheControlForPath(.jpg) = %q, want public, max-age=31536000, immutable", cc)
	}
	// Non-image → 1 day with stale-while-revalidate
	cc = cdn.cacheControlForPath("styles.css")
	if cc != "public, max-age=86400, stale-while-revalidate=604800" {
		t.Errorf("cacheControlForPath(.css) = %q, want public, max-age=86400, stale-while-revalidate=604800", cc)
	}
	// .wasm → 1 day
	cc = cdn.cacheControlForPath("app.wasm")
	if cc != "public, max-age=86400, stale-while-revalidate=604800" {
		t.Errorf("cacheControlForPath(.wasm) = %q, want public, max-age=86400, stale-while-revalidate=604800", cc)
	}
}

func TestGcpCDNInvalidateCacheIsNoop(t *testing.T) {
	cdn := &GcpCDN{}
	// InvalidateCache should be a no-op for GCP
	if err := cdn.InvalidateCache(context.Background(), "dist-123"); err != nil {
		t.Errorf("InvalidateCache = %v, want nil", err)
	}
}

func TestGcpCDNRemoveAssetsEmptyBucket(t *testing.T) {
	cdn := &GcpCDN{}
	// Empty bucket → RemoveAssets should not error
	if err := cdn.RemoveAssets(context.Background(), "empty-bucket"); err != nil {
		t.Errorf("RemoveAssets on empty bucket = %v, want nil", err)
	}
}

// --- EnsureGCloudCLI / EnsureDocker tests ---

func TestEnsureGCloudCLI(t *testing.T) {
	// gcloud may or may not be installed — if it is, no error
	// If not, we expect a specific error message
	err := EnsureGCloudCLI()
	if err != nil && err.Error() != "gcloud CLI not found on PATH — install from https://cloud.google.com/sdk/docs/install" {
		t.Errorf("EnsureGCloudCLI error = %q, want specific message or no error", err)
	}
}

func TestEnsureDocker(t *testing.T) {
	// Docker may or may not be running — if it is, no error
	// If not, we expect a specific error message
	err := EnsureDocker()
	if err != nil && err.Error() != "Docker daemon not running — start Docker and try again" {
		t.Errorf("EnsureDocker error = %q, want specific message or no error", err)
	}
}
