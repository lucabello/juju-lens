//go:build !linux

package probe

import (
	"context"
	"fmt"
	"runtime"
)

// AttachConfig configures a probe run. On non-Linux platforms the probe cannot
// attach eBPF uprobes; the type exists so the CLI compiles everywhere and fails
// with a clear message at runtime.
type AttachConfig struct {
	PIDs        []int
	AttachJujuc bool
	Filter      TopoFilter
	Out         interface{ Write([]byte) (int, error) }
	OnFrame     func(Frame) error
	Logf        func(format string, args ...any)
}

// Run is unsupported off Linux. eBPF uprobes require a Linux kernel ≥ 5.8 on
// the host running the target process (VISION §9.9).
func Run(_ context.Context, _ AttachConfig) error {
	return fmt.Errorf("juju-lens-probe requires Linux (eBPF uprobes); this host is %s", runtime.GOOS)
}

// EnumeratePIDs is unsupported off Linux.
func EnumeratePIDs() ([]int, error) {
	return nil, fmt.Errorf("process enumeration requires Linux; this host is %s", runtime.GOOS)
}

// UnitForPID is unsupported off Linux.
func UnitForPID(int) string { return "" }
