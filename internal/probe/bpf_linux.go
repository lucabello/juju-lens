//go:build linux

package probe

import (
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
)

// This file builds the eBPF programs juju-lens-probe attaches at the crypto/tls
// boundary. The programs are assembled in Go (via cilium/ebpf's asm package)
// rather than compiled from C, so the module builds with no clang/libbpf
// toolchain — matching VISION's "pure Go" constraint.
//
// Register layout (amd64, Go internal ABI): integer arguments are passed in
// RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11 and integer results start in RAX.
// For crypto/tls.(*Conn).Write(b []byte)/Read(b []byte):
//
//	RAX = receiver (*tls.Conn)   -> used as the connection id
//	RBX = b.ptr                  -> plaintext buffer
//	RCX = b.len                  -> buffer length (Write: bytes to send)
//	RAX (at return) = n          -> bytes actually read (Read)
//
// These offsets are the current-toolchain row of the per-Go-version table
// VISION §5.1 describes; a toolchain bump that changes the register ABI adds a
// new row here. pt_regs offsets are the stable x86_64 kernel layout.
const (
	// x86_64 struct pt_regs byte offsets.
	regRBX = 40
	regRAX = 80
	regRCX = 88

	// event field offsets within the ringbuf sample.
	evTs   = 0  // u64 ktime
	evPID  = 8  // u32 tgid
	evDir  = 12 // u32 direction (0=write, 1=read)
	evConn = 16 // u64 *tls.Conn
	evLen  = 24 // u32 captured length
	evData = 28 // u8[maxData]

	maxData   = 16384
	eventSize = evData + maxData

	dirWrite = 0
	dirRead  = 1
)

// buildCollectionSpec assembles the maps and programs for the probe. The
// returned spec is loaded by the OS-specific attach code.
func buildCollectionSpec() *ebpf.CollectionSpec {
	events := &ebpf.MapSpec{
		Name: "events",
		Type: ebpf.RingBuf,
		// 64 MiB: headroom to absorb a burst (e.g. many units running hooks
		// within the same second at bootstrap/scale-out) without the kernel
		// program's bpf_ringbuf_reserve failing — see emitInsns' "ring full:
		// drop" path. This alone doesn't fix a *sustained* overload (only a
		// faster/decoupled userspace reader does, see run_linux.go's Run), but
		// it's a cheap, safe first line of defence against short spikes, and
		// rlimit.RemoveMemlock (called in Run) means it isn't rlimit-bounded
		// (M13).
		MaxEntries: 1 << 26,
	}
	// readStash carries (conn, ptr) from Read's entry uprobe to its return
	// uretprobe, keyed by pid_tgid. Value is two u64s laid out {conn, ptr}.
	readStash := &ebpf.MapSpec{
		Name:       "read_stash",
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  16,
		MaxEntries: 4096,
	}

	return &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			"events":     events,
			"read_stash": readStash,
		},
		Programs: map[string]*ebpf.ProgramSpec{
			"probe_write":    {Name: "probe_write", Type: ebpf.Kprobe, License: "GPL", Instructions: writeEnterInsns()},
			"probe_read_in":  {Name: "probe_read_in", Type: ebpf.Kprobe, License: "GPL", Instructions: readEnterInsns()},
			"probe_read_ret": {Name: "probe_read_ret", Type: ebpf.Kprobe, License: "GPL", Instructions: readRetInsns()},
		},
	}
}

