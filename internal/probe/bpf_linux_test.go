//go:build linux

package probe

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestBPFProgramsAssemble marshals every probe program's instructions. This
// resolves all symbolic jump targets and map references and encodes each
// instruction, so it catches mislabelled jumps, dangling references, and
// malformed operands — the failure modes most likely in hand-written eBPF asm.
// It does not require a kernel; loading/attaching is validated on real hardware.
func TestBPFProgramsAssemble(t *testing.T) {
	spec := buildCollectionSpec()
	if len(spec.Programs) != 3 {
		t.Fatalf("expected 3 programs, got %d", len(spec.Programs))
	}
	for name, prog := range spec.Programs {
		if len(prog.Instructions) == 0 {
			t.Errorf("program %q has no instructions", name)
			continue
		}
		var buf bytes.Buffer
		if err := prog.Instructions.Marshal(&buf, binary.LittleEndian); err != nil {
			t.Errorf("program %q failed to assemble: %v", name, err)
		}
	}
	// The ring-buffer sample layout must stay in sync with decodeEvent.
	if evData != 28 || eventSize != evData+maxData {
		t.Fatalf("event layout drifted: evData=%d eventSize=%d", evData, eventSize)
	}
}
