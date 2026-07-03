// Command juju-lens is the CLI entry point. All subcommand wiring lives in
// internal/cli so this file stays tiny and only handles version metadata
// and top-level exit codes.
package main

import (
	"fmt"
	"os"

	"github.com/lucabello/juju-lens/internal/cli"
)

// version and commit are injected at build time via -ldflags. Defaults keep
// `go run` output sane during development.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := cli.Execute(cli.BuildInfo{Version: version, Commit: commit}); err != nil {
		fmt.Fprintln(os.Stderr, "juju-lens:", err)
		os.Exit(1)
	}
}
