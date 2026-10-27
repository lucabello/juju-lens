//go:build embedprobe

package probebin

import (
	"bytes"
	"debug/elf"
	"io"
	"testing"
)

// TestEmbeddedProbes: each embedded probe is a static Linux executable for its
// architecture, so it runs in any unit image.
func TestEmbeddedProbes(t *testing.T) {
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		rc, size, err := Open(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || int64(len(data)) != size || size < 1<<20 {
			t.Fatalf("%s: read %d of %d bytes: %v", arch, len(data), size, err)
		}
		f, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: not an ELF file: %v", arch, err)
		}
		if f.Machine != machine || f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
			t.Errorf("%s: machine %v type %v", arch, f.Machine, f.Type)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("%s: dynamically linked: units may not have its libc", arch)
			}
		}
	}
}
