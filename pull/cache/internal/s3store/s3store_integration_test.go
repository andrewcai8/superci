package s3store_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/s3store"
)

// These run against a real bucket that the test owns and empties: PULLCACHE_TEST_BUCKET and PULLCACHE_TEST_REGION
// (with AWS_PROFILE for the account). They prove what S3 does with the conditions the janitor and the daemon rely
// on: a delete that carries an ETag, and a marker created once.
func bucket(t *testing.T) (*s3store.Store, *s3.Client, string) {
	t.Helper()
	b, r := os.Getenv("PULLCACHE_TEST_BUCKET"), os.Getenv("PULLCACHE_TEST_REGION")
	if b == "" || r == "" {
		t.Skip("PULLCACHE_TEST_BUCKET and PULLCACHE_TEST_REGION are not set")
	}
	if !strings.HasSuffix(b, "-pull-evict") {
		t.Fatalf("%s is not a bucket this test may empty", b)
	}
	ctx := context.Background()
	st, err := s3store.New(ctx, b, b+"-stage", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	who, err := sts.NewFromConfig(st.Config()).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if acct := aws.ToString(who.Account); acct != "243891825325" {
		t.Fatalf("these tests write only in account ci-runners (243891825325), not %s", acct)
	}
	c := s3.NewFromConfig(st.Config())
	for _, p := range []string{"v1/", "a/"} {
		objs, err := st.List(ctx, p, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: &o.Key}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st, c, b
}

func put(t *testing.T, c *s3.Client, b, key, body string) {
	t.Helper()
	if _, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: &b, Key: &key, Body: strings.NewReader(body)}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTakesOnlyTheVersionListed(t *testing.T) {
	st, c, b := bucket(t)
	ctx := context.Background()
	put(t, c, b, "v1/d/1/main/v1/a", "first")
	put(t, c, b, "v1/d/1/main/v1/b", "first")
	put(t, c, b, "v1/d/1/main/v1/c", "first")
	listed, err := st.List(ctx, "v1/", 100)
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed %v, %v", listed, err)
	}
	put(t, c, b, "v1/d/1/main/v1/b", "saved again after the listing")
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: aws.String("v1/d/1/main/v1/c")}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Delete(ctx, listed)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]cache.Removal{"v1/d/1/main/v1/a": cache.Deleted, "v1/d/1/main/v1/b": cache.Changed, "v1/d/1/main/v1/c": cache.Gone}
	for i, o := range listed {
		if got[i] != want[o.Key] {
			t.Errorf("%s: %v, want %v", o.Key, got[i], want[o.Key])
		}
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: aws.String("v1/d/1/main/v1/b")})
	if err != nil {
		t.Fatalf("the entry saved again was deleted: %v", err)
	}
	body, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(body) != "saved again after the listing" {
		t.Fatalf("body %q", body)
	}
	if _, ok, _ := st.Head(ctx, "v1/d/1/main/v1/a"); ok {
		t.Fatal("the version listed was not deleted")
	}
}

func TestAMarkIsCreatedOnceAndRepeatedWritesAreNotAnError(t *testing.T) {
	st, _, _ := bucket(t)
	ctx := context.Background()
	mk := cache.MarkKey("v1/d/1/main/v1/a", time.Now())
	for range 3 {
		if err := st.CreateMark(ctx, mk); err != nil {
			t.Fatal(err)
		}
	}
	marks, err := st.List(ctx, cache.MarkRoot, 100)
	if err != nil || len(marks) != 1 || marks[0].Key != mk || marks[0].Size != 0 {
		t.Fatalf("markers %v, %v", marks, err)
	}
	if _, ok, err := st.Head(ctx, mk); err != nil || !ok {
		t.Fatalf("head %v %v", ok, err)
	}
}

func TestTheJanitorOnARealBucket(t *testing.T) {
	st, c, b := bucket(t)
	ctx := context.Background()
	key := func(name string) string {
		return cache.ObjectKey("1", cache.Scope{Name: "refs/heads/main"}, "v1", name, "main")
	}
	body := strings.Repeat("x", 1000)
	for _, n := range []string{"git-seed-v1-Linux-X64-10000000001", "git-seed-v1-Linux-X64-10000000002", "git-seed-v1-Linux-X64-10000000003", "uv-toolcache-v1-uv0.13.9"} {
		put(t, c, b, key(n), body)
		time.Sleep(1100 * time.Millisecond) // S3 stamps a save to the second: distinct save times need distinct seconds
	}
	if err := st.CreateMark(ctx, cache.MarkKey(key("uv-toolcache-v1-uv0.13.9"), time.Now())); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	lg := log.New(&out, "", 0)
	p := cache.Policy{CapBytes: 2000, MaxIdle: 30 * 24 * time.Hour}
	rep, err := cache.Janitor(ctx, st, p, time.Now(), true, lg)
	if err != nil || rep.Planned != 2 || rep.Deleted != 0 {
		t.Fatalf("dry %+v, %v\n%s", rep, err, out.String())
	}
	rep, err = cache.Janitor(ctx, st, p, time.Now(), false, lg)
	if err != nil || rep.Planned != 2 || rep.Deleted != 2 || rep.Freed != 2000 {
		t.Fatalf("real %+v, %v\n%s", rep, err, out.String())
	}
	left, _ := st.List(ctx, "v1/", 100)
	if len(left) != 2 {
		t.Fatalf("left %v", left)
	}
	for _, o := range left {
		if strings.HasSuffix(o.Key, "10000000001") || strings.HasSuffix(o.Key, "10000000002") {
			t.Fatalf("the superseded seed %s was kept", o.Key)
		}
	}
	rep, err = cache.Janitor(ctx, st, p, time.Now(), false, lg)
	if err != nil || rep.Planned != 0 {
		t.Fatalf("again %+v, %v", rep, err)
	}
}
