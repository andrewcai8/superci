package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ErrClaimed says another grant holds the machine's claim: the machine was granted its one runner, by this process
// or another, and gets no other.
var ErrClaimed = errors.New("the machine's runner was claimed by another grant")

// Claims is the fence of "a machine gets one runner, ever": one S3 object, issued/<instance id>, created only if
// absent (If-None-Match: *) before the runner is registered. S3 is strongly consistent, so of any number of grants,
// across restarts and overlapping listeners, exactly one creates it. The object holds the token of the grant that
// created it, so that grant alone may go on after a lost answer or a refused registration. The bucket refuses any
// write without If-None-Match and any delete (deploy/up.sh), and expires claims long after a machine's life.
type Claims struct {
	store  objects
	bucket string
}

type objects interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

func claimKey(instance string) string { return "issued/" + instance }

// Take claims the machine for the grant holding token. nil: the grant holds the claim (it made it now, or before an
// answer was lost). ErrClaimed: another grant holds it. Any other error: unknown, so the grant must not go on yet.
func (c *Claims) Take(ctx context.Context, instance, token string) error {
	key := claimKey(instance)
	_, err := c.store.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key),
		Body: bytes.NewReader([]byte(token)), IfNoneMatch: aws.String("*"), ContentType: aws.String("text/plain")})
	var api smithy.APIError
	if err == nil || !errors.As(err, &api) || api.ErrorCode() != "PreconditionFailed" {
		return err
	}
	got, err := c.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("read claim %s: %w", key, err)
	}
	defer got.Body.Close()
	holder, err := io.ReadAll(io.LimitReader(got.Body, 256))
	if err != nil {
		return fmt.Errorf("read claim %s: %w", key, err)
	}
	if string(holder) != token {
		return ErrClaimed
	}
	return nil
}

// newToken names one grant: 128 random bits.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
