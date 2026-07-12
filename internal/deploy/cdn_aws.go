package tofu

import (
	"context"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"
)

// s3CDNIface is the subset of the S3 SDK client used by CloudFrontCDN. It is an
// interface so tests can inject a mock; the concrete *s3.Client satisfies it.
type s3CDNIface interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// cfIface is the subset of the CloudFront SDK client used by CloudFrontCDN.
type cfIface interface {
	CreateInvalidation(ctx context.Context, params *cloudfront.CreateInvalidationInput, optFns ...func(*cloudfront.Options)) (*cloudfront.CreateInvalidationOutput, error)
}

// CloudFrontCDN implements CDNEngine for AWS: it uploads static assets to the
// origin S3 bucket and invalidates the CloudFront distribution cache.
type CloudFrontCDN struct {
	s3 s3CDNIface
	cf cfIface
}

// NewCloudFrontCDN constructs a CloudFrontCDN from a loaded aws.Config.
func NewCloudFrontCDN(awsCfg aws.Config) *CloudFrontCDN {
	return &CloudFrontCDN{
		s3: s3.NewFromConfig(awsCfg),
		cf: cloudfront.NewFromConfig(awsCfg),
	}
}

// syncUploadLimit bounds concurrent PutObject calls during SyncAssets.
const syncUploadLimit = 8

// assetKeyPrefix is the S3 key prefix every static asset is uploaded under. It
// MUST mirror the CloudFront `/public/*` cache behavior (resources.tf.json) and
// the `/public/...` asset URLs the layout emits: CloudFront forwards the full
// request path to S3 (it does NOT strip the path-pattern prefix), so a request
// for `/public/styles.css` looks up the S3 key `public/styles.css`. Uploading
// assets without this prefix makes every asset 403 (a missing key returns 403,
// not 404, because the OAC bucket policy grants GetObject but not ListBucket).
const assetKeyPrefix = "public"

// SyncAssets uploads every file under sourceDir to bucketName, concurrently
// (bounded to syncUploadLimit). Content-Type is inferred from the extension;
// .wasm and .wasm.gz files are special-cased so CloudFront serves them with the
// correct type and (for .gz) Content-Encoding in a single PutObject pass.
func (c *CloudFrontCDN) SyncAssets(ctx context.Context, bucketName, sourceDir string) error {
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, syncUploadLimit)

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
		// Mirror the local public/ dir under the S3 public/ prefix so keys match
		// the /public/* paths CloudFront forwards (see assetKeyPrefix).
		key := assetKeyPrefix + "/" + filepath.ToSlash(rel)

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.Go(func() error {
			defer func() { <-sem }()
			return c.uploadObject(ctx, bucketName, key, path)
		})
		return nil
	})
	if walkErr != nil {
		// A failed upload cancels the errgroup context, which makes the walk return
		// "context canceled" — that masks the real cause. Prefer g.Wait()'s error
		// (the actual upload failure) when the walk was only cancelled downstream.
		if werr := g.Wait(); werr != nil {
			return fmt.Errorf("uploading assets to %q: %w", bucketName, werr)
		}
		return fmt.Errorf("walking %q: %w", sourceDir, walkErr)
	}
	return g.Wait()
}

// uploadObject puts a single file, applying the wasm/gzip content-type rules.
func (c *CloudFrontCDN) uploadObject(ctx context.Context, bucketName, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	input := &s3.PutObjectInput{
		Bucket:       aws.String(bucketName),
		Key:          aws.String(key),
		Body:         f,
		CacheControl: aws.String(cacheControlForKey(key)),
	}

	switch {
	case strings.HasSuffix(key, ".wasm.br"):
		// Brotli-precompressed module. Both headers are required: the browser's
		// WebAssembly.instantiateStreaming rejects anything whose Content-Type is
		// not application/wasm ("Incorrect response MIME type"), and Content-Encoding
		// br is what makes it transparently decompress the body before compiling.
		input.ContentType = aws.String("application/wasm")
		input.ContentEncoding = aws.String("br")
	case strings.HasSuffix(key, ".wasm.gz"):
		input.ContentType = aws.String("application/wasm")
		input.ContentEncoding = aws.String("gzip")
	case strings.HasSuffix(key, ".wasm"):
		input.ContentType = aws.String("application/wasm")
	default:
		if ct := mime.TypeByExtension(filepath.Ext(key)); ct != "" {
			input.ContentType = aws.String(ct)
		}
	}

	if _, err := c.s3.PutObject(ctx, input); err != nil {
		return fmt.Errorf("uploading %q: %w", key, err)
	}
	return nil
}

// cacheControlForKey returns the Cache-Control header stored on each uploaded S3
// object. Without one, CloudFront (CachingOptimized) serves the /public/* assets
// with no browser TTL — Lighthouse's "Use efficient cache lifetimes" audit then
// flags every asset as uncached.
//
//   - Media (images) are content-stable and the heaviest payload, so they are
//     cached immutably for a year (the biggest win). If you ever replace an image
//     under the same key, rely on the per-deploy CloudFront /* invalidation for the
//     edge, or rename it to bust returning browsers.
//   - Everything else (styles.css, the per-page .wasm[.br|.gz], etc.) can change on
//     the next deploy under the SAME key, and browsers ignore CloudFront
//     invalidations, so `immutable` would pin a stale asset. These get a 1-day TTL
//     with a week of stale-while-revalidate — a real TTL that stays safe across
//     deploys.
func cacheControlForKey(key string) string {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico":
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=86400, stale-while-revalidate=604800"
	}
}

// RemoveAssets deletes every object in bucketName, batching deletes into the
// 1000-object groups DeleteObjects accepts.
func (c *CloudFrontCDN) RemoveAssets(ctx context.Context, bucketName string) error {
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	var batch []s3types.ObjectIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3types.Delete{Objects: batch},
		})
		batch = batch[:0]
		if err != nil {
			return fmt.Errorf("deleting objects from %q: %w", bucketName, err)
		}
		return nil
	}

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing objects in %q: %w", bucketName, err)
		}
		for _, obj := range page.Contents {
			batch = append(batch, s3types.ObjectIdentifier{Key: obj.Key})
			if len(batch) == 1000 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

// InvalidateCache creates a /* invalidation on the given distribution. The
// caller reference is the current nanosecond timestamp so each call is unique.
func (c *CloudFrontCDN) InvalidateCache(ctx context.Context, distID string) error {
	_, err := c.cf.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(distID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: aws.String(fmt.Sprint(time.Now().UnixNano())),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(1),
				Items:    []string{"/*"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating CloudFront invalidation for %q: %w", distID, err)
	}
	return nil
}
