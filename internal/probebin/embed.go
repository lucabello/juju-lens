//go:build embedprobe

package probebin

import "embed"

// Embedded says this build carries the probes.
const Embedded = true

// The files are built by `just build` and `just dist` before juju-lens is.
//
//go:embed linux_amd64 linux_arm64
var blobs embed.FS
