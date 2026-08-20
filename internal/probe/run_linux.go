//go:build linux

package probe

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// AttachConfig configures a probe run.
type AttachConfig struct {
	// PIDs to attach to. When empty, EnumeratePIDs is used to find every
	// jujud/containeragent process on the host.
	PIDs []int
	// AttachJujuc also probes jujuc.(*Jujuc).Main where present (unit agents).
	AttachJujuc bool
	// Filter, when non-empty, restricts attachment to agents whose topology
	// matches (a single controller or a single model). Only meaningful in
	// dynamic mode (no explicit PIDs).
	Filter TopoFilter
	// Out receives the length-prefixed frame stream (usually os.Stdout).
	Out io.Writer
	// OnFrame, when set, is called for every captured frame instead of
	// writing it to Out. It exists for human-readable verification modes
	// (`juju-lens-probe --plain`) that want to inspect capture directly.
	OnFrame func(Frame) error
	// Logf receives human-readable progress on stderr. May be nil.
	Logf func(format string, args ...any)
}

// Run loads the eBPF programs, attaches them to every target PID's TLS
// boundary, and streams captured frames to cfg.Out until ctx is cancelled.
// It is Linux-only; see run_other.go for the portable stub.
func Run(ctx context.Context, cfg AttachConfig) error {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("removing memlock rlimit: %w", err)
	}

	// When explicit PIDs are given we attach to exactly those (a fixed set).
	// Otherwise we run in dynamic mode: attach to every jujud/containeragent
	// now and keep watching /proc so agents that start later (new units,
	// restarts) are picked up automatically and dead ones are detached.
	watch := len(cfg.PIDs) == 0

	spec := buildCollectionSpec()
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("loading eBPF collection: %w", err)
	}
	defer coll.Close()

	tracker := &pidTracker{coll: coll, jujuc: cfg.AttachJujuc, filter: cfg.Filter, logf: logf, attached: map[int][]link.Link{}}
	defer tracker.closeAll()

	if watch {
		found, err := EnumeratePIDs()
		if err != nil {
			return err
		}
		tracker.reconcile(found)
	} else {
		tracker.reconcile(cfg.PIDs)
		if tracker.count() == 0 {
			return errors.New("could not attach to any target process")
		}
	}

	rd, err := ringbuf.NewReader(coll.Maps["events"])
	if err != nil {
		return fmt.Errorf("opening ring buffer: %w", err)
	}
	defer rd.Close()

	// Unblock rd.Read on cancellation.
	go func() {
		<-ctx.Done()
		_ = rd.Close()
	}()

	// Dynamic mode: periodically re-scan for new/dead agent processes.
	if watch {
		go func() {
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					found, err := EnumeratePIDs()
					if err != nil {
						continue
					}
					tracker.reconcile(found)
				}
			}
		}()
	}

	// The ring-buffer drain loop below must stay as fast as possible: if it
	// ever blocks (e.g. cfg.Out is a pipe to a recorder that's busy indexing
	// a burst of spans to SQLite), the kernel-side eBPF program's
	// bpf_ringbuf_reserve starts failing and silently drops captured TLS
	// traffic — real charm activity, gone, with nothing anywhere to log it
	// (see emitInsns' "ring full: drop" path in bpf_linux.go). Decoupling the
	// drain from the (potentially slow) sink via a buffered channel means a
	// downstream stall only costs userspace-side queue drops, which — unlike
	// kernel ring drops — we can count and report (M13).
	frames := make(chan Frame, frameQueueCap)
	var queueDrops atomic.Uint64
	sinkDone := make(chan error, 1)
	go func() { sinkDone <- runSink(cfg, frames, &queueDrops, logf, dropStatsInterval) }()

	topoCache := map[int]Topology{}
	err = func() error {
		for {
			rec, err := rd.Read()
			if err != nil {
				if errors.Is(err, ringbuf.ErrClosed) || ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("reading ring buffer: %w", err)
			}
			f, ok := decodeEvent(rec.RawSample, topoCache)
			if !ok {
				continue
			}
			select {
			case frames <- f:
			default:
				// The sink can't keep up even with frameQueueCap already
				// buffered ahead of it. Drop here, in userspace, where the
				// loss is at least counted and reported — not by blocking,
				// which would just push the same backlog into the kernel
				// ring buffer and turn it into an uncounted drop there.
				queueDrops.Add(1)
			}
		}
	}()
	close(frames)
	if sinkErr := <-sinkDone; sinkErr != nil && err == nil {
		err = sinkErr
	}
	return err
}

