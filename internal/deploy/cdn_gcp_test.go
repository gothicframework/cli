package tofu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// --- GcpCDN SyncAssets tests ---

func TestGcpCDNUploadObjectGzipContentTypeAndEncoding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.wasm.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cdn := &GcpCDN{}

	// Content-type for .wasm.gz should be application/wasm
	ct := cdn.contentTypeForPath("app.wasm.gz")
	if ct != "application/wasm" {
		t.Errorf("contentTypeForPath(.wasm.gz) = %q, want application/wasm", ct)
	}

	// Cache-control for .wasm.gz should be 1-day TTL
	cc := cdn.cacheControlForPath("app.wasm.gz")
	if cc != "public, max-age=86400, stale-while-revalidate=604800" {
		t.Errorf("cacheControlForPath(.wasm.gz) = %q, want public, max-age=86400, stale-while-revalidate=604800", cc)
	}
}

func TestGcpCDNUploadObjectBrotliContentTypeAndEncoding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.wasm.br"), []byte("br"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cdn := &GcpCDN{}

	// Content-type for .wasm.br should be application/wasm
	ct := cdn.contentTypeForPath("app.wasm.br")
	if ct != "application/wasm" {
		t.Errorf("contentTypeForPath(.wasm.br) = %q, want application/wasm", ct)
	}

	// Cache-control for .wasm.br should be 1-day TTL
	cc := cdn.cacheControlForPath("app.wasm.br")
	if cc != "public, max-age=86400, stale-while-revalidate=604800" {
		t.Errorf("cacheControlForPath(.wasm.br) = %q, want public, max-age=86400, stale-while-revalidate=604800", cc)
	}
}

func TestGcpCDNUploadObjectImageContentType(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "image.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cdn := &GcpCDN{}

	// Content-type for .png should be image/png
	ct := cdn.contentTypeForPath("image.png")
	if ct != "image/png" {
		t.Errorf("contentTypeForPath(.png) = %q, want image/png", ct)
	}

	// Cache-control for .png should be immutable 1 year
	cc := cdn.cacheControlForPath("image.png")
	if cc != "public, max-age=31536000, immutable" {
		t.Errorf("cacheControlForPath(.png) = %q, want public, max-age=31536000, immutable", cc)
	}
}

func TestGcpCDNInvalidateCacheNoop(t *testing.T) {
	cdn := &GcpCDN{}
	// InvalidateCache should be a no-op for GCP
	if err := cdn.InvalidateCache(context.Background(), "dist-123"); err != nil {
		t.Errorf("InvalidateCache = %v, want nil", err)
	}
}
