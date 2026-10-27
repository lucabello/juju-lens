// Package probecmd is the command line of the probe: what `juju-lens probe`
// and the juju-lens-probe command run. The probe attaches eBPF uprobes to the crypto/tls boundary of
// jujud/containeragent processes and streams the captured plaintext RPC frames
// to stdout as length-prefixed JSON. It is normally launched by `juju-lens
// record` (locally, or over juju ssh), but it is a standalone binary so it can
// be run and inspected on its own.
//
// With --unit it instead runs inside a unit (or on a machine) for `record
// --attach unit`, unprivileged, writing JSON lines (internal/unit).
package probecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/probe"
	"github.com/lucabello/juju-lens/internal/unit"
	"github.com/lucabello/juju-lens/internal/wire"
)

// version is the version of the program the probe runs in, set by Main.
var version = "dev"

// Main runs the probe with the command line args (without the program name)
// and exits the process when it is done. ver is the program's version.
func Main(ver string, args []string) {
	version = ver
	var (
		pids     []int
		jujuc    bool
		listPr   bool
		plain    bool
		decode   bool
		symbols  bool
		binPath  string
		ctrlUUID string
		modUUID  string

		unitMode   bool
		unlinkSelf bool
		noStacks   bool
		pebbleLogs bool
		lifetime   time.Duration
		linger     = unit.DefaultLinger
	)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--jujuc":
			jujuc = true
		case a == "--list":
			listPr = true
		case a == "--unit":
			unitMode = true
		case a == "--unlink-self":
			unlinkSelf = true
		case a == "--no-stacks":
			noStacks = true
		case a == "--pebble-logs":
			pebbleLogs = true
		case a == "--lifetime" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d <= 0 {
				fatal("invalid --lifetime %q: want a positive duration", args[i])
			}
			lifetime = d
		case a == "--linger" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				fatal("invalid --linger %q: want a duration, 0 or more", args[i])
			}
			linger = d
		case a == "--plain":
			plain = true
		case a == "--decode":
			decode = true
		case a == "--symbols":
			symbols = true
		case a == "--controller-uuid" && i+1 < len(args):
			i++
			ctrlUUID = strings.TrimSpace(args[i])
		case a == "--model-uuid" && i+1 < len(args):
			i++
			modUUID = strings.TrimSpace(args[i])
		case a == "--pid" && i+1 < len(args):
			i++
			for _, p := range strings.Split(args[i], ",") {
				n, err := strconv.Atoi(strings.TrimSpace(p))
				if err != nil {
					fatal("invalid --pid %q: %v", p, err)
				}
				pids = append(pids, n)
			}
		case a == "-h" || a == "--help":
			usage()
			return
		case !strings.HasPrefix(a, "-"):
			binPath = a // positional ELF path, for --symbols
		default:
			fatal("unknown argument %q (see --help)", a)
		}
	}

	if unlinkSelf {
		// The recorder streams this binary into /tmp for one session; remove
		// it now so nothing is left on the machine once the process exits.
		if exe, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(exe), unit.UploadPrefix) {
			_ = os.Remove(exe)
		}
	}
	if unitMode {
		runUnit(lifetime, linger, noStacks, pebbleLogs)
		return
	}

	if listPr {
		found, err := probe.EnumeratePIDs()
		if err != nil {
			fatal("%v", err)
		}
		for _, p := range found {
			fmt.Printf("%d\t%s\n", p, probe.UnitForPID(p))
		}
		return
	}

	if symbols {
		runSymbols(binPath, pids, jujuc)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := probe.AttachConfig{
		PIDs:        pids,
		AttachJujuc: jujuc,
		Filter:      probe.TopoFilter{ControllerUUID: ctrlUUID, ModelUUID: modUUID},
		Out:         os.Stdout,
		Logf:        func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) },
	}
	switch {
	case decode:
		// Verification mode: reassemble the websocket stream and print the
		// decoded RPC envelopes (requests un-masked). This is the same
		// pipeline `record` uses, so it proves end-to-end decode on live
		// traffic — including the masked client-side requests --plain can't
		// show readably.
		cfg.OnFrame = newEnvelopeDecoder().onFrame
		fmt.Fprintln(os.Stderr, "juju-lens-probe: --decode: printing decoded RPC envelopes (Ctrl-C to stop)")
	case plain:
		// Print each captured frame's raw plaintext. Server→client frames are
		// readable; client→server frames are websocket-masked and show as
		// bytes (use --decode to un-mask them).
		cfg.OnFrame = printPlain
		fmt.Fprintln(os.Stderr, "juju-lens-probe: --plain: printing captured frames (Ctrl-C to stop)")
	}
	if err := probe.Run(ctx, cfg); err != nil && ctx.Err() == nil {
		fatal("%v", err)
	}
}