// frameQueueCap buffers decoded frames between the ring-buffer drain loop and
// the sink goroutine (runSink), so a slow sink (a busy recorder on the other
// end of cfg.Out, or a slow OnFrame callback) can fall behind without stalling
// the drain loop itself. It's sized well above a single TLS-boundary burst
// (dozens of hooks firing across many units within the same second, e.g. at
// bootstrap) so that scenario is absorbed here rather than turning into
// kernel-side ring-buffer loss.
const frameQueueCap = 4096

// dropStatsInterval is how often queueDrops (frameQueueCap exhausted) is
// checked for a change and, if so, reported to the sink as a stats Frame and
// to Logf, so loss is visible promptly rather than only inferable after the
// fact from gaps in the recording.
const dropStatsInterval = 2 * time.Second

// runSink drains frames, delivering each to cfg.OnFrame or writing it to
// cfg.Out (through a buffered writer, to avoid a syscall per frame under
// load). It periodically reports queueDrops as a stats Frame (Frame.Drops) —
// the *cumulative* count, per Ingest's onDrops contract, not a per-interval
// delta — so a downstream consumer, and the recording itself, learns about
// userspace-side loss without polling for it. Pulled out of Run so the
// buffering/flushing/drop-reporting logic is unit-testable without a real
// eBPF ring buffer (M13). interval is normally dropStatsInterval; tests pass
// something much shorter so they don't have to wait on real wall-clock time.
func runSink(cfg AttachConfig, frames <-chan Frame, queueDrops *atomic.Uint64, logf func(string, ...any), interval time.Duration) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var out io.Writer = cfg.Out
	var bw *bufio.Writer
	if cfg.OnFrame == nil {
		bw = bufio.NewWriterSize(cfg.Out, 64*1024)
		out = bw
	}
	emit := func(f Frame) error {
		if cfg.OnFrame != nil {
			return cfg.OnFrame(f)
		}
		return WriteFrame(out, f)
	}
	flush := func() error {
		if bw == nil {
			return nil
		}
		return bw.Flush()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastReported uint64 // queueDrops is cumulative (Ingest's onDrops contract); only emit on change
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return flush()
			}
			if err := emit(f); err != nil {
				return fmt.Errorf("writing frame: %w", err)
			}
			// Once the queue drains (the burst that filled it is behind us),
			// flush promptly rather than leaving a live/--follow consumer
			// waiting on a buffer that's no longer filling.
			if len(frames) == 0 {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-t.C:
			n := queueDrops.Load()
			if n == lastReported {
				continue
			}
			lastReported = n
			logf("juju-lens-probe: %d frames dropped so far (sink falling behind); recording has a gap here", n)
			if err := emit(Frame{TsUnixNano: time.Now().UnixNano(), Drops: n}); err != nil {
				return fmt.Errorf("writing drop-stats frame: %w", err)
			}
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

// pidTracker owns the set of processes the probe is attached to and keeps it in
// sync with a desired PID set. Attaching a PID more than once is avoided; a PID
// that fails to attach is remembered (with a nil link slice) so we neither spam
// the log nor retry it every tick. When a tracked PID disappears its uprobes are
// closed. It is accessed from the reader goroutine's initial attach and the
// watcher goroutine, so it guards its map with a mutex.
type pidTracker struct {
	coll   *ebpf.Collection
	jujuc  bool
	filter TopoFilter
	logf   func(string, ...any)

	mu       sync.Mutex
	attached map[int][]link.Link // nil slice = attempted but failed/skip
}

// matches reports whether a process's topology passes the tracker's filter.
func (t *pidTracker) matches(topo Topology) bool {
	switch {
	case t.filter.ModelUUID != "":
		return topo.Model == t.filter.ModelUUID
	case t.filter.ControllerUUID != "":
		return topo.Controller == t.filter.ControllerUUID
	default:
		return true
	}
}

// reconcile attaches to any PID in want not already tracked, and detaches any
// tracked PID no longer in want.
func (t *pidTracker) reconcile(want []int) {
	wantSet := make(map[int]bool, len(want))
	for _, pid := range want {
		wantSet[pid] = true
	}
	for _, pid := range want {
		t.mu.Lock()
		_, known := t.attached[pid]
		t.mu.Unlock()
		if known {
			continue
		}
		// Resolve topology once, both to apply the filter and to label the log.
		topo := TopologyForPID(pid)
		if !t.matches(topo) {
			t.mu.Lock()
			t.attached[pid] = nil // remember the skip so we don't re-evaluate it
			t.mu.Unlock()
			continue
		}
		links, err := attachPID(t.coll, pid, t.jujuc)
		t.mu.Lock()
		t.attached[pid] = links // records nil on error so we don't retry it
		t.mu.Unlock()
		if err != nil {
			t.logf("juju-lens-probe: pid %d: %v", pid, err)
			continue
		}
		t.logf("juju-lens-probe: attached %d uprobes to pid %d (%s)", len(links), pid, describeTopo(topo))
	}
	// Detach processes that have gone away so their probes don't linger.
	t.mu.Lock()
	for pid, links := range t.attached {
		if wantSet[pid] {
			continue
		}
		for _, l := range links {
			_ = l.Close()
		}
		delete(t.attached, pid)
		if len(links) > 0 {
			t.logf("juju-lens-probe: detached pid %d (exited)", pid)
		}
	}
	t.mu.Unlock()
}

// count returns how many PIDs are currently attached with at least one uprobe.
func (t *pidTracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, links := range t.attached {
		if len(links) > 0 {
			n++
		}
	}
	return n
}

func (t *pidTracker) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, links := range t.attached {
		for _, l := range links {
			_ = l.Close()
		}
	}
	t.attached = map[int][]link.Link{}
}

