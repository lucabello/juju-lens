// Command juju-lens-probe is the probe on its own: `juju-lens` embeds it, runs
// it as `juju-lens probe`, and uploads it to the units for `record --attach
// unit`. See internal/probecmd.
package main

import (
	"os"

	"github.com/lucabello/juju-lens/internal/probecmd"
)

// version is injected at build time via -ldflags, like juju-lens's.
var version = "dev"

func main() { probecmd.Main(version, os.Args[1:]) }
