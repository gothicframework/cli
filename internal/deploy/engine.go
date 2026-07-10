// Package tofu wraps the OpenTofu binary, the Docker engine, and AWS SDK clients
// into a single deployment engine usable by cmd/deploy.go.
package tofu

import (
	"context"
)

// DeploymentEngine is the provider-agnostic contract used by cmd/deploy.go to
// run a full deploy/destroy lifecycle. The concrete implementation
// (TofuAwsEngine) holds the resolved *cli.Config and aws.Config internally, so
// callers only pass the stage. All resource names are computed by the engine.
type DeploymentEngine interface {
	// Prepare bootstraps state backend resources (S3 state bucket + DynamoDB
	// lock table) and generates the .tf.json working directory for the given
	// stage. Must be called before Build/Deploy/Destroy.
	Prepare(ctx context.Context, stage string) error

	// Build builds and pushes the container image; returns the image URI. The
	// repository name is the one computed in Prepare, so it is not passed here.
	Build(ctx context.Context, tag string) (imageURI string, err error)

	// Deploy runs tofu init + apply and returns the stack outputs.
	Deploy(ctx context.Context) (map[string]string, error)

	// Destroy runs tofu destroy.
	Destroy(ctx context.Context) error
}