// describeTopo renders a topology for the attach log line.
func describeTopo(t Topology) string {
	switch {
	case t.Unit != "":
		return t.Unit
	case t.Kind != "":
		return t.Kind
	default:
		return "?"
	}
}

func attachPID(coll *ebpf.Collection, pid int, withJujuc bool) ([]link.Link, error) {
	exe := fmt.Sprintf("/proc/%d/exe", pid)
	names := []string{SymTLSWrite, SymTLSRead}
	if withJujuc {
		names = append(names, SymJujucMain)
	}
	syms, missing, err := ResolveSymbols(exe, names)
	if err != nil {
		return nil, err
	}
	byName := map[string]uint64{}
	for _, s := range syms {
		byName[s.Name] = s.Entry
	}
	if _, ok := byName[SymTLSWrite]; !ok {
		return nil, fmt.Errorf("crypto/tls write symbol not found (%v)", missing)
	}

	ex, err := link.OpenExecutable(exe)
	if err != nil {
		return nil, err
	}
	var out []link.Link
	add := func(l link.Link, err error, what string) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, l)
		return nil
	}

	if addr, ok := byName[SymTLSWrite]; ok {
		l, e := ex.Uprobe("", coll.Programs["probe_write"], &link.UprobeOptions{PID: pid, Address: addr})
		if err := add(l, e, "write uprobe"); err != nil {
			return out, err
		}
	}
	if addr, ok := byName[SymTLSRead]; ok {
		// Entry uprobe stashes (conn, ptr) for the return side to pick up.
		l, e := ex.Uprobe("", coll.Programs["probe_read_in"], &link.UprobeOptions{PID: pid, Address: addr})
		if err := add(l, e, "read uprobe"); err != nil {
			return out, err
		}
		// Read's return is captured with ordinary uprobes at each RET site,
		// NOT a uretprobe: uretprobes patch the on-stack return address, which
		// crashes Go processes. At a RET the return value n is already in RAX,
		// so probe_read_ret reads it directly.
		rets, err := ResolveRETs(exe, SymTLSRead)
		if err != nil {
			return out, fmt.Errorf("locating read RET sites: %w", err)
		}
		for i, ret := range rets {
			l, e := ex.Uprobe("", coll.Programs["probe_read_ret"], &link.UprobeOptions{PID: pid, Address: ret})
			if err := add(l, e, fmt.Sprintf("read ret uprobe #%d", i)); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// decodeEvent parses a ring-buffer sample into a Frame. It returns ok=false for
// truncated samples. topoCache memoises the per-PID topology lookup (cmdline +
// agent.conf reads) so it happens once per process, not once per frame.
func decodeEvent(b []byte, topoCache map[int]Topology) (Frame, bool) {
	if len(b) < evData {
		return Frame{}, false
	}
	ts := binary.LittleEndian.Uint64(b[evTs:])
	pid := binary.LittleEndian.Uint32(b[evPID:])
	dir := binary.LittleEndian.Uint32(b[evDir:])
	conn := binary.LittleEndian.Uint64(b[evConn:])
	n := binary.LittleEndian.Uint32(b[evLen:])
	if int(evData+n) > len(b) {
		n = uint32(len(b) - evData)
	}
	data := make([]byte, n)
	copy(data, b[evData:evData+n])

	dirStr := "read"
	if dir == dirWrite {
		dirStr = "write"
	}
	topo, ok := topoCache[int(pid)]
	if !ok {
		topo = TopologyForPID(int(pid))
		topoCache[int(pid)] = topo
	}
	return Frame{
		TsUnixNano: bootNanoToWall(int64(ts)),
		PID:        int(pid),
		Dir:        dirStr,
		Conn:       conn,
		Controller: topo.Controller,
		Model:      topo.Model,
		App:        topo.App,
		Unit:       topo.Unit,
		Data:       data,
	}, true
}

// bootNanoToWall converts a bpf_ktime_get_ns() value (CLOCK_MONOTONIC
// nanoseconds since boot) into a wall-clock unix-nano timestamp using an offset
// sampled once at startup: wall = monotonic + (wallNow - monotonicNow).
// VISION §8.1 makes the eBPF clock canonical for RPC spans.
var bootOffsetNano = sampleBootOffset()

func sampleBootOffset() int64 {
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return 0 // degrade: treat bpf ktime as if it were wall-clock
	}
	return time.Now().UnixNano() - mono.Nano()
}

func bootNanoToWall(bootNano int64) int64 { return bootNano + bootOffsetNano }

// EnumeratePIDs scans /proc for jujud and containeragent processes.
func EnumeratePIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name == "jujud" || name == "containeragent" {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// UnitForPID best-effort parses a Juju unit name from a process's cmdline, e.g.
// "grafana/0". Returns "" for controller/machine agents with no unit.
func UnitForPID(pid int) string { return TopologyForPID(pid).Unit }

// Topology identifies the Juju entity a captured agent process belongs to,
// derived entirely from the host's view of the process: its cmdline (unit /
// machine / controller id, data-dir) and, where reachable, its agent.conf. The
// agent.conf is read through /proc/<pid>/root so it works even when the agent
// runs in a container (a k8s charm pod), whose filesystem the host can see
// there. Every field is best-effort; missing pieces are left blank.
type Topology struct {
	Controller string // controller UUID (from agent.conf)
	Model      string // model UUID (from agent.conf)
	App        string // application name (derived from the unit name)
	Unit       string // unit name, e.g. "grafana/0"
	Kind       string // "unit", "controller", "machine", or ""
}

// TopologyForPID resolves a process's Juju identity. It never errors: an
// unreadable cmdline or agent.conf just yields a sparser Topology.
//
// The agent tag is found two ways. Machine/controller jujud carry it on the
// cmdline (--machine-id/--controller-id); k8s unit containeragents do NOT put
// the unit name on their cmdline, so we fall back to listing the process's
// <data-dir>/agents directory (through /proc/<pid>/root, which sees into the
// container), where a unit agent has exactly one `unit-<app>-<n>` entry.
func TopologyForPID(pid int) Topology {
	var t Topology
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return t
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	val := func(i int) string {
		if i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	dataDir, tag := "", ""
	for i, a := range args {
		switch {
		case a == "--data-dir":
			dataDir = val(i)
		case strings.HasPrefix(a, "--data-dir="):
			dataDir = strings.TrimPrefix(a, "--data-dir=")
		case a == "--unit-name":
			tag = "unit-" + strings.Replace(val(i), "/", "-", 1)
		case strings.HasPrefix(a, "--unit-name="):
			tag = "unit-" + strings.Replace(strings.TrimPrefix(a, "--unit-name="), "/", "-", 1)
		case a == "--machine-id":
			tag = "machine-" + val(i)
		case a == "--controller-id":
			tag = "controller-" + val(i)
		}
	}
	if dataDir == "" {
		dataDir = "/var/lib/juju"
	}
	agentsDir := fmt.Sprintf("/proc/%d/root%s/agents", pid, dataDir)
	if tag == "" {
		tag = findAgentTag(agentsDir)
	}
	fillFromTag(&t, tag)
	if tag != "" {
		t.Controller, t.Model = parseAgentConf(agentsDir + "/" + tag + "/agent.conf")
	}
	return t
}

// findAgentTag inspects a data-dir/agents directory and returns the agent tag
// it holds, preferring a unit agent over a machine/controller one. Returns ""
// when the directory can't be read or holds no recognisable agent.
func findAgentTag(agentsDir string) string {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return ""
	}
	var machine, controller string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "unit-"):
			return name // a unit agent is the most specific; take it immediately
		case strings.HasPrefix(name, "machine-") && machine == "":
			machine = name
		case strings.HasPrefix(name, "controller-") && controller == "":
			controller = name
		}
	}
	if machine != "" {
		return machine
	}
	return controller
}

