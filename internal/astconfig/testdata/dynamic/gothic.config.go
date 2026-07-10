package main

import (
	"os"

	gothic "github.com/gothicframework/core/config"
)

var Config = gothic.Config{
	ProjectName: "dynamicapp",
	Deploy: &gothic.DeployConfig{
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
}
