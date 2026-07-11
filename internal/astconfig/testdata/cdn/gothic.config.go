package main

import (
	gothic "github.com/gothicframework/core/config"
)

var Config = gothic.Config{
	ProjectName: "cdnapp",
	Deploy: &gothic.DeployConfig{
		Providers: gothic.Providers{
			AWS: gothic.AWSProvider{
				Region: "us-east-1",
				CDN: gothic.CDNConfig{
					// QueryParams omitted → defaults to AllowAll.
					Cookies: gothic.Allow("session", "theme"),
					Headers: gothic.Allow("CloudFront-Viewer-Country"),
				},
			},
		},
	},
}
