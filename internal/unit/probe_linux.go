//go:build linux

package unit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config tunes a probe run. Only Out is required.
type Config struct {
	Version string
	Out     io.Writer
	Logf    func(format string, args ...any)

	// TapFeatures are the tap features hook processes run; nil means
	// DefaultFeatures.
	TapFeatures []string

	// IdleHz is how often /proc is polled for executions the tap doesn't
	// see while none runs; RunHz how often running ones are sampled. They
	// default to 10 and 20: at 50, the probe used 28 ms of CPU per second
	// of hook on otelcol and other COS charms, at 20, 18.
	IdleHz, RunHz int
	// MaxProcs caps the processes listed per Execution; default 200.
	MaxProcs int
	// Linger is how long after the last Pebble change a hook caused is
	// ready the workloads are still sampled every BurstEvery; 0 stops at
	// the hook's end. The probe's command line defaults it to
	// DefaultLinger.
	Linger time.Duration
	// PebbleLogs makes the probe follow the service logs of every workload
	// container through its Pebble, and write each line as a KindPebbleLog.
	PebbleLogs bool

	// The directories below default to the real ones; tests point them at
	// fixtures.
	ProcRoot   string // /proc
	AgentsDir  string // /var/lib/juju/agents
	TmpDir     string // /tmp: the ownership lock, uploaded probe binaries, the tap spool
	CgroupRoot string // /sys/fs/cgroup
	PodDir     string // /charm/containers: the workload containers' Pebble sockets
}

func (c *Config) defaults() {
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	if c.AgentsDir == "" {
		c.AgentsDir = "/var/lib/juju/agents"
	}
	if c.TmpDir == "" {
		c.TmpDir = os.TempDir()
	}
	if c.CgroupRoot == "" {
		c.CgroupRoot = "/sys/fs/cgroup"
	}
	if c.PodDir == "" {
		c.PodDir = "/charm/containers"
	}
	if c.TapFeatures == nil {
		c.TapFeatures = DefaultFeatures
	}
	if c.IdleHz <= 0 {
		c.IdleHz = 10
	}
	if c.RunHz <= 0 {
		c.RunHz = 20
	}
	if c.MaxProcs <= 0 {
		c.MaxProcs = 200
	}
}

// UploadPrefix starts the file name the recorder uploads the probe binary
// under in TmpDir.
const UploadPrefix = "juju-lens-probe-"

// leftoverAge is how long an upload can go unmodified before Run treats it
// as left behind by a session that died mid-upload or before its probe
// deleted itself.
const leftoverAge = 2 * time.Minute

