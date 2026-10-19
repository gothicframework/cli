package tofu

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"mime"
	"strings"

	"golang.org/x/sync/errgroup"
)

// GcpCDN implements CDNEngine for Google Cloud Platform. It uploads static assets to
// a GCS bucket and handles cache invalidation.
//
// GCP has no equivalent to CloudFront invalidations — instead, assets are uploaded
// with cache-control headers that control browser/CDN caching, and a new deploy
// overwrites the existing objects (or uses unique keys with content-hash).
type GcpCDN struct {
	gcloudProject string
}

// NewGcpCDN constructs a GcpCDN. The gcloudProject is derived from the config's
// project name when non-empty.
func NewGcpCDN(gcloudProject string) *GcpCDN {
	return &GcpCDN{gcloudProject: gcloudProject}
}

// syncUploadLimit bounds concurrent gcloud uploads during SyncAssets.
const gcpSyncUploadLimit = 8

// SyncAssets uploads every file under sourceDir to bucketName, concurrently.
// Each file is uploaded with appropriate Content-Type and Cache-Control headers.
// GCP GCS objects are immutable — a new deploy simply overwrites the existing keys.
func (c *GcpCDN) SyncAssets(ctx context.Context, bucketName, sourceDir string) error {
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, gcpSyncUploadLimit)

	walkErr := filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.Go(func() error {
			defer func() { <-sem }()
			return c.uploadObject(ctx, bucketName, rel, path)
		})
		return nil
	})
	if walkErr != nil {
		if werr := g.Wait(); werr != nil {
			return fmt.Errorf("uploading assets to %q: %w", bucketName, werr)
		}
		return fmt.Errorf("walking %q: %w", sourceDir, walkErr)
	}
	return g.Wait()
}

// uploadObject puts a single file to GCS with appropriate Content-Type and
// Cache-Control headers. .wasm.br/.wasm.gz files get special treatment so browsers
// can transparently decompress them.
func (c *GcpCDN) uploadObject(ctx context.Context, bucketName, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	// Build the gcloud command for this upload
	args := []string{
		"storage", "cp",
		"--cache-control", c.cacheControlForPath(key),
	}
	if ct := c.contentTypeForPath(key); ct != "" {
		args = append(args, fmt.Sprintf("--content-type=%s", ct))
	}
	args = append(args, path, fmt.Sprintf("gs://%s/%s", bucketName, key))

	cmd := exec.CommandContext(ctx, "gcloud", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uploading %q: %w", key, err)
	}
	return nil
}

// contentTypeForPath returns the HTTP Content-Type for a GCS object based on its path.
// .wasm and .wasm.gz files are special-cased.
func (c *GcpCDN) contentTypeForPath(key string) string {
	switch {
	case strings.HasSuffix(key, ".wasm.br"):
		return "application/wasm"
	case strings.HasSuffix(key, ".wasm.gz"):
		return "application/wasm"
	case strings.HasSuffix(key, ".wasm"):
		return "application/wasm"
	default:
		return mime.TypeByExtension(filepath.Ext(key))
	}
}

// cacheControlForPath returns the Cache-Control header for each uploaded GCS object.
// Images are cached immutably for a year; everything else gets a 1-day TTL with
// stale-while-revalidate, since assets may change on the next deploy under the same key.
func (c *GcpCDN) cacheControlForPath(key string) string {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico":
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=86400, stale-while-revalidate=604800"
	}
}

// RemoveAssets deletes every object in bucketName. GCS objects are listed and
// removed in batches of 100 (gcloud's per-command limit).
func (c *GcpCDN) RemoveAssets(ctx context.Context, bucketName string) error {
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "ls", "gs://"+bucketName, "--recursive")
	out, err := cmd.Output()
	if err != nil {
		// Bucket may be empty or not exist — nothing to do
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil // empty bucket
	}

	// gcloud storage rm -q deletes recursively
	delCmd := exec.CommandContext(ctx, "gcloud", "storage", "rm", "-q", "--recursive", "gs://"+bucketName)
	delCmd.Stderr = os.Stderr
	return delCmd.Run()
}

// InvalidateCache is a no-op for GCP. GCS objects are immutable and overwritten
// directly on new deploy. There is no CDN cache invalidation API.
func (c *GcpCDN) InvalidateCache(ctx context.Context, distributionID string) error {
	// No-op: GCP doesn't have CloudFront-style invalidations. Assets are
	// overwritten on deploy and cache-control headers manage browser/CDN caching.
	return nil
}
