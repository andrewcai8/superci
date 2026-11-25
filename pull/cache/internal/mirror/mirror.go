// Package mirror copies the default branch's entries from one region's bucket into the other's, so a machine finds
// main's caches in its own region whichever region saved them.
//
// An entry is written once and never changed, so the two buckets cannot disagree about a key: a copy is completed
// only with If-None-Match, and a key both regions saved keeps each region's own first save. The copy reads the one
// version it began with (If-Match on its ETag), so an entry the janitor deleted and a job saved again mid-copy ends
// the copy, never mixes two archives. A copy keeps its original's save time (cache.SavedMetadata): its own
// LastModified is when it was copied, and the newest of a restore key's matches goes by the save.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"superci-pull-cache/internal/cache"
)

// S3 is what a copy calls; *s3.Client is one.
type S3 interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPartCopy(context.Context, *s3.UploadPartCopyInput, ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// Outcome is what a copy did.
type Outcome string

const (
	Copied  Outcome = "copied"
	Exists  Outcome = "exists"  // the peer has the key (its own save, or an earlier copy)
	Gone    Outcome = "gone"    // the source is deleted or is no longer the version the copy began with
	Skipped Outcome = "skipped" // not an entry this mirror copies
)

// Mirror copies Prefix's entries of Src's bucket into Dst's. Src and Dst are clients in each bucket's region; a
// cross-region part copy goes at about 5 MB/s, so a large entry is copied as many parts at once.
type Mirror struct {
	Src, Dst             S3
	SrcBucket, DstBucket string
	Prefix               string
	PartSize             int64
	Parallel             int
}

func code(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func missing(err error) bool {
	var nf *types.NotFound
	c := code(err)
	return errors.As(err, &nf) || c == "NotFound" || c == "NoSuchKey"
}

func changed(err error) bool {
	c := code(err)
	return c == "PreconditionFailed" || c == "NoSuchKey" || c == "NotFound"
}

func taken(err error) bool {
	c := code(err)
	return c == "PreconditionFailed" || c == "ConditionalRequestConflict"
}

// Copy copies key into the peer bucket unless the peer has it.
func (m *Mirror) Copy(ctx context.Context, key string) (Outcome, error) {
	if !strings.HasPrefix(key, m.Prefix) {
		return Skipped, nil
	}
	if _, err := m.Dst.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &m.DstBucket, Key: &key}); err == nil {
		return Exists, nil
	} else if !missing(err) {
		return "", fmt.Errorf("head %s in %s: %w", key, m.DstBucket, err)
	}
	src, err := m.Src.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &m.SrcBucket, Key: &key})
	if err != nil {
		if missing(err) {
			return Gone, nil
		}
		return "", fmt.Errorf("head %s in %s: %w", key, m.SrcBucket, err)
	}
	size, etag := aws.ToInt64(src.ContentLength), aws.ToString(src.ETag)
	saved := map[string]string{cache.SavedMetadata: savedOf(src).Format(time.RFC3339Nano)}
	// (A copy source is URL-encoded: an entry's name has the scope's escapes in it, "%2F" among them.)
	from := m.SrcBucket + "/" + (&url.URL{Path: key}).EscapedPath()
	if size <= m.PartSize {
		_, err := m.Dst.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &m.DstBucket, Key: &key, CopySource: &from,
			CopySourceIfMatch: &etag, IfNoneMatch: aws.String("*"), MetadataDirective: types.MetadataDirectiveReplace, Metadata: saved})
		return settle(err)
	}
	return m.parts(ctx, key, from, etag, size, saved)
}

// savedOf is when the source was saved: its own save time if it is a copy, else its LastModified.
func savedOf(src *s3.HeadObjectOutput) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, src.Metadata[cache.SavedMetadata]); err == nil {
		return t
	}
	return aws.ToTime(src.LastModified)
}

// settle names a single copy's end. Its two preconditions both answer 412: the peer has the key, or the source is
// no longer the version this copy began with. Either way nothing is left to copy.
func settle(err error) (Outcome, error) {
	switch {
	case err == nil:
		return Copied, nil
	case taken(err):
		return Exists, nil
	case missing(err):
		return Gone, nil
	}
	return "", err
}

func (m *Mirror) parts(ctx context.Context, key, from, etag string, size int64, meta map[string]string) (Outcome, error) {
	mp, err := m.Dst.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &m.DstBucket, Key: &key, Metadata: meta})
	if err != nil {
		return "", err
	}
	abort := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = m.Dst.AbortMultipartUpload(c, &s3.AbortMultipartUploadInput{Bucket: &m.DstBucket, Key: &key, UploadId: mp.UploadId})
	}
	n := int((size + m.PartSize - 1) / m.PartSize)
	done := make([]types.CompletedPart, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	slots := make(chan struct{}, max(1, m.Parallel))
	for i := range n {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			pn := int32(i + 1)
			lo := int64(i) * m.PartSize
			hi := min(lo+m.PartSize, size) - 1
			out, err := m.Dst.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &m.DstBucket, Key: &key, UploadId: mp.UploadId, PartNumber: &pn,
				CopySource: &from, CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", lo, hi)), CopySourceIfMatch: &etag})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
				}
				return
			}
			done[i] = types.CompletedPart{ETag: out.CopyPartResult.ETag, PartNumber: &pn}
		}()
	}
	wg.Wait()
	if first != nil {
		abort()
		if changed(first) {
			return Gone, nil
		}
		return "", first
	}
	_, err = m.Dst.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &m.DstBucket, Key: &key, UploadId: mp.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done}, IfNoneMatch: aws.String("*")})
	if err != nil {
		abort()
		if taken(err) {
			return Exists, nil
		}
		return "", err
	}
	return Copied, nil
}
