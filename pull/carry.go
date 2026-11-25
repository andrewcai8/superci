package main

import (
	"context"
	"encoding/json"
	"errors"
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

// loadPools resumes each set from what the last listener saved, if it saved within the window: an older save says
// nothing about the jobs arriving now.
func loadPools(ctx context.Context, client *ssm.Client, name string, sets []*Set, window time.Duration, now time.Time) error {
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

func savePools(ctx context.Context, client *ssm.Client, name string, sets []*Set, now time.Time) error {
	saved := carriedPools{At: now, Sets: map[string]Carried{}}
	for _, s := range sets {
		saved.Sets[s.spec.Name] = s.state.Carry()
	}
	value, _ := json.Marshal(saved)
	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Type: ssmtypes.ParameterTypeString,
		Tier: ssmtypes.ParameterTierIntelligentTiering, Value: aws.String(string(value)), Overwrite: aws.Bool(true)})
	return err
}
