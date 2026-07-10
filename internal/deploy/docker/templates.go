// Package docker wraps the Docker engine SDK and the AWS ECR SDK to build the
// Gothic Lambda image and push it to Elastic Container Registry. It replaces the
// `sam build` + ECR push previously performed by the SAM CLI.
//
// Provider Dockerfiles are embedded in the CLI binary (never seeded to a user's
// project on disk). They are selected at build time by the deployment provider
// (e.g. "aws", "gcp"). A user may override the embedded Dockerfile by setting
// DockerfilePath in gothic.config.go.
package docker

import "embed"

// DockerfileFS holds the provider-specific Dockerfiles compiled into the binary.
//
//go:embed embedded
var DockerfileFS embed.FS
