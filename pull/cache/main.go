// pullcache serves GitHub's Actions cache to the jobs of one superci-pull machine from an S3 bucket.
//
//	pullcache serve    the cache service, on the machine's private address
//	pullcache wrap D   put it in front of the runner's Node (D: the runner's externals folder)
//	pullcache janitor  keep the bucket under a size
//	pullcache mirror   copy the default branch's entries the peer region lacks (what the mirror function does per save)
//
// As `node` (a wrapped Node) it becomes the real Node, with the job's cache service pointed here. As `bootstrap` it
// is a Lambda function: the broker (PULLCACHE_LAMBDA=broker), which gives a job credentials to write its own scope,
// the janitor (PULLCACHE_LAMBDA=janitor), or the mirror (PULLCACHE_LAMBDA=mirror), which copies each entry the
// default branch saves into the other region's bucket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	"superci-pull-cache/internal/broker"
	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/mirror"
	"superci-pull-cache/internal/s3store"
)

// defaultCapGB is what the janitor keeps of a region's entries at most (see the README's cost).
const defaultCapGB = 1000

func verifier() *cache.Verifier {
	return &cache.Verifier{JWKSURL: env("PULLCACHE_JWKS_URL", cache.GitHubJWKS), Issuer: env("PULLCACHE_ISSUER", cache.GitHubIssuer)}
}

func env(name, or string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return or
}

func main() {
	if filepath.Base(os.Args[0]) == "node" {
		fmt.Fprintln(os.Stderr, "pullcache:", cache.RunAsNode(os.Args))
		os.Exit(127)
	}
	if filepath.Base(os.Args[0]) == "bootstrap" {
		lambdaMain()
		return
	}
	lg := log.New(os.Stderr, "pullcache: ", log.LstdFlags|log.Lmicroseconds)
	if len(os.Args) < 2 {
		lg.Fatal("usage: pullcache serve | wrap <externals> | janitor | mirror")
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(lg)
	case "wrap":
		if len(os.Args) != 3 {
			lg.Fatal("usage: pullcache wrap <the runner's externals folder>")
		}
		err = cache.Wrap(os.Args[2])
	case "janitor":
		err = janitor(lg, os.Args[2:])
	case "mirror":
		err = mirrorAll(lg, os.Args[2:])
	default:
		err = fmt.Errorf("not a command: %s", os.Args[1])
	}
	if err != nil {
		lg.Fatal(err)
	}
}

// privateIP is the address the machine's network gives it, where a job's containers reach it too (nothing off the
// machine does: the security group has no inbound rule). No packet is sent.
func privateIP() (string, error) {
	if v := os.Getenv("PULLCACHE_LISTEN"); v != "" {
		return v, nil
	}
	c, err := net.Dial("udp", "169.254.169.254:80")
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func serve(lg *log.Logger) error {
	bucket, region := os.Getenv("PULLCACHE_BUCKET"), os.Getenv("PULLCACHE_REGION")
	if bucket == "" || region == "" {
		return fmt.Errorf("PULLCACHE_BUCKET and PULLCACHE_REGION are not set")
	}
	ctx := context.Background()
	var issue broker.Client
	st, err := s3store.New(ctx, bucket, env("PULLCACHE_STAGE_BUCKET", bucket+"-stage"), region, func(ctx context.Context, token string) (aws.Credentials, cache.Grant, error) {
		return issue.Issue(ctx, token)
	})
	if err != nil {
		return err
	}
	issue = broker.Client{Lambda: lambda.NewFromConfig(st.Config()), Function: env("PULLCACHE_BROKER", "superci-pull-cache-broker")}
	upstream, err := url.Parse(env("PULLCACHE_UPSTREAM", "https://results-receiver.actions.githubusercontent.com"))
	if err != nil {
		return err
	}
	ip, err := privateIP()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip, env("PULLCACHE_PORT", "8257")))
	if err != nil {
		return err
	}
	base := "http://" + l.Addr().String()
	srv := &cache.Server{Store: st, Verify: verifier().Verify, DefaultBranch: env("PULLCACHE_DEFAULT_BRANCH", "main"), Base: base, Upstream: upstream, Log: lg}
	if dir := os.Getenv("PULLCACHE_EXTERNALS"); dir != "" {
		at := filepath.Join(dir, cache.AddressFile)
		if err := os.WriteFile(at+".new", []byte(base), 0o644); err != nil {
			return err
		}
		if err := os.Rename(at+".new", at); err != nil {
			return err
		}
	}
	lg.Printf("serving GitHub's cache at %s from %s (%s); default branch %s", base, bucket, region, srv.DefaultBranch)
	return (&http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
}

func janitor(lg *log.Logger, args []string) error {
	fs := flag.NewFlagSet("janitor", flag.ExitOnError)
	bucket := fs.String("bucket", os.Getenv("PULLCACHE_BUCKET"), "the bucket")
	region := fs.String("region", os.Getenv("PULLCACHE_REGION"), "its region")
	capGB := fs.Int64("cap-gb", defaultCapGB, "entries kept at most, in GB")
	dry := fs.Bool("dry-run", false, "say what would go, delete nothing")
	_ = fs.Parse(args)
	if *bucket == "" || *region == "" {
		return fmt.Errorf("-bucket and -region (or PULLCACHE_BUCKET and PULLCACHE_REGION) are needed")
	}
	st, err := s3store.New(context.Background(), *bucket, *bucket+"-stage", *region, nil)
	if err != nil {
		return err
	}
	n, freed, err := cache.Janitor(context.Background(), st, *capGB<<30, *dry, lg)
	if *dry {
		lg.Printf("janitor: dry run, nothing deleted (%d entries, %d MB would go)", n, freed>>20)
	} else {
		lg.Printf("janitor: %d entries deleted, %d MB freed", n, freed>>20)
	}
	return err
}

func mirrorAll(lg *log.Logger, args []string) error {
	fs := flag.NewFlagSet("mirror", flag.ExitOnError)
	bucket := fs.String("bucket", "", "the bucket to copy from")
	region := fs.String("region", "", "its region")
	peer := fs.String("peer-bucket", "", "the bucket to copy into")
	peerRegion := fs.String("peer-region", "", "its region")
	prefix := fs.String("prefix", cache.EntryRoot+"d/", "the entries to copy")
	_ = fs.Parse(args)
	ctx := context.Background()
	m, err := newMirror(ctx, *bucket, *region, *peer, *peerRegion)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(*prefix, cache.EntryRoot) {
		return fmt.Errorf("-prefix %q is not under %s", *prefix, cache.EntryRoot)
	}
	m.Prefix = *prefix
	st, err := s3store.New(ctx, *bucket, *bucket+"-stage", *region, nil)
	if err != nil {
		return err
	}
	objs, err := st.List(ctx, m.Prefix, 1<<22)
	if err != nil {
		return err
	}
	// Oldest first, so the copies are made in the order of the saves.
	sort.Slice(objs, func(i, j int) bool { return objs[i].Modified.Before(objs[j].Modified) })
	counts := map[mirror.Outcome]int{}
	for _, o := range objs {
		out, err := m.Copy(ctx, o.Key)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Key, err)
		}
		counts[out]++
		if out == mirror.Copied {
			lg.Printf("mirror: copied %s (%d MB)", o.Key, o.Size>>20)
		}
	}
	lg.Printf("mirror: %d entries of %s: %v", len(objs), *bucket, counts)
	return nil
}
