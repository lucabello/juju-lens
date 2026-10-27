// Package probebin holds the probe binaries that juju-lens carries inside
// itself: one static Linux build per architecture, which `record --attach
// unit` uploads to the units, and `--attach host --ssh-target` to the machine
// it records. They are built before juju-lens and embedded with the
// embedprobe build tag (`just build`, `just dist`); a juju-lens built without
// the tag, such as by `go install`, carries none.
package probebin

import (
	"errors"
	"fmt"
	"io"
)

// Open returns the probe for goarch ("amd64" or "arm64") and its size.
func Open(goarch string) (io.ReadCloser, int64, error) {
	if !Embedded {
		return nil, 0, errors.New("this juju-lens was built without the embedded probe")
	}
	name := "linux_" + goarch
	f, err := blobs.Open(name)
	if err != nil {
		return nil, 0, fmt.Errorf("no embedded probe for %s", goarch)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}
