package main

import (
	"context"

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

// BeforeDeploy mutates the context so the test can assert the round-trip.
func BeforeDeploy(ctx context.Context, gctx *gothic.GothicContext) error {
	if gctx.Outputs == nil {
		gctx.Outputs = map[string]string{}
	}
	gctx.Outputs["test"] = "ok"
	return nil
}
