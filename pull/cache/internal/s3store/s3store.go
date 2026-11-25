// Package s3store is the cache's bucket: S3, reached with the machine's role.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"superci-pull-cache/internal/cache"
)

// Store keeps entries in bucket and an upload's blocks in stage, a bucket of its own without versioning: a block
// deleted from a versioned bucket is still kept, and paid for, as an old version.
type Store struct {
	c      *s3.Client
	bucket string
	stage  string
}

// of is the bucket a name belongs in.
func (s *Store) of(key string) *string {
	if strings.HasPrefix(key, cache.StagingRoot) {
		return &s.stage
	}
	return &s.bucket
}

var _ cache.Store = (*Store)(nil)

func New(ctx context.Context, bucket, stage, region string) (*Store, error) {
	hc := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.MaxIdleConns, t.MaxIdleConnsPerHost = 256, 128
	})
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region), config.WithHTTPClient(hc),
		// No checksum computed over a block unless S3 requires one: the bytes come from a loopback socket.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
	if err != nil {
		return nil, err
	}
	return &Store{c: s3.NewFromConfig(cfg), bucket: bucket, stage: stage}, nil
}

func code(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func (s *Store) Head(ctx context.Context, key string) (int64, bool, error) {
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: s.of(key), Key: &key})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) || code(err) == "NotFound" || code(err) == "NoSuchKey" {
			return 0, false, nil
		}
		return 0, false, err
	}
	return aws.ToInt64(out.ContentLength), true, nil
}

func (s *Store) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	var out []cache.Object
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: s.of(prefix), Prefix: &prefix})
	for p.HasMorePages() && len(out) < max {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, cache.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

func (s *Store) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{Bucket: s.of(key), Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))},
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
	return err
}

// Assemble joins staged blocks into the entry by S3's own copy (nothing crosses the machine) and completes the
// upload only if no object has that name: a save that loses a race is turned away, never written over.
func (s *Store) Assemble(ctx context.Context, final string, parts []cache.Part) error {
	for i, p := range parts[:len(parts)-1] {
		if p.Size < 5<<20 {
			return fmt.Errorf("block %d: %w", i, cache.ErrPartsTooSmall)
		}
	}
	mp, err := s.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &final})
	if err != nil {
		return err
	}
	abort := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.c.AbortMultipartUpload(c, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &final, UploadId: mp.UploadId})
	}
	done := make([]types.CompletedPart, len(parts))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	slots := make(chan struct{}, 16)
	for i, p := range parts {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			n := int32(i + 1)
			out, err := s.c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &s.bucket, Key: &final, UploadId: mp.UploadId, PartNumber: &n, CopySource: aws.String(*s.of(p.Key) + "/" + p.Key)})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
				}
				return
			}
			done[i] = types.CompletedPart{ETag: out.CopyPartResult.ETag, PartNumber: &n}
		}()
	}
	wg.Wait()
	if first != nil {
		abort()
		return first
	}
	_, err = s.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &s.bucket, Key: &final, UploadId: mp.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done}, IfNoneMatch: aws.String("*")})
	if err != nil {
		abort()
		if c := code(err); c == "PreconditionFailed" || c == "ConditionalRequestConflict" {
			return cache.ErrExists
		}
		return err
	}
	return nil
}

func (s *Store) Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, Range: aws.String(fmt.Sprintf("bytes=%d-%d", start, end))})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *Store) Delete(ctx context.Context, keys []string) error {
	for _, bucket := range []*string{&s.bucket, &s.stage} {
		var mine []string
		for _, k := range keys {
			if s.of(k) == bucket {
				mine = append(mine, k)
			}
		}
		for len(mine) > 0 {
			n := min(len(mine), 1000)
			ids := make([]types.ObjectIdentifier, n)
			for i, k := range mine[:n] {
				ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
			}
			if _, err := s.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}}); err != nil {
				return err
			}
			mine = mine[n:]
		}
	}
	return nil
}

func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	objs, err := s.List(ctx, prefix, 100000)
	if err != nil {
		return err
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	return s.Delete(ctx, keys)
}
