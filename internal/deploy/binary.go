// Package tofu — binary.go implements the OpenTofu binary manager.
//
// Resolution order (three tiers), highest precedence first:
//
//  1. Config override: if cli.Config.TofuBinaryPath is set, that path is used
//     verbatim (after validating it exists and is executable). Nothing is
//     downloaded or cached.
//  2. Cache hit: if .gothicCli/bin/tofu already exists and is executable, it is
//     returned without touching the network.
//  3. Download: the pinned OpenTofu version is fetched via github.com/opentofu/tofudl,
//     written to .gothicCli/bin/tofu, marked executable, and returned.
package tofu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/opentofu/tofudl"
)

// BinaryManager resolves the OpenTofu binary, downloading and caching it under
// .gothicCli/bin/tofu when necessary.
type BinaryManager interface {
	// EnsureBinary returns the absolute (or project-relative) path to a usable
	// tofu binary.
	EnsureBinary(ctx context.Context) (string, error)
}

// PinnedTofuVersion is the single OpenTofu version this CLI downloads and runs.
// Never resolve "latest" at runtime — a pinned version keeps deploys
// reproducible and lets us vet the exact tofu behavior we ship against.
const PinnedTofuVersion = "1.9.0"

// cachePath is the on-disk location of the cached tofu binary, relative to the
// project root the CLI is invoked from.
var cachePath = filepath.Join(".gothicCli", "bin", "tofu")

// tofudlManager is the default BinaryManager implementation backed by tofudl.
type tofudlManager struct {
	config *cli.Config
}

// NewBinaryManager returns a BinaryManager that resolves the tofu binary using
// the config override → cache → download tiers documented at the package level.
func NewBinaryManager(c *cli.Config) BinaryManager {
	return &tofudlManager{config: c}
}

// init registers NewBinaryManager with pkg/cli so GothicCli.GetConfig() can
// construct a BinaryManager without pkg/cli importing this package (which would
// be an import cycle, since this package imports pkg/cli for the Config type).
func init() {
	cli.NewBinaryManager = func(c *cli.Config) cli.BinaryManager {
		return NewBinaryManager(c)
	}
	cli.NewDeploymentEngine = func(c *cli.Config, awsCfg any) (cli.DeploymentEngine, error) {
		cfg, ok := awsCfg.(aws.Config)
		if !ok {
			return nil, fmt.Errorf("NewDeploymentEngine: expected aws.Config, got %T", awsCfg)
		}
		return NewTofuAwsEngine(c, cfg)
	}
	cli.NewCDNEngine = func(c *cli.Config, awsCfg any) cli.CDNEngine {
		cfg, ok := awsCfg.(aws.Config)
		if !ok {
			return nil
		}
		return NewCloudFrontCDN(cfg)
	}
}

// isExecutable reports whether info has any of the executable permission bits set.
func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0111 != 0
}

func (m *tofudlManager) EnsureBinary(ctx context.Context) (string, error) {
	// Tier 1 — config override.
	if m.config != nil && m.config.TofuBinaryPath != "" {
		path := m.config.TofuBinaryPath
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("TofuBinaryPath %q is not accessible: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("TofuBinaryPath %q is a directory, not an executable", path)
		}
		if !isExecutable(info) {
			return "", fmt.Errorf("TofuBinaryPath %q is not executable (missing the executable bit)", path)
		}
		return path, nil
	}

	// Tier 2 — cache hit.
	if info, err := os.Stat(cachePath); err == nil && !info.IsDir() && isExecutable(info) {
		return cachePath, nil
	}

	// Tier 3 — download and cache.
	return m.download(ctx)
}

// download fetches the pinned OpenTofu binary and writes it to cachePath.
// Platform and architecture default to the current runtime when omitted, so we
// only constrain the version.
func (m *tofudlManager) download(ctx context.Context) (string, error) {
	dl, err := tofudl.New()
	if err != nil {
		return "", fmt.Errorf("initializing OpenTofu downloader: %w", err)
	}

	artifact, err := dl.Download(ctx, tofudl.DownloadOptVersion(PinnedTofuVersion))
	if err != nil {
		return "", fmt.Errorf("downloading OpenTofu %s: %w", PinnedTofuVersion, err)
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return "", fmt.Errorf("creating tofu cache directory: %w", err)
	}
	if err := os.WriteFile(cachePath, artifact, 0755); err != nil {
		return "", fmt.Errorf("writing tofu binary to %s: %w", cachePath, err)
	}
	// WriteFile honors umask, so re-assert the executable bit explicitly.
	if err := os.Chmod(cachePath, 0755); err != nil {
		return "", fmt.Errorf("setting executable bit on %s: %w", cachePath, err)
	}

	fmt.Fprintf(os.Stderr, "Downloaded OpenTofu %s to %s\n", PinnedTofuVersion, cachePath)
	return cachePath, nil
}
