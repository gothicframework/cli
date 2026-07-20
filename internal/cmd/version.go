/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"

	gothci_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/spf13/cobra"
)

// CURRENT_VERSION is the CLI / product ("generation") version — what `gothic
// version` prints and what the `cli/v3` module is tagged at.
var CURRENT_VERSION string = "v3.5.0"

// FrameworkModules are the published framework libraries a freshly-scaffolded
// project imports, each pinned to the version this CLI scaffolds against. The
// libraries version INDEPENDENTLY of the CLI generation (a pin per module), so
// these are v1.x while the CLI is v3.x — see 01_architecture / 16_adrs.
//
// ONLY `gothic init` writes these versions (via InitializeModule). No other CLI
// command rewrites the user's go.mod, so a project may bump any of them later.
// To ship new library versions, bump the entries here.
var FrameworkModules = []gothci_cli.FrameworkModule{
	{Path: "github.com/gothicframework/core", Version: "v1.5.0"},
	{Path: "github.com/gothicframework/components", Version: "v1.2.0"},
	{Path: "github.com/gothicframework/middlewares", Version: "v1.2.0"},
}

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show current Gothic Framework Version",
	Long:  `Show current Gothic Framework Version`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Gothic Framework - %s\n", CURRENT_VERSION)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