// runUnit runs the unit probe until SIGINT/SIGTERM, the optional lifetime,
// or a broken stdout (the recorder went away). linger is how long workloads
// are sampled in bursts after a hook's last Pebble change; noStacks leaves
// the tap's stack sampling off.
func runUnit(lifetime, linger time.Duration, noStacks, pebbleLogs bool) {
	ctx, stop := unitContext()
	defer stop()
	if lifetime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lifetime)
		defer cancel()
	}
	features := unit.DefaultFeatures
	if noStacks {
		features = slices.DeleteFunc(slices.Clone(features), func(f string) bool { return f == unit.FeatureStacks })
	}
	err := unit.Run(ctx, unit.Config{
		Version:     version,
		Out:         os.Stdout,
		Logf:        func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) },
		TapFeatures: features,
		Linger:      linger,
		PebbleLogs:  pebbleLogs,
	})
	if err != nil {
		fatal("%v", err)
	}
}

// unitContext is cancelled when the recorder stops the unit probe: SIGINT,
// SIGTERM, or SIGHUP when its `juju ssh` session hangs up. SIGPIPE is
// ignored rather than notified: notifying it would turn a broken pipe on any
// descriptor into a stop, including the Pebble socket of a workload
// container that restarted. With it ignored, a write to the recorder's
// closed stdout fails with EPIPE instead of killing the process, and the
// probe stops on that error after removing what it installed.
func unitContext() (context.Context, context.CancelFunc) {
	signal.Ignore(syscall.SIGPIPE)
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

// runSymbols resolves the TLS-boundary symbols in a target binary without
// attaching anything — a non-privileged first check that .gopclntab parsing
// works on the real (stripped) jujud/containeragent. Target is a positional
// ELF path or, with --pid N, /proc/N/exe.
func runSymbols(binPath string, pids []int, jujuc bool) {
	path := binPath
	if path == "" {
		if len(pids) != 1 {
			fatal("--symbols needs a binary path or exactly one --pid")
		}
		path = fmt.Sprintf("/proc/%d/exe", pids[0])
	}
	if li, lerr := probe.InspectLayout(path); lerr == nil {
		fmt.Printf("layout: type=%s .text{addr=0x%x off=0x%x} pcln=%s magic=0x%x ptrSize=%d textStart=0x%x\n",
			li.ELFType, li.TextAddr, li.TextOff, li.PclnSection, li.PclnMagic, li.PtrSize, li.TextStart)
		if li.TextStart != 0 && li.TextStart != li.TextAddr {
			fmt.Printf("  !! textStart (0x%x) != .text addr (0x%x): gosym base is skewed by 0x%x\n",
				li.TextStart, li.TextAddr, li.TextStart-li.TextAddr)
		}
	}
	names := []string{probe.SymTLSWrite, probe.SymTLSRead}
	if jujuc {
		names = append(names, probe.SymJujucMain)
	}
	found, missing, err := probe.ResolveSymbols(path, names)
	if err != nil {
		fatal("resolving symbols in %s: %v", path, err)
	}
	// Open the ELF so we can also report the actual byte at each resolved
	// offset. A 0xCC there is exactly what makes the kernel reject a uprobe
	// with ENOTSUPP ("possible trap insn"): either our offset is wrong (lands
	// on inter-function padding) or a stale breakpoint is still installed.
	bin, berr := os.Open(path)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "note: cannot open %s for byte-check: %v\n", path, berr)
	} else {
		defer bin.Close()
	}
	byteAt := func(off uint64) string {
		if bin == nil {
			return "??"
		}
		var b [1]byte
		if _, err := bin.ReadAt(b[:], int64(off)); err != nil {
			return "??"
		}
		flag := ""
		if b[0] == 0xcc {
			flag = " <-- 0xCC TRAP (uprobe will fail ENOTSUPP)"
		}
		return fmt.Sprintf("0x%02x%s", b[0], flag)
	}
	disasmAt := func(off uint64) string {
		if bin == nil {
			return ""
		}
		var b [16]byte
		n, _ := bin.ReadAt(b[:], int64(off))
		if n == 0 {
			return ""
		}
		return probe.DisasmFirst(b[:n])
	}
	callTarget := func(vaddr, off uint64) string {
		if bin == nil {
			return ""
		}
		var b [16]byte
		n, _ := bin.ReadAt(b[:], int64(off))
		if n == 0 || b[0] != 0xe8 { // e8 = CALL rel32
			return ""
		}
		rel := int32(uint32(b[1]) | uint32(b[2])<<8 | uint32(b[3])<<16 | uint32(b[4])<<24)
		target := vaddr + 5 + uint64(int64(rel))
		name, err := probe.FuncContaining(path, target)
		if err != nil {
			return fmt.Sprintf(" -> 0x%x (%v)", target, err)
		}
		return fmt.Sprintf(" -> %s", name)
	}
	for _, s := range found {
		fmt.Printf("%-70s vaddr=0x%x  file-offset=0x%x  byte=%s\n", s.Name, s.Vaddr, s.Entry, byteAt(s.Entry))
		fmt.Printf("    entry insn: %s%s\n", disasmAt(s.Entry), callTarget(s.Vaddr, s.Entry))
		if s.Name == probe.SymTLSRead {
			rets, rerr := probe.ResolveRETs(path, probe.SymTLSRead)
			if rerr != nil {
				fmt.Printf("    RET sites: %v\n", rerr)
				continue
			}
			for i, r := range rets {
				fmt.Printf("    ret[%d] file-offset=0x%x  byte=%s\n", i, r, byteAt(r))
			}
		}
	}
	for _, m := range missing {
		fmt.Printf("%-70s MISSING\n", m)
	}
	if len(found) == 0 {
		fatal("no target symbols resolved (is %s a Go binary?)", path)
	}
}

