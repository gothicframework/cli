package main

import (
	"context"
	"errors"

	gothic "github.com/gothicframework/core/config"
)

var Config = gothic.Config{
	ProjectName: "orchapp",
	Deploy: &gothic.DeployConfig{
		Provider: gothic.AWS,
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				Region:  "us-east-1",
				Profile: "default",
				Stages: map[string]gothic.Stage{
					"dev": {},
				},
			},
		},
	},
}

// BeforeDeploy always fails so the test can assert error propagation.
func BeforeDeploy(ctx context.Context, gctx *gothic.GothicContext) error {
	return errors.New("boom")
}
