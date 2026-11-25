// Package broker turns a job's runtime token into AWS credentials that can write that job's own scope of the cache
// and nothing else. It runs as a Lambda function; the machines' role can read the cache and call it, but write
// nothing, so a job's step that takes the machine's credentials cannot write another scope's entries.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"superci-pull-cache/internal/cache"
)

// Lifetime is how long issued credentials last (an hour is the most a role chained from the function's own can
// have). The daemon asks again when they run out.
const Lifetime = time.Hour

type Request struct {
	Token string `json:"token"`
}

type Reply struct {
	AccessKeyID     string      `json:"access_key_id"`
	SecretAccessKey string      `json:"secret_access_key"`
	SessionToken    string      `json:"session_token"`
	Expires         time.Time   `json:"expires"`
	Grant           cache.Grant `json:"grant"`
}

type AssumeRoler interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, opts ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// Broker checks a token as the daemon does and assumes the writer role with a session policy for the token's grant.
// The writer role's own policy allows writes to every scope; the session policy narrows what these credentials can
// do to the intersection, one scope.
type Broker struct {
	Verify        func(token string) (cache.Access, error)
	DefaultBranch string
	Bucket, Stage string
	Role          string
	STS           AssumeRoler
	Log           *log.Logger
}

func (b *Broker) Issue(ctx context.Context, req Request) (Reply, error) {
	a, err := b.Verify(req.Token)
	if err != nil {
		b.Log.Printf("refused: %v", err)
		return Reply{}, fmt.Errorf("refused: %w", err)
	}
	g, ok := cache.GrantFor(a, b.DefaultBranch)
	if !ok {
		b.Log.Printf("refused: repository %s: the token may not write the cache", a.Repo)
		return Reply{}, errors.New("refused: the token may not write the cache")
	}
	sum := sha256.Sum256([]byte(g.Entries))
	out, err := b.STS.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(b.Role),
		RoleSessionName: aws.String("pullcache-" + a.Repo + "-" + hex.EncodeToString(sum[:6])),
		Policy:          aws.String(Policy(b.Bucket, b.Stage, g)),
		DurationSeconds: aws.Int32(int32(Lifetime / time.Second)),
	})
	if err != nil {
		b.Log.Printf("repository %s, %s: the writer role could not be assumed: %v", a.Repo, g.Entries, err)
		return Reply{}, err
	}
	c := out.Credentials
	b.Log.Printf("issued: repository %s, %s and %s, until %s", a.Repo, g.Entries, g.Stage, aws.ToTime(c.Expiration).UTC().Format(time.RFC3339))
	return Reply{AccessKeyID: aws.ToString(c.AccessKeyId), SecretAccessKey: aws.ToString(c.SecretAccessKey), SessionToken: aws.ToString(c.SessionToken),
		Expires: aws.ToTime(c.Expiration), Grant: g}, nil
}

// Policy is the session policy for one grant: an entry may be created (S3's multipart upload included) under the
// scope's prefix of the entries bucket, and blocks put, read (to be copied into the entry) and deleted under the
// scope's prefix of the stage bucket. The prefixes are esc'd, so nothing in them is an IAM wildcard or variable.
func Policy(bucket, stage string, g cache.Grant) string {
	type statement struct {
		Effect   string
		Action   []string
		Resource string
	}
	b, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []statement{
		{"Allow", []string{"s3:PutObject", "s3:AbortMultipartUpload"}, "arn:aws:s3:::" + bucket + "/" + g.Entries + "*"},
		{"Allow", []string{"s3:PutObject", "s3:GetObject", "s3:DeleteObject"}, "arn:aws:s3:::" + stage + "/" + g.Stage + "*"},
	}})
	return string(b)
}

// Client is the daemon's side: it calls the function with the machine's role.
type Client struct {
	Lambda   *lambda.Client
	Function string
}

func (c Client) Issue(ctx context.Context, token string) (aws.Credentials, cache.Grant, error) {
	in, _ := json.Marshal(Request{Token: token})
	out, err := c.Lambda.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(c.Function), Payload: in})
	if err != nil {
		return aws.Credentials{}, cache.Grant{}, err
	}
	if out.FunctionError != nil {
		var e struct{ ErrorMessage string }
		_ = json.Unmarshal(out.Payload, &e)
		return aws.Credentials{}, cache.Grant{}, fmt.Errorf("the broker: %s", e.ErrorMessage)
	}
	var r Reply
	if err := json.Unmarshal(out.Payload, &r); err != nil {
		return aws.Credentials{}, cache.Grant{}, fmt.Errorf("the broker's answer: %v", err)
	}
	return aws.Credentials{AccessKeyID: r.AccessKeyID, SecretAccessKey: r.SecretAccessKey, SessionToken: r.SessionToken,
		CanExpire: true, Expires: r.Expires, Source: "pullcache broker"}, r.Grant, nil
}