// fillFromTag populates Unit/App/Kind from an agent tag such as
// "unit-grafana-0" (-> grafana/0), "machine-3", or "controller-0".
func fillFromTag(t *Topology, tag string) {
	switch {
	case strings.HasPrefix(tag, "unit-"):
		rest := strings.TrimPrefix(tag, "unit-")
		t.Kind = "unit"
		if i := strings.LastIndexByte(rest, '-'); i >= 0 {
			t.App = rest[:i]
			t.Unit = rest[:i] + "/" + rest[i+1:]
		} else {
			t.App, t.Unit = rest, rest
		}
	case strings.HasPrefix(tag, "controller-"):
		t.Kind = "controller"
	case strings.HasPrefix(tag, "machine-"):
		t.Kind = "machine"
	}
}

// parseAgentConf extracts the controller and model UUIDs from an agent.conf.
// The file is YAML with top-level `controller: controller-<uuid>` and
// `model: model-<uuid>` keys; we line-scan rather than pull in a YAML
// dependency, tolerating either the tag-prefixed or bare-UUID form.
func parseAgentConf(path string) (controller, model string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "controller:"); ok {
			controller = strings.TrimPrefix(strings.TrimSpace(v), "controller-")
		} else if v, ok := strings.CutPrefix(line, "model:"); ok {
			model = strings.TrimPrefix(strings.TrimSpace(v), "model-")
		}
	}
	return controller, model
}