// Run is the probe's unit mode. It takes ownership of the host, finds the
// Juju agent, writes a hello, and installs the tap in every charm venv. Then
// it keeps the tap installed and its heartbeat fresh, streams the tap's
// records, measures each execution's resource usage and writes a
// resources snapshot every ResourcesEvery, and writes a heartbeat record
// every HeartbeatEvery, until ctx ends or a write fails (the recorder went
// away). It removes the tap and returns nil in both cases, and after
// writing a KindBusy record when another recording owns the host.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	bw := bufio.NewWriterSize(cfg.Out, 64<<10)
	enc := json.NewEncoder(bw)
	emit := func(r Record) error {
		r.TS = time.Now().UTC()
		return enc.Encode(r)
	}
	write := func(r Record) error {
		if err := emit(r); err != nil {
			return err
		}
		return bw.Flush()
	}

	removeLeftovers(cfg.TmpDir, cfg.ProcRoot, cfg.Logf)
	own, err := takeOwnership(filepath.Join(cfg.TmpDir, lockName), StaleAfter, cfg.Logf)
	var busy *busyError
	if errors.As(err, &busy) {
		cfg.Logf("unit probe: %v", busy)
		return write(Record{Kind: KindBusy, Busy: &Busy{PID: busy.pid, HeartbeatAgeMs: busy.age.Milliseconds()}})
	}
	if err != nil {
		return err
	}
	defer own.release()

	var logs *logStreamer
	if cfg.PebbleLogs {
		logs = newLogStreamer(ctx, cfg.Logf)
	}
	var ag agent
	for {
		if ag, err = findAgent(cfg.ProcRoot); err == nil {
			break
		}
		cfg.Logf("unit probe: %v; retrying", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		own.touch()
	}
	res := newSampler(cfg, ag, emit)
	defer res.close()
	var logCh <-chan PebbleLog
	if logs != nil {
		logCh = logs.ch
		res.onWorkload = func(name, socket string) { logs.add(name, socket) }
		for _, name := range res.workloadNames() {
			logs.add(name, filepath.Join(res.podDir, name, "pebble.socket"))
		}
	}
	res.tapProcEnd = slices.Contains(cfg.TapFeatures, "proc")
	hello := func(ag agent) *Hello {
		h := &Hello{
			Version: cfg.Version, PID: os.Getpid(), Substrate: ag.substrate,
			Agent: ag.name, AgentPID: ag.pid, AgentStart: ag.start,
			Units: localUnits(cfg.AgentsDir), BootTime: bootTime(cfg.ProcRoot),
		}
		res.hello(h)
		return h
	}
	res.rehello = func(ag agent) error { return emit(Record{Kind: KindHello, Hello: hello(ag)}) }
	if err := write(Record{Kind: KindHello, Hello: hello(ag)}); err != nil {
		return nil
	}
	own.touch()
	if res.resources() != nil || bw.Flush() != nil {
		return nil
	}

	report := func(events []Loader) error {
		for i := range events {
			if err := write(Record{Kind: KindLoader, Loader: &events[i]}); err != nil {
				return err
			}
		}
		return nil
	}
	tap := newTapLoader(cfg.AgentsDir, cfg.TmpDir, ag.uid, cfg.TapFeatures, cfg.Logf)
	var sp *spooler
	var spoolCh chan spoolEvents
	if err := tap.setup(); err != nil {
		cfg.Logf("unit probe: not installing the tap: %v", err)
		if report([]Loader{{Event: LoaderUnavailable, Reason: err.Error()}}) != nil {
			return nil
		}
		tap = nil
	} else {
		// Removal is written if the recorder is still there to read it.
		defer func() { _ = report(tap.remove()) }()
		// The watch starts before the first scan, so a line written in
		// between is seen by one or both, and offsets keep it from being
		// forwarded twice.
		if sp, err = newSpooler(tap.spool, cfg.ProcRoot, slices.Contains(cfg.TapFeatures, FeatureProc), emit, cfg.Logf); err != nil {
			cfg.Logf("unit probe: not streaming the spool: %v", err)
		} else {
			defer sp.close()
			sp.res, res.flushTap = res, sp.flush
			spoolCh = make(chan spoolEvents, 16)
			go sp.watch(spoolCh)
			if sp.scan() != nil || bw.Flush() != nil {
				return nil
			}
		}
		if report(tap.ensure()) != nil {
			return nil
		}
	}

	// The tap's heartbeat and installation are kept every tapEvery, so a
	// venv a refresh replaced gets the tap back within about that time.
	t := time.NewTicker(tapEvery)
	defer t.Stop()
	lastBeat := time.Now()
	poll := time.NewTimer(res.interval())
	defer poll.Stop()
	nextRes := time.Now().Add(ResourcesEvery)
	for {
		select {
		case <-ctx.Done():
			// What hooks wrote up to now still reaches the recorder.
			if sp != nil && sp.scan() == nil {
				_ = bw.Flush()
			}
			return nil
		case l := <-logCh:
			// A burst of lines goes out together.
			batch := []PebbleLog{l}
		drain:
			for len(batch) < logBatch {
				select {
				case l := <-logCh:
					batch = append(batch, l)
				default:
					break drain
				}
			}
			for i := range batch {
				if emit(Record{Kind: KindPebbleLog, PebbleLog: &batch[i]}) != nil {
					return nil
				}
			}
			if bw.Flush() != nil {
				cfg.Logf("unit probe: recorder gone; stopping")
				return nil
			}
		case ev, ok := <-spoolCh:
			if !ok {
				spoolCh = nil // the watch failed; reap still finds ended hooks
				continue
			}
			if sp.handle(ev) != nil || bw.Flush() != nil {
				cfg.Logf("unit probe: recorder gone; stopping")
				return nil
			}
		case now := <-poll.C:
			err := res.tick(now)
			if err == nil && !now.Before(nextRes) {
				nextRes = now.Add(ResourcesEvery)
				err = res.resources()
			}
			if err != nil || bw.Flush() != nil {
				cfg.Logf("unit probe: recorder gone; stopping")
				return nil
			}
			poll.Reset(res.interval())
		case now := <-t.C:
			if tap != nil {
				if err := tap.heartbeat(); err != nil {
					cfg.Logf("unit probe: tap heartbeat: %v", err)
				}
				if report(tap.ensure()) != nil {
					cfg.Logf("unit probe: recorder gone; stopping")
					return nil
				}
			}
			if sp != nil {
				if sp.reap() != nil || bw.Flush() != nil {
					cfg.Logf("unit probe: recorder gone; stopping")
					return nil
				}
			}
			if now.Sub(lastBeat) < HeartbeatEvery {
				continue
			}
			lastBeat = now
			if err := write(Record{Kind: KindHeartbeat}); err != nil {
				cfg.Logf("unit probe: recorder gone (%v); stopping", err)
				return nil
			}
			own.touch()
		}
	}
}

