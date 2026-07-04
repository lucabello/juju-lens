package probe

import (
	"debug/elf"
	"debug/gosym"
	"fmt"

	"golang.org/x/arch/x86/x86asm"
)

// TLS boundary symbols we attach to. crypto/tls names are stable stdlib
// symbols present in every jujud/containeragent build; the jujuc symbol is
// present only on unit agents and is attached best-effort.
const (
	SymTLSWrite  = "crypto/tls.(*Conn).Write"
	SymTLSRead   = "crypto/tls.(*Conn).Read"
	SymJujucMain = "github.com/juju/juju/internal/worker/uniter/runner/jujuc.(*Jujuc).Main"
)

// Symbol is a resolved function. Vaddr is its link-time virtual address (what
// gosym reports); Entry is the corresponding byte offset into the ELF file,
// which is what the kernel uprobe PMU — and therefore link.Uprobe's
// UprobeOptions.Address — actually expects. Passing a virtual address where a
// file offset is required attaches the probe to the wrong instruction and it
// silently never fires, so the two are kept distinct.
type Symbol struct {
	Name  string
	Vaddr uint64
	Entry uint64 // file offset, ready to pass to UprobeOptions.Address
}

// ResolveSymbols opens the ELF at path and resolves each requested function
// name to its file offset using the Go pclntab. It works on stripped release
// binaries because .gopclntab (or its PIE/section-scan fallbacks) is always
// present in a Go binary even when .symtab is not. Names that cannot be found
// are returned in `missing` rather than failing the whole call, so a caller can
// attach the TLS probes even when the optional jujuc symbol is absent.
func ResolveSymbols(path string, names []string) (found []Symbol, missing []string, err error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open elf %q: %w", path, err)
	}
	defer f.Close()

	tab, err := goSymTable(f)
	if err != nil {
		return nil, nil, err
	}
	text, textData := textSection(f)
	for _, name := range names {
		fn := tab.LookupFunc(name)
		if fn == nil {
			missing = append(missing, name)
			continue
		}
		// Skip any leading ES/CS/SS/DS segment-override prefix Go's linker
		// emits as no-op alignment padding: the kernel's uprobe validator
		// rejects an instruction carrying one (ENOTSUPP), and the prefix is
		// ignored by the CPU in 64-bit mode, so probing one byte later hits the
		// same instruction with identical register state.
		vaddr := fn.Entry + segPrefixSkip(text, textData, fn.Entry)
		off, ok := fileOffset(f, vaddr)
		if !ok {
			missing = append(missing, name)
			continue
		}
		found = append(found, Symbol{Name: name, Vaddr: vaddr, Entry: off})
	}
	return found, missing, nil
}

// textSection returns the .text section and its bytes, or (nil, nil) if absent.
func textSection(f *elf.File) (*elf.Section, []byte) {
	s := f.Section(".text")
	if s == nil {
		return nil, nil
	}
	d, err := s.Data()
	if err != nil {
		return nil, nil
	}
	return s, d
}

// isSegPrefix reports whether b is an ES/CS/SS/DS segment-override prefix.
// These four are ignored by the CPU in long mode but rejected by the kernel's
// is_prefix_bad() when they lead a uprobe target instruction. FS (0x64) and GS
// (0x65) are deliberately excluded: they are meaningful (TLS) and the kernel
// accepts them.
func isSegPrefix(b byte) bool {
	return b == 0x26 || b == 0x2e || b == 0x36 || b == 0x3e
}

// segPrefixSkip returns how many leading no-op segment-override prefix bytes sit
// at vaddr, so the caller can advance the probe point past them. Returns 0 when
// the bytes are unavailable or the instruction has no such prefix.
func segPrefixSkip(text *elf.Section, data []byte, vaddr uint64) uint64 {
	if text == nil || vaddr < text.Addr || vaddr >= text.Addr+uint64(len(data)) {
		return 0
	}
	code := data[vaddr-text.Addr:]
	var n uint64
	for int(n) < len(code) && isSegPrefix(code[n]) {
		n++
	}
	return n
}

