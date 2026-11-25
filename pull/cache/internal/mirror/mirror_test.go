package mirror

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"superci-pull-cache/internal/cache"
)

// fakeS3 is both regions' buckets in memory, with S3's answers to the preconditions a copy sends.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // bucket + "/" + key
	meta    map[string]map[string]string
	at      map[string]time.Time
	clock   time.Time
	uploads map[string]map[int32][]byte
	upMeta  map[string]map[string]string
	next    int
	// beforeComplete runs once, just before a multipart upload completes.
	beforeComplete func()
	// onPart runs before each part copy.
	onPart func()
}

func newFake() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, meta: map[string]map[string]string{}, at: map[string]time.Time{},
		clock: time.Unix(1_800_000_000, 0), uploads: map[string]map[int32][]byte{}, upMeta: map[string]map[string]string{}}
}

func etag(b []byte) string { return fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum(b))) }

func apiErr(c string) error { return &smithy.GenericAPIError{Code: c} }

func (f *fakeS3) put(bucket, key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(bucket+"/"+key, b, nil)
}

// write stores an object at the next tick of the bucket's clock, as S3 stamps LastModified.
func (f *fakeS3) write(k string, b []byte, meta map[string]string) {
	f.clock = f.clock.Add(time.Minute)
	f.objects[k], f.meta[k], f.at[k] = b, meta, f.clock
}

func (f *fakeS3) get(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[bucket+"/"+key]
	return b, ok
}

// source reads a copy source the way S3 does: "bucket/" then the URL-encoded key.
func (f *fakeS3) source(src, ifMatch string) ([]byte, error) {
	bucket, esc, _ := strings.Cut(src, "/")
	key, err := url.PathUnescape(esc)
	if err != nil {
		return nil, err
	}
	b, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, apiErr("NoSuchKey")
	}
	if ifMatch != "" && ifMatch != etag(b) {
		return nil, apiErr("PreconditionFailed")
	}
	return b, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	b, ok := f.get(*in.Bucket, *in.Key)
	if !ok {
		return nil, &types.NotFound{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := *in.Bucket + "/" + *in.Key
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(b))), ETag: aws.String(etag(b)), Metadata: f.meta[k], LastModified: aws.Time(f.at[k])}, nil
}

func (f *fakeS3) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := f.source(*in.CopySource, aws.ToString(in.CopySourceIfMatch))
	if err != nil {
		return nil, err
	}
	if _, ok := f.objects[*in.Bucket+"/"+*in.Key]; ok && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, apiErr("PreconditionFailed")
	}
	f.write(*in.Bucket+"/"+*in.Key, b, in.Metadata)
	return &s3.CopyObjectOutput{}, nil
}

func (f *fakeS3) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprint(f.next)
	f.uploads[id] = map[int32][]byte{}
	f.upMeta[id] = in.Metadata
	return &s3.CreateMultipartUploadOutput{UploadId: &id}, nil
}

func (f *fakeS3) UploadPartCopy(_ context.Context, in *s3.UploadPartCopyInput, _ ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error) {
	if f.onPart != nil {
		f.onPart()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := f.source(*in.CopySource, aws.ToString(in.CopySourceIfMatch))
	if err != nil {
		return nil, err
	}
	var lo, hi int
	if _, err := fmt.Sscanf(*in.CopySourceRange, "bytes=%d-%d", &lo, &hi); err != nil {
		return nil, err
	}
	part := append([]byte(nil), b[lo:hi+1]...)
	f.uploads[*in.UploadId][*in.PartNumber] = part
	return &s3.UploadPartCopyOutput{CopyPartResult: &types.CopyPartResult{ETag: aws.String(etag(part))}}, nil
}

func (f *fakeS3) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if f.beforeComplete != nil {
		f.beforeComplete()
		f.beforeComplete = nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[*in.Bucket+"/"+*in.Key]; ok && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, apiErr("PreconditionFailed")
	}
	var all []byte
	for _, p := range in.MultipartUpload.Parts {
		all = append(all, f.uploads[*in.UploadId][*p.PartNumber]...)
	}
	f.write(*in.Bucket+"/"+*in.Key, all, f.upMeta[*in.UploadId])
	delete(f.uploads, *in.UploadId)
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, *in.UploadId)
	return &s3.AbortMultipartUploadOutput{}, nil
}

const entry = "v1/d/77/refs%2Fheads%2Fmain/abc/setup-go-Linux-x64"

func setup(size int) (*fakeS3, *Mirror, []byte) {
	f := newFake()
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i * 7)
	}
	f.put("east-1", entry, b)
	return f, &Mirror{Src: f, Dst: f, SrcBucket: "east-1", DstBucket: "east-2", Prefix: "v1/d/", PartSize: 10, Parallel: 4}, b
}