// logBatch is how many workload log lines the main loop writes before it
// flushes.
const logBatch = 256

// tapEvery is how often the probe refreshes the tap's heartbeat and checks
// its installation.
const tapEvery = time.Second

// agent is the Juju agent whose children are charm executions.
type agent struct {
	pid       int
	name      string
	substrate string
	uid       int       // effective uid; hook processes run as it
	start     time.Time // when the process started
}

// findAgent locates the containeragent of a Kubernetes charm container, or
// the machine agent.
func findAgent(procRoot string) (agent, error) {
	for _, pid := range listPIDs(procRoot) {
		args := cmdlineArgs(procRoot, pid)
		if len(args) < 2 {
			continue
		}
		var ag agent
		switch base := filepath.Base(args[0]); {
		case base == "containeragent" && args[1] == "unit":
			ag = agent{pid: pid, name: base, substrate: "kubernetes"}
		case base == "jujud" && args[1] == "machine":
			ag = agent{pid: pid, name: base, substrate: "machine"}
		default:
			continue
		}
		uid, ok := procEUID(procRoot, pid)
		if !ok {
			continue // gone since listed
		}
		ag.uid, ag.start = uid, procStart(procRoot, pid)
		return ag, nil
	}
	return agent{}, errors.New("no Juju agent (containeragent unit / jujud machine) found in /proc")
}

// procEUID reads a process's effective uid from /proc/<pid>/status.
func procEUID(procRoot string, pid int) (int, bool) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/status", procRoot, pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) < 2 {
				return 0, false
			}
			uid, err := strconv.Atoi(f[1])
			return uid, err == nil
		}
	}
	return 0, false
}

// procStart is when a process started, from /proc/<pid>/stat and the boot
// time. Zero if unreadable.
func procStart(procRoot string, pid int) time.Time {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procRoot, pid))
	if err != nil {
		return time.Time{}
	}
	// The command name can hold spaces and parentheses; fields follow the
	// last ')'. starttime is field 22, the 20th after it.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return time.Time{}
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	boot := bootTime(procRoot)
	if err != nil || boot.IsZero() {
		return time.Time{}
	}
	// USER_HZ is 100 on every Linux architecture Juju runs on.
	return boot.Add(time.Duration(ticks) * (time.Second / 100)).UTC()
}

// removeLeftovers deletes uploaded probe binaries that no session is writing
// or running any more: an upload cut off mid-way, or one whose probe never
// started. A running probe deleted its own file at start, and an upload in
// progress keeps its mtime fresh.
func removeLeftovers(tmpDir, procRoot string, logf func(string, ...any)) {
	paths, _ := filepath.Glob(filepath.Join(tmpDir, UploadPrefix+"*"))
	if len(paths) == 0 {
		return
	}
	running := map[string]bool{}
	for _, pid := range listPIDs(procRoot) {
		if exe, err := os.Readlink(fmt.Sprintf("%s/%d/exe", procRoot, pid)); err == nil {
			running[exe] = true
		}
	}
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || running[p] || time.Since(st.ModTime()) < leftoverAge {
			continue
		}
		if os.Remove(p) == nil {
			logf("unit probe: removed leftover upload %s", p)
		}
	}
}

func listPIDs(procRoot string) []int {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(ents))
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

func cmdlineArgs(procRoot string, pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/cmdline", procRoot, pid))
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

// localUnits lists the units whose agent directories are on this host.
func localUnits(agentsDir string) []string {
	ents, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if u := unitFromTag(e.Name()); u != "" {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// unitFromTag turns an agent directory name ("unit-grafana-k8s-0") into a
// unit name ("grafana-k8s/0").
func unitFromTag(tag string) string {
	rest, ok := strings.CutPrefix(tag, "unit-")
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 || i == len(rest)-1 {
		return ""
	}
	if _, err := strconv.Atoi(rest[i+1:]); err != nil {
		return ""
	}
	return rest[:i] + "/" + rest[i+1:]
}

// processStart is when the probe started, taken as early as Go allows.
var processStart = time.Now()

// bootTime is the time the start times in /proc/<pid>/stat count from. It
// comes from the probe's own start, in both clocks: inside an LXD container,
// lxcfs makes /proc/uptime the container's uptime, while start times count
// from the kernel's boot, days earlier. /proc/uptime is the fallback.
func bootTime(procRoot string) time.Time {
	if b, err := os.ReadFile(procRoot + "/self/stat"); err == nil {
		if st, err := parseProcStat(b); err == nil && st.start > 0 {
			return processStart.Add(-time.Duration(st.start) * time.Second / clkTck).UTC()
		}
	}
	b, err := os.ReadFile(procRoot + "/uptime")
	if err != nil {
		return time.Time{}
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return time.Time{}
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(secs * float64(time.Second))).UTC()
}