// ResolveRETs returns the ELF file offsets of every RET instruction in the
// named function. The read path attaches ordinary uprobes at these offsets
// instead of a uretprobe: uretprobes patch the return address on the stack,
// which corrupts Go processes (the runtime copies goroutine stacks and does its
// own unwinding). At a RET site the Go ABI has already placed the return value
// in RAX, so the same capture program works — without touching the stack.
func ResolveRETs(path, name string) ([]uint64, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open elf %q: %w", path, err)
	}
	defer f.Close()

	tab, err := goSymTable(f)
	if err != nil {
		return nil, err
	}
	fn := tab.LookupFunc(name)
	if fn == nil {
		return nil, fmt.Errorf("function %s not found", name)
	}
	text := f.Section(".text")
	if text == nil {
		return nil, fmt.Errorf("no .text section")
	}
	data, err := text.Data()
	if err != nil {
		return nil, fmt.Errorf("reading .text: %w", err)
	}
	if fn.Entry < text.Addr || fn.End > text.Addr+uint64(len(data)) || fn.End <= fn.Entry {
		return nil, fmt.Errorf("function %s out of .text range", name)
	}
	code := data[fn.Entry-text.Addr : fn.End-text.Addr]

	var rets []uint64
	for pos := 0; pos < len(code); {
		inst, err := x86asm.Decode(code[pos:], 64)
		if err != nil || inst.Len == 0 {
			pos++ // undecodable byte (rare); resync one byte at a time
			continue
		}
		if inst.Op == x86asm.RET {
			// Advance past any leading segment-override prefix for the same
			// reason as entry probes (the kernel rejects it with ENOTSUPP).
			skip := segPrefixSkip(text, data, fn.Entry+uint64(pos))
			if off, ok := fileOffset(f, fn.Entry+uint64(pos)+skip); ok {
				rets = append(rets, off)
			}
		}
		pos += inst.Len
	}
	if len(rets) == 0 {
		return nil, fmt.Errorf("no RET instructions found in %s", name)
	}
	return rets, nil
}

// LayoutInfo describes where a Go binary's text lives, for diagnosing symbol
// resolution mismatches. TextStart is the address the pclntab header records as
// the base for function PCs (Go 1.18+); when it differs from the .text section
// address, feeding gosym the section address skews every resolved entry.
type LayoutInfo struct {
	ELFType     string
	TextAddr    uint64 // .text section virtual address
	TextOff     uint64 // .text section file offset
	PclnSection string
	PtrSize     int
	PclnMagic   uint32
	TextStart   uint64 // pcHeader.textStart (0 if not the 1.18+ format)
}

// InspectLayout parses just enough of the ELF and pclntab header to report the
// text base mismatch that breaks entry resolution on some builds.
func InspectLayout(path string) (LayoutInfo, error) {
	var li LayoutInfo
	f, err := elf.Open(path)
	if err != nil {
		return li, err
	}
	defer f.Close()
	li.ELFType = f.Type.String()
	if s := f.Section(".text"); s != nil {
		li.TextAddr, li.TextOff = s.Addr, s.Offset
	}
	var pcln []byte
	for _, name := range []string{".gopclntab", ".data.rel.ro.gopclntab"} {
		if s := f.Section(name); s != nil {
			if d, derr := s.Data(); derr == nil {
				li.PclnSection, pcln = name, d
				break
			}
		}
	}
	if len(pcln) >= 8 {
		li.PclnMagic = uint32(pcln[0]) | uint32(pcln[1])<<8 | uint32(pcln[2])<<16 | uint32(pcln[3])<<24
		li.PtrSize = int(pcln[7])
		// Go 1.18+/1.20 header: magic(4) pad(2) minLC(1) ptrSize(1) then
		// nfunc, nfiles, textStart — each ptrSize wide. textStart is the 3rd.
		if li.PtrSize == 8 && len(pcln) >= 8+3*8 {
			off := 8 + 2*8
			li.TextStart = uint64(pcln[off]) | uint64(pcln[off+1])<<8 | uint64(pcln[off+2])<<16 |
				uint64(pcln[off+3])<<24 | uint64(pcln[off+4])<<32 | uint64(pcln[off+5])<<40 |
				uint64(pcln[off+6])<<48 | uint64(pcln[off+7])<<56
		}
	}
	return li, nil
}

// FuncContaining returns the name of the function whose body contains vaddr,
// using the Go pclntab. It is a diagnostic helper for --symbols (e.g. to name
// the target of a leading CALL in a function's prologue).
func FuncContaining(path string, vaddr uint64) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tab, err := goSymTable(f)
	if err != nil {
		return "", err
	}
	fn := tab.PCToFunc(vaddr)
	if fn == nil {
		return "", fmt.Errorf("no func at 0x%x", vaddr)
	}
	return fn.Name, nil
}

