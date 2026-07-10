package main

import (
	"context"

	gothic "github.com/gothicframework/core/config"
)

var Config = gothic.Config{
	ProjectName:    "fullapp",
	TofuBinaryPath: "/usr/local/bin/tofu",
	WasmBinary:     "/opt/tinygo/bin/tinygo",
	TailwindBinary: "/opt/tailwind/tailwindcss",
	OptimizeImages: gothic.OptimizeImagesConfig{
		LowResolutionRate: 20,
	},
	Deploy: &gothic.DeployConfig{
		ServerMemory:  1024,
		ServerTimeout: 60,
		Region:        "us-west-2",
		Profile:       "prod-profile",
		Stages: map[string]gothic.Stage{
			"dev": {
				ENV: map[string]gothic.EnvValue{
					"PORT": gothic.Env("8080"),
				},
			},
			"prod": {
				HostedZoneId:   gothic.SSMParam("/fullapp/prod/hosted-zone"),
				CustomDomain:   gothic.Env("app.example.com"),
				CertificateArn: gothic.SecretsManager("/fullapp/prod/cert-arn"),
				WafArn:         gothic.Env("arn:aws:wafv2:us-west-2:111122223333:global/webacl/xyz"),
				ENV: map[string]gothic.EnvValue{
					"PORT":        gothic.Env("443"),
					"DB_URL":      gothic.SSMParam("/fullapp/prod/db-url"),
					"API_KEY":     gothic.SecretsManager("/fullapp/prod/api-key"),
					"JSON_SECRET": gothic.SecretsManager("/fullapp/prod/creds").Get("password"),
					"JSON_PARAM":  gothic.SSMParam("/fullapp/prod/config").Get("host"),
				},
			},
		},
	},
}

// BeforeDeploy is a top-level hook used to exercise HasHook.
func BeforeDeploy(ctx context.Context, gctx *gothic.GothicContext) error {
	return nil
}
