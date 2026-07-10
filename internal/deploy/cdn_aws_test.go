package tofu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// --- mocks ---

type mockCDNS3 struct {
	mu sync.Mutex

	putInputs    []*s3.PutObjectInput
	deleteCalls  int
	deletedKeys  int
	listPages    [][]s3types.Object // successive pages returned by ListObjectsV2
	listPageIdx  int
}

func (m *mockCDNS3) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putInputs = append(m.putInputs, in)
	return &s3.PutObjectOutput{}, nil
}

func (m *mockCDNS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &s3.ListObjectsV2Output{}
	if m.listPageIdx < len(m.listPages) {
		out.Contents = m.listPages[m.listPageIdx]
		m.listPageIdx++
	}
	// IsTruncated drives the paginator to request another page.
	more := m.listPageIdx < len(m.listPages)
	out.IsTruncated = aws.Bool(more)
	if more {
		out.NextContinuationToken = aws.String("tok" + strconv.Itoa(m.listPageIdx))
	}
	return out, nil
}

func (m *mockCDNS3) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	m.deletedKeys += len(in.Delete.Objects)
	return &s3.DeleteObjectsOutput{}, nil
}

type mockCF struct {
	lastInput *cloudfront.CreateInvalidationInput
}

func (m *mockCF) CreateInvalidation(ctx context.Context, in *cloudfront.CreateInvalidationInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateInvalidationOutput, error) {
	m.lastInput = in
	return &cloudfront.CreateInvalidationOutput{}, nil
}

// --- tests ---

func TestSyncAssetsWasmGzContentTypeAndEncoding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.wasm.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s3m := &mockCDNS3{}
	cdn := &CloudFrontCDN{s3: s3m, cf: &mockCF{}}

	if err := cdn.SyncAssets(context.Background(), "bucket", dir); err != nil {
		t.Fatalf("SyncAssets: %v", err)
	}
	if len(s3m.putInputs) != 1 {
		t.Fatalf("expected 1 PutObject, got %d", len(s3m.putInputs))
	}
	in := s3m.putInputs[0]
	if in.ContentType == nil || *in.ContentType != "application/wasm" {
		t.Errorf("ContentType = %v, want application/wasm", in.ContentType)
	}
	if in.ContentEncoding == nil || *in.ContentEncoding != "gzip" {
		t.Errorf("ContentEncoding = %v, want gzip", in.ContentEncoding)
	}
}

func TestSyncAssetsWasmContentType(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.wasm"), []byte("w"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s3m := &mockCDNS3{}
	cdn := &CloudFrontCDN{s3: s3m, cf: &mockCF{}}

	if err := cdn.SyncAssets(context.Background(), "bucket", dir); err != nil {
		t.Fatalf("SyncAssets: %v", err)
	}
	if len(s3m.putInputs) != 1 {
		t.Fatalf("expected 1 PutObject, got %d", len(s3m.putInputs))
	}
	in := s3m.putInputs[0]
	if in.ContentType == nil || *in.ContentType != "application/wasm" {
		t.Errorf("ContentType = %v, want application/wasm", in.ContentType)
	}
	if in.ContentEncoding != nil {
		t.Errorf("ContentEncoding = %v, want nil for plain .wasm", *in.ContentEncoding)
	}
}

func TestRemoveAssetsBatchesInto1000Groups(t *testing.T) {
	// 2500 keys across the paginator → batches of 1000,1000,500 → 3 DeleteObjects.
	var page []s3types.Object
	for i := 0; i < 2500; i++ {
		page = append(page, s3types.Object{Key: aws.String("k" + strconv.Itoa(i))})
	}
	s3m := &mockCDNS3{listPages: [][]s3types.Object{page}}
	cdn := &CloudFrontCDN{s3: s3m, cf: &mockCF{}}

	if err := cdn.RemoveAssets(context.Background(), "bucket"); err != nil {
		t.Fatalf("RemoveAssets: %v", err)
	}
	if s3m.deleteCalls != 3 {
		t.Errorf("DeleteObjects called %d times, want 3", s3m.deleteCalls)
	}
	if s3m.deletedKeys != 2500 {
		t.Errorf("deleted %d keys, want 2500", s3m.deletedKeys)
	}
}

func TestInvalidateCacheUsesWildcardAndNumericReference(t *testing.T) {
	cf := &mockCF{}
	cdn := &CloudFrontCDN{s3: &mockCDNS3{}, cf: cf}

	if err := cdn.InvalidateCache(context.Background(), "E123"); err != nil {
		t.Fatalf("InvalidateCache: %v", err)
	}
	if cf.lastInput == nil {
		t.Fatal("CreateInvalidation not called")
	}
	batch := cf.lastInput.InvalidationBatch
	if got := batch.Paths.Items[0]; got != "/*" {
		t.Errorf("Paths.Items[0] = %q, want /*", got)
	}
	if got := aws.ToInt32(batch.Paths.Quantity); got != 1 {
		t.Errorf("Paths.Quantity = %d, want 1", got)
	}
	ref := aws.ToString(batch.CallerReference)
	if _, err := strconv.ParseInt(ref, 10, 64); err != nil {
		t.Errorf("CallerReference %q is not a numeric string: %v", ref, err)
	}
	if aws.ToString(cf.lastInput.DistributionId) != "E123" {
		t.Errorf("DistributionId = %q, want E123", aws.ToString(cf.lastInput.DistributionId))
	}
}