// DisasmFirst decodes the first x86-64 instruction in b and returns a human
// string including its length and any prefixes — used by --symbols to show
// exactly what instruction a uprobe would land on (a segment-override prefix
// such as 0x36/0x3e makes the kernel reject the probe with ENOTSUPP).
func DisasmFirst(b []byte) string {
	inst, err := x86asm.Decode(b, 64)
	if err != nil {
		return fmt.Sprintf("undecodable (% x)", b[:min(len(b), 8)])
	}
	return fmt.Sprintf("%-24s len=%d bytes=% x", x86asm.GNUSyntax(inst, 0, nil), inst.Len, b[:inst.Len])
}

// fileOffset translates a virtual address into a byte offset within the ELF
// file, using the PT_LOAD segment that contains it — the same conversion
// cilium/ebpf performs when it resolves a symbol itself
// (offset = vaddr - segment.Vaddr + segment.Off). The executable segment is
// preferred; a non-executable containing segment is a last resort.
func fileOffset(f *elf.File, vaddr uint64) (uint64, bool) {
	var fallback uint64
	haveFallback := false
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if vaddr < p.Vaddr || vaddr >= p.Vaddr+p.Memsz {
			continue
		}
		off := vaddr - p.Vaddr + p.Off
		if p.Flags&elf.PF_X != 0 {
			return off, true
		}
		fallback, haveFallback = off, true
	}
	return fallback, haveFallback
}

// goSymTable builds a gosym.Table from a Go binary's pclntab. It follows the
// same section fallback chain gojue/ecapture uses: .gopclntab first, then the
// PIE-mangled .data.rel.ro.gopclntab.
func goSymTable(f *elf.File) (*gosym.Table, error) {
	pclntab, err := pclntabData(f)
	if err != nil {
		return nil, err
	}
	ln := gosym.NewLineTable(pclntab, gosymTextBase(f, pclntab))
	// A stripped binary has an empty .gosymtab; gosym still resolves function
	// entries from the pclntab's own function table, which is what we need.
	var symtab []byte
	if s := f.Section(".gosymtab"); s != nil {
		if d, derr := s.Data(); derr == nil {
			symtab = d
		}
	}
	tab, err := gosym.NewTable(symtab, ln)
	if err != nil {
		return nil, fmt.Errorf("parsing pclntab: %w", err)
	}
	return tab, nil
}

// gosymTextBase returns the base address to feed gosym.NewLineTable: the value
// gosym adds to each function's stored PC offset. For Go 1.18+ pclntabs that is
// the pcHeader's textStart field (the address of runtime.text), which is NOT
// always the .text section address — some link layouts place a few bytes before
// runtime.text, so runtime.text sits inside .text at a small offset. Feeding the
// .text section address in that case skews every resolved entry by exactly that
// gap. We prefer textStart from the header and fall back to the .text section
// address for older pclntab formats that lack the field.
func gosymTextBase(f *elf.File, pcln []byte) uint64 {
	if ts, ok := pclnTextStart(pcln); ok {
		return ts
	}
	if s := f.Section(".text"); s != nil {
		return s.Addr
	}
	return 0
}

// pclnTextStart reads the textStart field from a Go 1.18+/1.20 pclntab header.
// Layout: magic(4) pad(2) minLC(1) ptrSize(1) nfunc(ptr) nfiles(ptr)
// textStart(ptr). Returns ok=false for other formats or a non-8-byte ptrSize.
func pclnTextStart(pcln []byte) (uint64, bool) {
	if len(pcln) < 8+3*8 {
		return 0, false
	}
	magic := uint32(pcln[0]) | uint32(pcln[1])<<8 | uint32(pcln[2])<<16 | uint32(pcln[3])<<24
	// 0xfffffff1 = Go 1.20, 0xfffffff0 = Go 1.18. Both carry textStart.
	if magic != 0xfffffff1 && magic != 0xfffffff0 {
		return 0, false
	}
	if pcln[7] != 8 { // ptrSize
		return 0, false
	}
	off := 8 + 2*8
	ts := uint64(pcln[off]) | uint64(pcln[off+1])<<8 | uint64(pcln[off+2])<<16 |
		uint64(pcln[off+3])<<24 | uint64(pcln[off+4])<<32 | uint64(pcln[off+5])<<40 |
		uint64(pcln[off+6])<<48 | uint64(pcln[off+7])<<56
	if ts == 0 {
		return 0, false
	}
	return ts, true
}

func pclntabData(f *elf.File) ([]byte, error) {
	for _, name := range []string{".gopclntab", ".data.rel.ro.gopclntab"} {
		if s := f.Section(name); s != nil {
			d, err := s.Data()
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			return d, nil
		}
	}
	return nil, fmt.Errorf("no .gopclntab section (is %s a Go binary?)", "target")
}
