package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// carriedPools is the SSM parameter's value: every set's Carried, and when it was saved.
type carriedPools struct {
	At   time.Time          `json:"at"`
	Sets map[string]Carried `json:"sets"`
}

// paramStore is the part of SSM the pools' save and load use.
type paramStore interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

// poolParamName is the parameter one deployment keeps its sets' pools in. Each deployment writes only its own, so a
// dev listener never overwrites production's, and the listener a roll starts reads what the one it replaces saved.
func poolParamName(prefix, deployment string) string {
	return strings.TrimRight(prefix, "/") + "/" + deployment
}

// loadPools resumes each set from what the last listener saved, if it saved within the window: an older save says
// nothing about the jobs arriving now.
func loadPools(ctx context.Context, client paramStore, name string, sets []*Set, window time.Duration, now time.Time) error {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved carriedPools
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &saved); err != nil {
		return err
	}
	if now.Sub(saved.At) > window {
		return nil
	}
	for _, s := range sets {
		if c, ok := saved.Sets[s.spec.Name]; ok {
			s.state.Resume(c)
		}
	}
	return nil
}

// savePools writes every set's pool to the deployment's parameter, whole: what it writes follows from the sets' state
// alone, so a repeated save, or one after a crash, leaves the same value.
func savePools(ctx context.Context, client paramStore, name string, sets []*Set, now time.Time, rule PoolRule) error {
	saved := carriedPools{At: now, Sets: map[string]Carried{}}
	for _, s := range sets {
		saved.Sets[s.spec.Name] = s.state.Carry(now, rule)
	}
	value, _ := json.Marshal(saved)
	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Type: ssmtypes.ParameterTypeString,
		Tier: ssmtypes.ParameterTierIntelligentTiering, Value: aws.String(string(value)), Overwrite: aws.Bool(true)})
	return err
}
