// Package s3store is the cache's bucket: S3, read with the machine's role and written with credentials the broker
// issues for one job's scope.
package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Issue gets credentials for the job whose token this is, and the grant they are for.
type Issue func(ctx context.Context, token string) (aws.Credentials, cache.Grant, error)

// Store reads entries from bucket with the role it is given, and hands out a writer per job. An upload's blocks go
// in stage, a bucket of its own without versioning.
type Store struct {
	cfg    aws.Config
	c      *s3.Client
	bucket string
	stage  string
	issue  Issue

	mu      sync.Mutex
	writers map[[32]byte]*writer
}

var (
	_ cache.Store  = (*Store)(nil)
	_ cache.Pruner = (*Store)(nil)
)

// New is the bucket as this process's role reaches it. issue may be nil where nothing is saved (the janitor).
func New(ctx context.Context, bucket, stage, region string, issue Issue) (*Store, error) {
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
	return &Store{cfg: cfg, c: s3.NewFromConfig(cfg), bucket: bucket, stage: stage, issue: issue, writers: map[[32]byte]*writer{}}, nil
}

// Config is the AWS configuration the store reads with (the machine's role).
func (s *Store) Config() aws.Config { return s.cfg }

func code(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func (s *Store) Head(ctx context.Context, key string) (cache.Object, bool, error) {
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) || code(err) == "NotFound" || code(err) == "NoSuchKey" {
			return cache.Object{}, false, nil
		}
		return cache.Object{}, false, err
	}
	return cache.Object{Key: key, Size: aws.ToInt64(out.ContentLength), Modified: aws.ToTime(out.LastModified), ETag: aws.ToString(out.ETag)}, true, nil
}

func (s *Store) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	var out []cache.Object
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() && len(out) < max {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, cache.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified), ETag: aws.ToString(o.ETag)})
		}
	}
	return out, nil
}

func (s *Store) Open(ctx context.Context, key, etag string, start, end int64) (io.ReadCloser, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, IfMatch: aws.String(etag), Range: aws.String(fmt.Sprintf("bytes=%d-%d", start, end))})
	if err != nil {
		if c := code(err); c == "PreconditionFailed" || c == "NoSuchKey" {
			return nil, fmt.Errorf("%w: %v", cache.ErrChanged, err)
		}
		return nil, err
	}
	return out.Body, nil
}

// Delete is the janitor's: entries, with its own role (the only one that may delete one).
func (s *Store) Delete(ctx context.Context, keys []string) error {
	return deleteKeys(ctx, s.c, s.bucket, keys)
}

func deleteKeys(ctx context.Context, c *s3.Client, bucket string, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		ids := make([]types.ObjectIdentifier, n)
		for i, k := range keys[:n] {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("%d of %d not deleted: %s", len(out.Errors), n, aws.ToString(out.Errors[0].Code))
		}
		keys = keys[n:]
	}
	return nil
}

// A job's writer: an S3 client whose credentials the broker issued for the job's grant, kept while the job saves
// (the credentials last an hour and are asked for again five minutes before they run out).
type writer struct {
	c      *s3.Client
	bucket string
	stage  string
	grant  cache.Grant
	used   time.Time
}

// Writer is the job's writer, made once per token. The credentials are not asked for until the first write.
func (s *Store) Writer(_ context.Context, token string, g cache.Grant) (cache.Writer, error) {
	if s.issue == nil {
		return nil, errors.New("this process has no broker to write with")
	}
	id := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, w := range s.writers {
		if now.Sub(w.used) > 12*time.Hour {
			delete(s.writers, k)
		}
	}
	if w, ok := s.writers[id]; ok && w.grant == g {
		w.used = now
		return w, nil
	}
	issue := s.issue
	creds := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		c, got, err := issue(ctx, token)
		if err != nil {
			return aws.Credentials{}, err
		}
		if got != g {
			return aws.Credentials{}, fmt.Errorf("the broker grants %+v, this server expected %+v (is the default branch set alike in both?)", got, g)
		}
		return c, nil
	}), func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = 5 * time.Minute })
	w := &writer{c: s3.NewFromConfig(s.cfg, func(o *s3.Options) { o.Credentials = creds }), bucket: s.bucket, stage: s.stage, grant: g, used: now}
	s.writers[id] = w
	return w, nil
}

// Put keeps a block in the stage bucket.
func (w *writer) Put(ctx context.Context, key string, body []byte) error {
	_, err := w.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &w.stage, Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))},
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
	return err
}

// Assemble joins staged blocks into the entry by S3's own copy (nothing crosses the machine) and completes the
// upload only if no object has that name: a save that loses a race is turned away, never written over.
func (w *writer) Assemble(ctx context.Context, final string, parts []cache.Part) error {
	for i, p := range parts[:len(parts)-1] {
		if p.Size < 5<<20 {
			return fmt.Errorf("block %d: %w", i, cache.ErrPartsTooSmall)
		}
	}
	mp, err := w.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &w.bucket, Key: &final})
	if err != nil {
		return err
	}
	abort := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = w.c.AbortMultipartUpload(c, &s3.AbortMultipartUploadInput{Bucket: &w.bucket, Key: &final, UploadId: mp.UploadId})
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
			// (A copy source is URL-encoded: a block's name has the scope's escapes in it, "%2F" among them.)
			src := w.stage + "/" + (&url.URL{Path: p.Key}).EscapedPath()
			out, err := w.c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &w.bucket, Key: &final, UploadId: mp.UploadId, PartNumber: &n, CopySource: &src})
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
	_, err = w.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &w.bucket, Key: &final, UploadId: mp.UploadId,
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

// Delete deletes the job's own blocks from the stage bucket.
func (w *writer) Delete(ctx context.Context, keys []string) error {
	return deleteKeys(ctx, w.c, w.stage, keys)
}
