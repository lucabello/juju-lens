//go:build !embedprobe

package probebin

import "embed"

// Embedded says this build carries the probes.
const Embedded = false

var blobs embed.FS
