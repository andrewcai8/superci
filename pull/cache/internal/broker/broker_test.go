package broker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"superci-pull-cache/internal/broker"
	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

type fakeSTS struct{ calls []*sts.AssumeRoleInput }

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.calls = append(f.calls, in)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("AKIA"), SecretAccessKey: aws.String("s"),
		SessionToken: aws.String("t"), Expiration: aws.Time(time.Now().Add(time.Hour))}}, nil
}

type rig struct {
	b      *broker.Broker
	sts    *fakeSTS
	signer *cachetest.Signer
	log    *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	signer := cachetest.NewSigner("k1")
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(signer.JWKS()) }))
	t.Cleanup(jwks.Close)
	v := &cache.Verifier{JWKSURL: jwks.URL, Issuer: cache.GitHubIssuer}
	f, logs := &fakeSTS{}, &bytes.Buffer{}
	return &rig{b: &broker.Broker{Verify: v.Verify, DefaultBranch: "main", Bucket: "B", Stage: "S", Role: "arn:aws:iam::1:role/writer", STS: f, Log: log.New(logs, "", 0)},
		sts: f, signer: signer, log: logs}
}

func (r *rig) token(scopes ...string) string {
	return r.signer.Mint(cache.GitHubIssuer, "77", cachetest.AC(scopes...), time.Now().Add(time.Hour))
}

type policy struct {
	Statement []struct {
		Effect   string
		Action   []string
		Resource string
	}
}

// A pull request's credentials name its own scope and nothing else: not main's entries, not another scope's blocks.
func TestAPullRequestsCredentialsReachItsOwnScopeOnly(t *testing.T) {
	r := newRig(t)
	got, err := r.b.Issue(t.Context(), broker.Request{Token: r.token("refs/pull/7/merge:rw", "refs/heads/main:r")})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.sts.calls) != 1 {
		t.Fatalf("%d calls to STS", len(r.sts.calls))
	}
	in := r.sts.calls[0]
	if aws.ToString(in.RoleArn) != "arn:aws:iam::1:role/writer" || aws.ToInt32(in.DurationSeconds) != 3600 {
		t.Fatalf("assumed %s for %d s", aws.ToString(in.RoleArn), aws.ToInt32(in.DurationSeconds))
	}
	var p policy
	if err := json.Unmarshal([]byte(aws.ToString(in.Policy)), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"arn:aws:s3:::B/v1/r/77/refs%2Fpull%2F7%2Fmerge/*": "[s3:PutObject s3:AbortMultipartUpload]",
		"arn:aws:s3:::S/u/77/refs%2Fpull%2F7%2Fmerge/*":    "[s3:PutObject s3:GetObject s3:DeleteObject]",
	}
	if len(p.Statement) != len(want) {
		t.Fatalf("policy %s", aws.ToString(in.Policy))
	}
	for _, s := range p.Statement {
		if s.Effect != "Allow" || want[s.Resource] == "" || want[s.Resource] != fmt.Sprint(s.Action) {
			t.Fatalf("policy %s", aws.ToString(in.Policy))
		}
	}
	if got.Grant != (cache.Grant{Entries: "v1/r/77/refs%2Fpull%2F7%2Fmerge/", Stage: "u/77/refs%2Fpull%2F7%2Fmerge/"}) || got.AccessKeyID != "AKIA" {
		t.Fatalf("reply %+v", got.Grant)
	}
	if bytes.Contains(r.log.Bytes(), []byte(`"s"`)) || bytes.Contains(r.log.Bytes(), []byte("AKIA")) {
		t.Fatalf("a credential was logged:\n%s", r.log)
	}
}

func TestMainsCredentialsAreForMainsScope(t *testing.T) {
	r := newRig(t)
	if _, err := r.b.Issue(t.Context(), broker.Request{Token: r.token("refs/heads/main:rw")}); err != nil {
		t.Fatal(err)
	}
	if p := aws.ToString(r.sts.calls[0].Policy); !bytes.Contains([]byte(p), []byte("arn:aws:s3:::B/v1/d/77/refs%2Fheads%2Fmain/*")) {
		t.Fatalf("policy %s", p)
	}
}

func TestNoCredentialsForATokenThatCannotWrite(t *testing.T) {
	r := newRig(t)
	other := cachetest.NewSigner("k1").Mint(cache.GitHubIssuer, "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))
	for name, tok := range map[string]string{"read only": r.token("refs/heads/main:r"), "another key": other, "not a token": "x"} {
		if _, err := r.b.Issue(t.Context(), broker.Request{Token: tok}); err == nil {
			t.Errorf("%s: credentials issued", name)
		}
	}
	if len(r.sts.calls) != 0 {
		t.Fatalf("STS was called %d times", len(r.sts.calls))
	}
}
