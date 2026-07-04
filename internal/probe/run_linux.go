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

	topoCache := map[int]Topology{}
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
		if cfg.OnFrame != nil {
			if err := cfg.OnFrame(f); err != nil {
				return err
			}
			continue
		}
		if err := WriteFrame(cfg.Out, f); err != nil {
			return fmt.Errorf("writing frame: %w", err)
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
