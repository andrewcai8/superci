package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	awslambda "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"superci-pull-cache/internal/broker"
	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/s3store"
)

// lambdaMain runs one of the two functions, by PULLCACHE_LAMBDA. Both read their bucket from PULLCACHE_BUCKET and
// run in its region.
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
		awslambda.Start(func(ctx context.Context) (string, error) {
			n, freed, err := cache.Janitor(ctx, st, capGB<<30, false, lg)
			return fmt.Sprintf("%d entries deleted, %d MB freed", n, freed>>20), err
		})
	default:
		lg.Fatal("PULLCACHE_LAMBDA is broker or janitor")
	}
}
