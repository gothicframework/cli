// Command gothic is the Gothic Framework CLI entrypoint.
//
// The binary is deliberately produced from cmd/gothic/main.go so that
// `go install github.com/gothicframework/cli/v4/cmd/gothic` names the
// resulting executable "gothic" (after the cmd/ leaf dir), independent of
// the module path.
package main

import "github.com/gothicframework/cli/v4/internal/cmd"

func main() {
	cmd.Execute()
}
