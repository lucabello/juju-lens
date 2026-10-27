//go:build !linux

package unit

import (
	"context"
	"errors"
	"io"
	"time"
)

// Config tunes a probe run; see the Linux build.
type Config struct {
	Version     string
	Out         io.Writer
	Logf        func(format string, args ...any)
	TapFeatures []string
	Linger      time.Duration
	PebbleLogs  bool
	ProcRoot    string
	AgentsDir   string
	TmpDir      string
}

// UploadPrefix starts the file name the recorder uploads the probe binary
// under.
const UploadPrefix = "juju-lens-probe-"

// Run needs Linux: Juju units only run there.
func Run(context.Context, Config) error {
	return errors.New("the unit probe runs only on Linux")
}
