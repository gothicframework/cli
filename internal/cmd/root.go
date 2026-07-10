/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"os"

	"github.com/spf13/cobra"

	// Blank import registers astconfig.Parse into pkg/cli.ConfigParser so
	// GetConfig can read gothic.config.go without an import cycle.
	_ "github.com/gothicframework/cli/v3/internal/astconfig"
)

type RunEFunc func(cmd *cobra.Command, args []string) error

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "gothic",
	Short: "Gothic — a server-rendered Go web framework with WASM client-side state.",
	Long: `Gothic is a server-rendered Go web framework combining HTMX, Templ, and
Alpine.js on the front end with client-side state compiled to WebAssembly,
and OpenTofu-based deploys to AWS.

Use the CLI to work through the whole app lifecycle:

  gothic init          scaffold a new Gothic project
  gothic hot-reload    run the dev server, watching templates, Go, and CSS
  gothic build         compile Templ files to Go
  gothic wasm          build the client-side WASM state modules
  gothic css           generate CSS from classes used in your templates
  gothic optimize-images  optimize project images
  gothic deploy        deploy the app to AWS`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