func TestALargeEntryArrivesWholeInThePeer(t *testing.T) {
	f, m, b := setup(95)
	got, err := m.Copy(context.Background(), entry)
	if err != nil || got != Copied {
		t.Fatalf("copy: %v %v", got, err)
	}
	if peer, _ := f.get("east-2", entry); !bytes.Equal(peer, b) {
		t.Fatalf("the peer holds %d bytes, not the entry's %d", len(peer), len(b))
	}
	if len(f.uploads) != 0 {
		t.Fatalf("%d uploads left open", len(f.uploads))
	}
}

func TestASmallEntryIsOneCopy(t *testing.T) {
	f, m, b := setup(7)
	if got, err := m.Copy(context.Background(), entry); err != nil || got != Copied {
		t.Fatalf("copy: %v %v", got, err)
	}
	if peer, _ := f.get("east-2", entry); !bytes.Equal(peer, b) {
		t.Fatal("the peer does not hold the entry")
	}
}

func TestThePeersOwnSaveIsNeverWrittenOver(t *testing.T) {
	f, m, _ := setup(95)
	f.put("east-2", entry, []byte("east-2 saved it first"))
	if got, err := m.Copy(context.Background(), entry); err != nil || got != Exists {
		t.Fatalf("copy: %v %v", got, err)
	}
	if peer, _ := f.get("east-2", entry); string(peer) != "east-2 saved it first" {
		t.Fatalf("the peer's save was replaced: %q", peer)
	}
}

func TestASaveInThePeerDuringTheCopyWins(t *testing.T) {
	f, m, _ := setup(95)
	f.beforeComplete = func() { f.put("east-2", entry, []byte("saved mid-copy")) }
	if got, err := m.Copy(context.Background(), entry); err != nil || got != Exists {
		t.Fatalf("copy: %v %v", got, err)
	}
	if peer, _ := f.get("east-2", entry); string(peer) != "saved mid-copy" {
		t.Fatalf("the peer's save was replaced: %q", peer)
	}
	if len(f.uploads) != 0 {
		t.Fatalf("%d uploads left open", len(f.uploads))
	}
}

func TestAnEntryReplacedMidCopyIsNotMixed(t *testing.T) {
	f, m, _ := setup(95)
	var once sync.Once
	f.onPart = func() { once.Do(func() { f.put("east-1", entry, []byte("deleted and saved again by another job")) }) }
	if got, err := m.Copy(context.Background(), entry); err != nil || got != Gone {
		t.Fatalf("copy: %v %v", got, err)
	}
	if _, ok := f.get("east-2", entry); ok {
		t.Fatal("the peer holds an entry made of two versions")
	}
	if len(f.uploads) != 0 {
		t.Fatalf("%d uploads left open", len(f.uploads))
	}
}

func TestOnlyTheDefaultBranchIsMirrored(t *testing.T) {
	f, m, _ := setup(95)
	pr := "v1/r/77/refs%2Fpull%2F7%2Fmerge/abc/deps"
	f.put("east-1", pr, []byte("a pull request's"))
	if got, err := m.Copy(context.Background(), pr); err != nil || got != Skipped {
		t.Fatalf("copy: %v %v", got, err)
	}
	if _, ok := f.get("east-2", pr); ok {
		t.Fatal("a pull request's entry was copied")
	}
}

func TestADeletedEntryIsNotCopied(t *testing.T) {
	_, m, _ := setup(95)
	if got, err := m.Copy(context.Background(), entry+"-gone"); err != nil || got != Gone {
		t.Fatalf("copy: %v %v", got, err)
	}
}

func TestACopyKeepsItsOriginalsSaveTime(t *testing.T) {
	for _, size := range []int{7, 95} {
		f, m, _ := setup(size)
		saved := f.at["east-1/"+entry]
		if got, err := m.Copy(context.Background(), entry); err != nil || got != Copied {
			t.Fatalf("size %d: copy: %v %v", size, got, err)
		}
		k := "east-2/" + entry
		if !f.at[k].After(saved) {
			t.Fatalf("size %d: the copy's LastModified %v is not after the save %v (the test's clock is wrong)", size, f.at[k], saved)
		}
		if got := f.meta[k][cache.SavedMetadata]; got != saved.Format(time.RFC3339Nano) {
			t.Fatalf("size %d: the copy says it was saved %q, the original was saved %v", size, got, saved)
		}
		// A copy of the copy (the peer's mirror answering the copy's own notice) still names the first save.
		m2 := &Mirror{Src: f, Dst: f, SrcBucket: "east-2", DstBucket: "east-3", Prefix: "v1/d/", PartSize: 10, Parallel: 4}
		if _, err := m2.Copy(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
		if got := f.meta["east-3/"+entry][cache.SavedMetadata]; got != saved.Format(time.RFC3339Nano) {
			t.Fatalf("size %d: a copy of a copy says %q, not the first save %v", size, got, saved)
		}
	}
}
