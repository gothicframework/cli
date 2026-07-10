package main

import gothic "github.com/gothicframework/core/config"

// This fixture selects a provider that does not exist yet (GCP) so the parser
// must reject it with a clear "unknown deploy provider" error.
var Config = gothic.Config{
	ProjectName: "badproviderapp",
	Deploy: &gothic.DeployConfig{
		Provider: gothic.GCP,
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				Region: "us-east-1",
				Stages: map[string]gothic.Stage{
					"dev": {},
				},
			},
		},
	},
}