// emitInsns appends the shared "reserve a sample, fill header, copy `size`
// bytes from `srcPtrReg`, submit" tail. On entry: R6=connValue, R7=ptrValue,
// R8=len(unclamped). It clobbers R0-R5 and R9. `dir` selects the direction
// field. `doneLabel` must be unique per program.
func emitInsns(dir int64, doneLabel string) asm.Instructions {
	return asm.Instructions{
		// Clamp len (R8) to maxData.
		asm.JLE.Imm(asm.R8, maxData, doneLabel+"_clamped"),
		asm.Mov.Imm(asm.R8, maxData),
		asm.Mov.Imm(asm.R0, 0).WithSymbol(doneLabel + "_clamped"),
		asm.JNE.Imm(asm.R8, 0, doneLabel+"_nonempty"),
		asm.Return(), // nothing to capture
		// Reserve a fixed-size sample from the ring buffer.
		asm.LoadMapPtr(asm.R1, 0).WithReference("events").WithSymbol(doneLabel + "_nonempty"),
		asm.Mov.Imm(asm.R2, eventSize),
		asm.Mov.Imm(asm.R3, 0),
		asm.FnRingbufReserve.Call(),
		asm.JNE.Imm(asm.R0, 0, doneLabel+"_reserved"),
		asm.Return(), // ring full: drop (reported by the reader as a gap)
		asm.Mov.Reg(asm.R9, asm.R0).WithSymbol(doneLabel + "_reserved"), // R9 = sample ptr
		// ts = bpf_ktime_get_ns()
		asm.FnKtimeGetNs.Call(),
		asm.StoreMem(asm.R9, evTs, asm.R0, asm.DWord),
		// pid = bpf_get_current_pid_tgid() >> 32
		asm.FnGetCurrentPidTgid.Call(),
		asm.RSh.Imm(asm.R0, 32),
		asm.StoreMem(asm.R9, evPID, asm.R0, asm.Word),
		// dir, conn, len
		asm.StoreImm(asm.R9, evDir, dir, asm.Word),
		asm.StoreMem(asm.R9, evConn, asm.R6, asm.DWord),
		asm.StoreMem(asm.R9, evLen, asm.R8, asm.Word),
		// bpf_probe_read_user(dst=&sample.data, size=R8, src=R7)
		asm.Mov.Reg(asm.R1, asm.R9),
		asm.Add.Imm(asm.R1, evData),
		asm.Mov.Reg(asm.R2, asm.R8),
		asm.Mov.Reg(asm.R3, asm.R7),
		asm.FnProbeReadUser.Call(),
		// bpf_ringbuf_submit(sample, 0)
		asm.Mov.Reg(asm.R1, asm.R9),
		asm.Mov.Imm(asm.R2, 0),
		asm.FnRingbufSubmit.Call(),
		asm.Mov.Imm(asm.R0, 0).WithSymbol(doneLabel),
		asm.Return(),
	}
}

// writeEnterInsns captures Write's buffer at entry (data is already present).
func writeEnterInsns() asm.Instructions {
	ins := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),                    // R6 = ctx temporarily
		asm.LoadMem(asm.R7, asm.R6, regRBX, asm.DWord), // ptr
		asm.LoadMem(asm.R8, asm.R6, regRCX, asm.DWord), // len
		asm.LoadMem(asm.R6, asm.R6, regRAX, asm.DWord), // conn (overwrites ctx; done with it)
	}
	return append(ins, emitInsns(dirWrite, "w_done")...)
}

// readEnterInsns stashes (conn, ptr) so the return probe can copy the bytes
// that Read fills in.
func readEnterInsns() asm.Instructions {
	return asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1), // ctx
		// value = {conn, ptr} on the stack at R10-16.
		asm.LoadMem(asm.R1, asm.R6, regRAX, asm.DWord), // conn
		asm.StoreMem(asm.RFP, -16, asm.R1, asm.DWord),
		asm.LoadMem(asm.R1, asm.R6, regRBX, asm.DWord), // ptr
		asm.StoreMem(asm.RFP, -8, asm.R1, asm.DWord),
		// key = pid_tgid on the stack at R10-24.
		asm.FnGetCurrentPidTgid.Call(),
		asm.StoreMem(asm.RFP, -24, asm.R0, asm.DWord),
		// bpf_map_update_elem(&read_stash, &key, &value, BPF_ANY)
		asm.LoadMapPtr(asm.R1, 0).WithReference("read_stash"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -24),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, -16),
		asm.Mov.Imm(asm.R4, 0), // BPF_ANY
		asm.FnMapUpdateElem.Call(),
		asm.Mov.Imm(asm.R0, 0),
		asm.Return(),
	}
}

// readRetInsns looks up the stashed buffer and copies the actual bytes read
// (the return value n in RAX) out to the ring buffer.
func readRetInsns() asm.Instructions {
	ins := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1), // ctx
		// n = return value (RAX). Bail if n <= 0.
		asm.LoadMem(asm.R8, asm.R6, regRAX, asm.DWord),
		asm.JSGT.Imm(asm.R8, 0, "r_have_n"),
		asm.Mov.Imm(asm.R0, 0),
		asm.Return(),
		// key = pid_tgid; lookup stash.
		asm.FnGetCurrentPidTgid.Call().WithSymbol("r_have_n"),
		asm.StoreMem(asm.RFP, -8, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, 0).WithReference("read_stash"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JNE.Imm(asm.R0, 0, "r_found"),
		asm.Return(), // no stashed entry (missed the enter probe)
		// R0 -> {conn, ptr}. Load into R6/R7, then delete the entry.
		asm.LoadMem(asm.R6, asm.R0, 0, asm.DWord).WithSymbol("r_found"), // conn
		asm.LoadMem(asm.R7, asm.R0, 8, asm.DWord),                       // ptr
		asm.FnGetCurrentPidTgid.Call(),
		asm.StoreMem(asm.RFP, -8, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, 0).WithReference("read_stash"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -8),
		asm.FnMapDeleteElem.Call(),
	}
	return append(ins, emitInsns(dirRead, "r_done")...)
}
