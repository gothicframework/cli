package tofu

import "context"

// CDNEngine is the provider-agnostic contract for syncing static assets to the
// origin bucket and invalidating the CDN cache. Concrete implementations (e.g.
// CloudFrontCDN) are filled in Phase 6.
type CDNEngine interface {
	// SyncAssets does a single-pass concurrent upload of localDir to bucketName.
	SyncAssets(ctx context.Context, bucketName string, localDir string) error

	// RemoveAssets deletes uploaded assets from bucketName.
	RemoveAssets(ctx context.Context, bucketName string) error

	// InvalidateCache creates a /* invalidation on the CDN distribution.
	InvalidateCache(ctx context.Context, distributionID string) error
}
