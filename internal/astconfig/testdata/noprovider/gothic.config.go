package main

import gothic "github.com/gothicframework/core/config"

// This fixture omits Deploy.Provider entirely so the parser must default it to AWS.
var Config = gothic.Config{
	ProjectName: "noproviderapp",
	Deploy: &gothic.DeployConfig{
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				Region:  "eu-west-1",
				Profile: "default",
				Stages: map[string]gothic.Stage{
					"dev": {},
				},
			},
		},
	},
}
