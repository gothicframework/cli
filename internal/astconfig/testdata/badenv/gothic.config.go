package main

import gothic "github.com/gothicframework/core/config"

var Config = gothic.Config{
	ProjectName: "badenv",
	Deploy: &gothic.DeployConfig{
		Region: "us-east-1",
		Stages: map[string]gothic.Stage{
			"dev": {
				ENV: map[string]gothic.EnvValue{
					"X": gothic.MysteryFunc("y"),
				},
			},
		},
	},
}
