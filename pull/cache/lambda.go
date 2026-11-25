package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	awslambda "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"superci-pull-cache/internal/broker"
	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/mirror"
	"superci-pull-cache/internal/s3store"
)

// lambdaMain runs one of the three functions, by PULLCACHE_LAMBDA. Each reads its bucket from PULLCACHE_BUCKET and
// runs in its region.
func lambdaMain() {
	lg := log.New(os.Stderr, "", 0)
	ctx := context.Background()
	bucket, region := os.Getenv("PULLCACHE_BUCKET"), os.Getenv("AWS_REGION")
	switch os.Getenv("PULLCACHE_LAMBDA") {
	case "broker":
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			lg.Fatal(err)
		}
		b := &broker.Broker{Verify: verifier().Verify, DefaultBranch: env("PULLCACHE_DEFAULT_BRANCH", "main"), Bucket: bucket,
			Stage: env("PULLCACHE_STAGE_BUCKET", bucket+"-stage"), Role: os.Getenv("PULLCACHE_WRITER_ROLE"), STS: sts.NewFromConfig(cfg), Log: lg}
		awslambda.Start(b.Issue)
	case "janitor":
		capGB, err := strconv.ParseInt(env("PULLCACHE_CAP_GB", strconv.Itoa(defaultCapGB)), 10, 64)
		if err != nil {
			lg.Fatal(err)
		}
		st, err := s3store.New(ctx, bucket, bucket+"-stage", region, nil)
		if err != nil {
			lg.Fatal(err)
		}
		idle, err := time.ParseDuration(env("PULLCACHE_MAX_IDLE", cache.DefaultMaxIdle.String()))
		if err != nil {
			lg.Fatal(err)
		}
		awslambda.Start(func(ctx context.Context) (string, error) {
			rep, err := cache.Janitor(ctx, st, cache.Policy{CapBytes: capGB << 30, MaxIdle: idle, SupersededGrace: cache.DefaultSupersededGrace}, time.Now(), false, lg)
			return rep.String(), err
		})
	case "mirror":
		m, err := newMirror(ctx, bucket, region, os.Getenv("PULLCACHE_PEER_BUCKET"), os.Getenv("PULLCACHE_PEER_REGION"))
		if err != nil {
			lg.Fatal(err)
		}
		// A failed copy fails the invocation, and Lambda runs it again: a copy that is done answers "exists".
		awslambda.Start(func(ctx context.Context, e events.S3Event) error {
			var failed error
			for _, r := range e.Records {
				if r.S3.Bucket.Name != bucket {
					continue
				}
				out, err := m.Copy(ctx, r.S3.Object.URLDecodedKey)
				// One JSON line a copy: the metric filter up.sh makes reads lag_ms, the time from the save to the copy.
				line, _ := json.Marshal(map[string]any{"ev": "cache.mirror", "key": r.S3.Object.URLDecodedKey, "to": m.DstBucket,
					"outcome": out, "lag_ms": time.Since(r.EventTime).Milliseconds(), "err": fmt.Sprint(err)})
				lg.Print(string(line))
				failed = errors.Join(failed, err)
			}
			return failed
		})
	default:
		lg.Fatal("PULLCACHE_LAMBDA is broker, janitor or mirror")
	}
}

// newMirror copies bucket's default-branch entries into peer, with a client in each region.
func newMirror(ctx context.Context, bucket, region, peer, peerRegion string) (*mirror.Mirror, error) {
	if bucket == "" || region == "" || peer == "" || peerRegion == "" {
		return nil, fmt.Errorf("the mirror needs its bucket and region and the peer's (PULLCACHE_PEER_BUCKET, PULLCACHE_PEER_REGION)")
	}
	src, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	dst, err := config.LoadDefaultConfig(ctx, config.WithRegion(peerRegion))
	if err != nil {
		return nil, err
	}
	return &mirror.Mirror{Src: s3.NewFromConfig(src), Dst: s3.NewFromConfig(dst), SrcBucket: bucket, DstBucket: peer,
		Prefix: cache.EntryRoot + "d/", PartSize: 16 << 20, Parallel: 32}, nil
}
