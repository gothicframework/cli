package cli

import (
	"fmt"
	"regexp"

	config "github.com/gothicframework/core/config"
)

var validS3BucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)
var validStageName = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

func ValidateBucketName(name string) error {
	if !validS3BucketName.MatchString(name) {
		return fmt.Errorf("invalid S3 bucket name %q: must be 3-63 characters, lowercase alphanumeric, dots, or hyphens", name)
	}
	return nil
}

func ValidateStageName(name string) error {
	if !validStageName.MatchString(name) {
		return fmt.Errorf("invalid stage name %q: must be alphanumeric only", name)
	}
	return nil
}

type Config struct {
	ProjectName       string `json:"projectName"`
	GoModName         string `json:"goModuleName"`
	TofuBinaryPath    string `json:"tofuBinaryPath,omitempty"`
	DockerfilePath    string `json:"dockerfilePath,omitempty"`
	TailwindBinary    string `json:"tailwindBinary,omitempty"`
	WasmBinary        string `json:"wasmBinary,omitempty"`
	WasmTinyGoVersion string `json:"wasmTinyGoVersion,omitempty"`
	OptimizeImages    struct {
		LowResolutionRate int `json:"lowResolutionRate"`
		Quality           int `json:"quality"`
	} `json:"optimizeImages"`
	Deploy *DeployConfig `json:"deploy"`
}

// Provider mirrors config.Provider: the internal enum the AST parser produces to
// select a deploy provider. AWS is the zero value / only supported value in v3.
type Provider int

const (
	AWS Provider = iota
)

// DeployConfig mirrors config.DeployConfig: a provider selector plus per-provider
// settings. The AWS-specific fields moved under Providers.AWS.
type DeployConfig struct {
	Provider  Provider  `json:"provider"`
	Providers Providers `json:"providers"`
}

// Providers mirrors config.Providers.
type Providers struct {
	AWS AWSProvider `json:"aws"`
}

// AWSProvider mirrors config.AWSProvider: the AWS-specific deploy settings.
// CDN reuses the user-facing config.CDNConfig type directly (like the per-stage
// *config.EnvValue fields) so the CloudFront distribution knobs have a single
// source of truth; the parser fills it and tfgen consumes it.
type AWSProvider struct {
	ServerMemory  int                     `json:"serverMemory"`
	ServerTimeout int                     `json:"serverTimeout"`
	Region        string                  `json:"region"`
	Profile       string                  `json:"profile"`
	Stages        map[string]EnvVariables `json:"stages"`
	CDN           config.CDNConfig        `json:"cdn"`
}
type EnvVariables struct {
	BucketName     string
	LambdaName     string
	HostedZoneId   *config.EnvValue           `json:"hostedZoneId"`
	CustomDomain   *config.EnvValue           `json:"customDomain"`
	CertificateArn *config.EnvValue           `json:"certificateArn"`
	WafArn         *config.EnvValue           `json:"wafArn"`
	ENV            map[string]config.EnvValue `json:"env,omitempty"`
}
