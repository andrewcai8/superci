// pullcache serves GitHub's Actions cache to the jobs of one superci-pull machine from an S3 bucket.
//
//	pullcache serve    the cache service, on the machine's private address
//	pullcache wrap D   put it in front of the runner's Node (D: the runner's externals folder)
//	pullcache janitor  keep the bucket under a size
//	pullcache hydrate M R   unpack the entries manifest M names under R while the machine waits for its job; SIGTERM stops it
//	pullcache pin D    lock the files under D in the page cache and hold them until killed (see pin)
//	pullcache mirror   copy the default branch's entries the peer region lacks (what the mirror function does per save)
//
// As `node` (a wrapped Node) it becomes the real Node, with the job's cache service pointed here. As `bootstrap` it
// is a Lambda function: the broker (PULLCACHE_LAMBDA=broker), which gives a job credentials to write its own scope,
// the janitor (PULLCACHE_LAMBDA=janitor), or the mirror (PULLCACHE_LAMBDA=mirror), which copies each entry the
// default branch saves into the other region's bucket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	"superci-pull-cache/internal/broker"
	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/mirror"
	"superci-pull-cache/internal/pin"
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
		lg.Fatal("usage: pullcache serve | wrap <externals> | janitor | hydrate <manifest> <root> | pin <dir> | mirror | version")
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(revision)
	case "serve":
		lg.Printf("serve, revision %s", revision)
		err = serve(lg)
	case "wrap":
		if len(os.Args) != 3 {
			lg.Fatal("usage: pullcache wrap <the runner's externals folder>")
		}
		err = cache.Wrap(os.Args[2])
	case "janitor":
		err = janitor(lg, os.Args[2:])
	case "hydrate":
		err = hydrate(lg, os.Args[2:])
	case "pin":
		err = pinDir(os.Args[2:])
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
	srv := &cache.Server{Store: st, Verify: verifier().Verify, DefaultBranch: env("PULLCACHE_DEFAULT_BRANCH", "main"), Base: base, Upstream: upstream, Log: lg,
		Mark: cache.SpoolMarks(env("PULLCACHE_MARKS", "/run/superci-marks"), time.Now),
		Uses: &cache.Uses{Store: st, Log: lg}}
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
	capMB := fs.Int64("cap-mb", 0, "entries kept at most, in MB (instead of -cap-gb, to see what a small cap would delete)")
	idle := fs.Duration("max-idle", cache.DefaultMaxIdle, "an entry nobody restored for this long is deleted whatever the size (0: no limit)")
	dry := fs.Bool("dry-run", false, "say what would go and why, delete nothing")
	_ = fs.Parse(args)
	if *bucket == "" || *region == "" {
		return fmt.Errorf("-bucket and -region (or PULLCACHE_BUCKET and PULLCACHE_REGION) are needed")
	}
	st, err := s3store.New(context.Background(), *bucket, *bucket+"-stage", *region, nil)
	if err != nil {
		return err
	}
	capBytes := *capGB << 30
	if *capMB > 0 {
		capBytes = *capMB << 20
	}
	rep, err := cache.Janitor(context.Background(), st, cache.Policy{CapBytes: capBytes, MaxIdle: *idle, SupersededGrace: cache.DefaultSupersededGrace}, time.Now(), *dry, lg)
	lg.Printf("janitor: %s", rep)
	return err
}

// hydrate unpacks a manifest's entries while the machine waits for its job (boot.sh), with the machine's read-only
// role. boot.sh runs it as the runner's user, so what it writes is the job's own. It stops at -timeout, and at
// SIGTERM, which the runner's job-started hook sends so it never competes with the job; an entry it had not finished
// is left in a .tmp- directory no job takes. What became of each entry goes to <root>/.hydrated.json.
func hydrate(lg *log.Logger, args []string) error {
	fs := flag.NewFlagSet("hydrate", flag.ExitOnError)
	timeout := fs.Duration("timeout", 100*time.Second, "give up on what is not done by then")
	jitter := fs.Duration("jitter", 3*time.Second, "wait a random time up to this before the first read, so a burst of machines does not read one prefix in the same second")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: pullcache hydrate [-timeout d] <manifest> <root>")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	m, err := cache.ParseManifest(raw)
	if err != nil {
		return err
	}
	bucket, region := os.Getenv("PULLCACHE_BUCKET"), os.Getenv("PULLCACHE_REGION")
	if bucket == "" || region == "" {
		return fmt.Errorf("PULLCACHE_BUCKET and PULLCACHE_REGION are not set")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	st, err := s3store.New(ctx, bucket, bucket+"-stage", region, nil)
	if err != nil {
		return err
	}
	if *jitter > 0 {
		select {
		case <-time.After(rand.N(*jitter)):
		case <-ctx.Done():
		}
	}
	began := time.Now()
	h := &cache.Hydrator{Reader: st, DefaultBranch: env("PULLCACHE_DEFAULT_BRANCH", "main"), AnyScope: os.Getenv("PULLCACHE_HYDRATE_ANY_SCOPE") == "1", Root: fs.Arg(1), Log: lg,
		ToolCache: env("RUNNER_TOOL_CACHE", "/opt/hostedtoolcache"), Platform: cache.ThisPlatform()}
	res := h.Run(ctx, m)
	lg.Printf("hydrate: %d entries in %.1f s (stopped: %v)", len(res), time.Since(began).Seconds(), ctx.Err() != nil)
	var ok, mb int64
	for _, r := range res {
		if r.Error == "" {
			ok++
			mb += r.MB
		}
	}
	fmt.Printf("hydrated %d/%d entries %d MB\n", ok, len(res), mb)
	for _, r := range res {
		if r.Tree != "" {
			fmt.Printf("checkout %s\n", r.Tree)
		}
	}
	b, err := json.MarshalIndent(map[string]any{"seconds": time.Since(began).Seconds(), "stopped": ctx.Err() != nil, "entries": res}, "", "  ")
	if err != nil {
		return err
	}
	at := filepath.Join(fs.Arg(1), ".hydrated.json")
	if err := os.WriteFile(at+".new", b, 0o644); err != nil {
		return err
	}
	return os.Rename(at+".new", at)
}

// pinDir locks the regular files under a directory in memory (boot.sh starts it on the hydrated checkout, under setsid,
// so the job-started hook that ends the hydration does not end it). It prints one line (files, bytes, seconds, whether
// a cap stopped it) and then holds the maps until it is killed or the machine powers off. A lock the machine refuses is
// in the line and never an exit status: a machine that cannot pin runs its job as it did.
func pinDir(args []string) error {
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	maxBytes := fs.Int64("max-bytes", pin.DefaultMaxBytes, "stop locking after this many bytes of file data")
	maxFiles := fs.Int("max-files", pin.DefaultMaxFiles, "stop locking after this many files")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pullcache pin [-max-bytes n] [-max-files n] <dir>")
	}
	res, err := pin.Run(fs.Arg(0), pin.Options{MaxBytes: *maxBytes, MaxFiles: *maxFiles})
	if err != nil {
		return err
	}
	fmt.Println(res.Line())
	if res.Capped {
		fmt.Fprintf(os.Stderr, "pullcache: pin stopped at its cap (%d files, %d bytes; -max-files %d, -max-bytes %d)\n", res.Files, res.Bytes, *maxFiles, *maxBytes)
	}
	if res.Files == 0 {
		return nil
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	res.Release()
	return nil
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
