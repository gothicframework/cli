package main

import (
	"os"

	gothic "github.com/gothicframework/core/config"
)

var Config = gothic.Config{
	ProjectName: "dynamicapp",
	Deploy: &gothic.DeployConfig{
		Provider: gothic.AWS,
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				Region: "us-east-1",
				Stages: map[string]gothic.Stage{
					"dev": {
						ENV: map[string]gothic.EnvValue{
							"PORT":    gothic.Env("8080"),
							"DYNAMIC": gothic.Env(os.Getenv("X")),
						},
					},
				},
			},
		},
	},
}