// envelopeDecoder reassembles the websocket stream per (pid, conn) and prints
// each decoded RPC envelope. It mirrors what internal/probe.Ingest does, so a
// clean stream here means `record` will index the same data.
type envelopeDecoder struct {
	demuxes map[[2]uint64]*wire.ConnDemux
}

func newEnvelopeDecoder() *envelopeDecoder {
	return &envelopeDecoder{demuxes: map[[2]uint64]*wire.ConnDemux{}}
}

func (d *envelopeDecoder) onFrame(f probe.Frame) error {
	key := [2]uint64{uint64(f.PID), f.Conn}
	dx := d.demuxes[key]
	if dx == nil {
		dx = wire.NewConnDemux(f.Conn)
		d.demuxes[key] = dx
	}
	dir := wire.DirRead
	if f.Dir == "write" {
		dir = wire.DirWrite
	}
	who := topoTag(f)
	for _, e := range dx.Feed(dir, f.Data) {
		if e.IsRequest() {
			fmt.Printf("[req  %s rid=%d] %s.%s %s\n",
				who, e.RequestID, e.Type, e.Request, preview(e.Params, 200))
		} else if e.Error != "" {
			fmt.Printf("[resp %s rid=%d] ERROR %s %s\n",
				who, e.RequestID, e.ErrorCode, e.Error)
		} else {
			fmt.Printf("[resp %s rid=%d] %s\n",
				who, e.RequestID, preview(e.Response, 200))
		}
	}
	return nil
}

