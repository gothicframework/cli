package main

import gothic "github.com/gothicframework/core/config"

var Config = gothic.Config{
	ProjectName: "basicapp",
	Deploy: &gothic.DeployConfig{
		Provider: gothic.AWS,
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				ServerMemory:  512,
				ServerTimeout: 30,
				Region:        "us-east-1",
				Profile:       "default",
				Stages: map[string]gothic.Stage{
					"dev": {
						ENV: map[string]gothic.EnvValue{
							"PORT": gothic.Env("8080"),
						},
					},
				},
			},
		},
	},
}