// topoTag renders the capture's topology compactly for the verification views:
// unit (or kind) plus a short model id, so multi-agent output is legible.
func topoTag(f probe.Frame) string {
	who := f.Unit
	if who == "" {
		who = fmt.Sprintf("pid=%d", f.PID)
	}
	if f.Model != "" {
		m := f.Model
		if len(m) > 8 {
			m = m[:8]
		}
		who += " model=" + m
	}
	return who
}

// preview compacts a JSON payload and truncates it for one-line display.
func preview(raw json.RawMessage, max int) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	s := buf.String()
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(+%d)", len(s)-max)
	}
	return s
}

// printPlain renders one captured frame: a header plus a bounded, printable
// preview of the plaintext (non-printable bytes shown as '.').
func printPlain(f probe.Frame) error {
	const previewMax = 512
	data := f.Data
	truncated := ""
	if len(data) > previewMax {
		data = data[:previewMax]
		truncated = fmt.Sprintf(" …(+%d bytes)", len(f.Data)-previewMax)
	}
	buf := make([]byte, len(data))
	for i, b := range data {
		if b >= 0x20 && b < 0x7f {
			buf[i] = b
		} else {
			buf[i] = '.'
		}
	}
	unit := f.Unit
	if unit == "" {
		unit = "-"
	}
	fmt.Printf("[%s pid=%d conn=%d unit=%s len=%d] %s%s\n",
		f.Dir, f.PID, f.Conn, unit, len(f.Data), string(buf), truncated)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `juju-lens-probe — stream Juju API RPC frames captured at the TLS boundary

Usage:
  juju-lens-probe [--pid N[,N...]] [--jujuc]         # emit the binary frame stream
  juju-lens-probe --decode [--pid N[,N...]]          # print decoded RPC envelopes (verify pipeline)
  juju-lens-probe --plain [--pid N[,N...]]           # print raw captured plaintext
  juju-lens-probe --symbols [--pid N | <binary>]     # resolve TLS symbols only (no attach, no root)
  juju-lens-probe --list                             # list candidate PIDs and exit
  juju-lens-probe --unit [--lifetime D] [--linger D] [--no-stacks] [--pebble-logs] # run inside a unit for record --attach unit (JSON lines)

Flags:
  --pid N[,N...]       attach to these PIDs (default: all jujud/containeragent)
  --controller-uuid U  in dynamic mode, only attach to agents in controller U
  --model-uuid U       in dynamic mode, only attach to agents in model U
  --jujuc              also probe jujuc.(*Jujuc).Main on unit agents
  --decode         reassemble websockets and print decoded RPC envelopes (un-masks requests)
  --plain          print each captured frame's raw plaintext
  --symbols        resolve crypto/tls (and, with --jujuc, jujuc) symbols and exit
  --list           list candidate jujud/containeragent PIDs and exit
  --unit           run as the unit probe; needs no privileges beyond the unit's
  --lifetime D     with --unit, exit after D
  --linger D       with --unit, sample workloads 10 times a second until D after a hook's
                   last Pebble change is ready (default 30s; record --linger)
  --unlink-self    with --unit, delete this binary at start (used by juju-lens record)
  --no-stacks      with --unit, don't sample the charms' Python stacks (record --profile=false)
  --pebble-logs    with --unit, also stream the workload containers' service logs through
                   their Pebble (record --k8s-log on Kubernetes)

Without --plain, frames are written to stdout as 4-byte-length-prefixed JSON.
Attaching requires Linux kernel >= 5.8 and CAP_BPF (or root); --symbols does not.
`)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "juju-lens-probe: "+format+"\n", a...)
	os.Exit(1)
}
