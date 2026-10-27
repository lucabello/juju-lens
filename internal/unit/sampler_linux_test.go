//go:build linux

package unit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHost is a /proc and cgroup tree for the resource sampler: an agent,
// its children, and the agent's cgroup counters, all under a temp dir.
type fakeHost struct {
	t                   *testing.T
	proc, cgroup, spool string
	boot                time.Time
	recs                []Record
	res                 *sampler
	sp                  *spooler
}

const fakeAgent = 100

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	root := t.TempDir()
	h := &fakeHost{
		t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup"),
		spool: filepath.Join(root, "spool"), boot: time.Now().Add(-1000 * time.Second),
	}
	for _, d := range []string{h.proc, filepath.Join(h.cgroup, "agent"), h.spool} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.write(h.proc, "uptime", "1000.00 900.00\n")
	h.process(fakeAgent, 1, 1000, 0, nil, "containeragent\x00unit\x00")
	h.counters(0)

	cfg := Config{ProcRoot: h.proc, CgroupRoot: h.cgroup, AgentsDir: filepath.Join(root, "agents"), PodDir: filepath.Join(root, "pod")}
	cfg.defaults()
	ag, err := findAgent(h.proc)
	if err != nil {
		t.Fatal(err)
	}
	emit := func(r Record) error {
		h.recs = append(h.recs, r)
		return nil
	}
	h.res = newSampler(cfg, ag, emit)
	h.res.tapProcEnd = true
	h.sp, err = newSpooler(h.spool, h.proc, true, emit, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.sp.close)
	h.sp.res, h.res.flushTap = h.res, h.sp.flush
	return h
}

func (h *fakeHost) write(dir, name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// process writes /proc/<pid>: started at startTick, with cutime as its
// reaped children's user time (ticks), and the given environment.
func (h *fakeHost) process(pid, ppid int, startTick, cutime uint64, env []string, cmdline string) {
	h.t.Helper()
	dir := filepath.Join(h.proc, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	// Fields 3..24 after "pid (comm)": state ppid pgrp session tty tpgid
	// flags minflt cminflt majflt cmajflt utime stime cutime cstime
	// priority nice threads itrealvalue starttime vsize rss.
	h.write(dir, "stat", fmt.Sprintf("%d (proc) S %d %d %d 0 -1 0 0 0 0 0 5 2 %d 0 20 0 1 0 %d 1000 2560\n",
		pid, ppid, pid, pid, cutime, startTick))
	h.write(dir, "cmdline", cmdline)
	h.write(dir, "environ", strings.Join(env, "\x00"))
	h.write(dir, "cgroup", "0::/agent\n")
	h.write(dir, "status", "Uid:\t0\t0\t0\t0\n")
	h.cgroupProcs()
}

func (h *fakeHost) exit(pid int) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.proc, fmt.Sprint(pid))); err != nil {
		h.t.Fatal(err)
	}
	h.cgroupProcs()
}

// cgroupProcs lists the live processes in the agent cgroup's cgroup.procs.
func (h *fakeHost) cgroupProcs() {
	h.t.Helper()
	ents, _ := os.ReadDir(h.proc)
	var b strings.Builder
	for _, e := range ents {
		if e.IsDir() {
			b.WriteString(e.Name() + "\n")
		}
	}
	h.write(filepath.Join(h.cgroup, "agent"), "cgroup.procs", b.String())
}

// counters sets the agent cgroup's CPU to cpuUsec.
func (h *fakeHost) counters(cpuUsec int) {
	dir := filepath.Join(h.cgroup, "agent")
	h.write(dir, "cpu.stat", fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec 0\nthrottled_usec 0\n", cpuUsec, cpuUsec))
	h.write(dir, "memory.current", "1048576\n")
}

func (h *fakeHost) tick() {
	h.t.Helper()
	if err := h.res.tick(time.Now()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) kinds() []string {
	var out []string
	for _, r := range h.recs {
		out = append(out, r.Kind)
	}
	return out
}

func (h *fakeHost) executions() []*Execution {
	var out []*Execution
	for _, r := range h.recs {
		if r.Execution != nil {
			out = append(out, r.Execution)
		}
	}
	return out
}

var hookEnv = []string{"JUJU_UNIT_NAME=otelcol/0", "JUJU_DISPATCH_PATH=hooks/config-changed", "JUJU_HOOK_NAME=config-changed", "JUJU_SECRET=hunter2"}

func TestSamplerTakesBoundariesFromTheTap(t *testing.T) {
	h := newFakeHost(t)
	// A hook process the agent started 10 s ago, which the tap recorded.
	h.process(200, fakeAgent, 99000, 0, hookEnv, "python3\x00./src/charm.py\x00")
	started := h.boot.Add(990 * time.Second)
	t0, t1 := started.Add(80*time.Millisecond), started.Add(1500*time.Millisecond)
	h.write(h.spool, fmt.Sprintf("otelcol-0-200-%d.jsonl", t0.UnixNano()),
		fmt.Sprintf(`{"t":%d,"m":1,"k":"proc_start","pid":200,"ppid":100}`+"\n"+`{"t":%d,"m":2,"k":"proc_end","rc":0}`+"\n", t0.UnixNano(), t1.UnixNano()))
	if err := h.sp.scan(); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.kinds(); strings.Join(got, ",") != "tap,tap" {
		t.Fatalf("records while the hook runs: %v", got)
	}

	// It exits: the agent reaps it (1.2 s of its CPU) and the cgroup
	// counted 1.5 s.
	h.exit(200)
	h.process(fakeAgent, 1, 1000, 120, nil, "containeragent\x00unit\x00")
	h.counters(1_500_000)
	h.tick()

	if got := strings.Join(h.kinds(), ","); got != "tap,tap,execution,spool_end" {
		t.Fatalf("records: %s; want the execution before the spool_end", got)
	}
	e := h.executions()[0]
	if e.Source != SourceTap || e.TapFile == "" || e.Kind != ExecHook || e.Name != "config-changed" || e.Unit != "otelcol/0" {
		t.Errorf("execution %+v", e)
	}
	if !e.Start.Equal(t0) || !e.End.Equal(t1) || e.EndSlackMs != 0 || e.Truncated {
		t.Errorf("boundaries %v..%v slack %v truncated %v; want the tap's %v..%v", e.Start, e.End, e.EndSlackMs, e.Truncated, t0, t1)
	}
	if e.ProcStart.Sub(started).Abs() > time.Second {
		t.Errorf("proc start %v, want about %v", e.ProcStart, started)
	}
	if e.Tree.CPUUserMs != 1200 {
		t.Errorf("tree CPU %v ms, want 1200 from the agent's cutime", e.Tree.CPUUserMs)
	}
	if e.Cgroup.CPUMs != 1500 {
		t.Errorf("cgroup CPU %v ms, want 1500", e.Cgroup.CPUMs)
	}
	if _, ok := e.Env["secret"]; ok || e.Env["hook_name"] != "config-changed" {
		t.Errorf("env %v: want the tap's JUJU_* keys only", e.Env)
	}
}

func TestSamplerFindsWhatTheTapMisses(t *testing.T) {
	h := newFakeHost(t)
	h.tick()
	// `juju exec`: no Python, so no tap records. Next to it, the agent's
	// own helper, which has no Juju environment: not an execution.
	h.process(299, fakeAgent, 99490, 0, nil, "tmux\x00has-session\x00-t\x00otelcol/0\x00")
	h.process(300, fakeAgent, 99500, 0, []string{"JUJU_UNIT_NAME=otelcol/0", "JUJU_ACTION_NAME=juju-exec", "JUJU_CONTEXT_ID=otelcol/0-run-1"}, "/bin/bash\x00-s\x00")
	h.tick()
	h.exit(299)
	h.exit(300)
	h.tick()
	es := h.executions()
	if len(es) != 1 {
		t.Fatalf("executions: %v", h.kinds())
	}
	if e := es[0]; e.Source != SourceProc || e.TapFile != "" || e.Kind != ExecJuju || e.Name != "juju-exec" || e.EndSlackMs == 0 || !e.Start.Equal(e.ProcStart) || e.Overlapped {
		t.Errorf("execution %+v", e)
	}
}

func TestSamplerMarksOverlaps(t *testing.T) {
	h := newFakeHost(t)
	h.process(200, fakeAgent, 99000, 0, hookEnv, "python3\x00")
	h.process(201, fakeAgent, 99010, 0, []string{"JUJU_UNIT_NAME=ubuntu/0", "JUJU_HOOK_NAME=update-status"}, "python3\x00")
	h.tick()
	h.exit(200)
	h.exit(201)
	h.process(fakeAgent, 1, 1000, 300, nil, "containeragent\x00unit\x00")
	h.tick()
	es := h.executions()
	if len(es) != 2 {
		t.Fatalf("executions: %v", h.kinds())
	}
	for _, e := range es {
		if !e.Overlapped || e.Tree.CPUUserMs != 0 {
			t.Errorf("%s: overlapped %v, tree CPU %v; want overlapped and no tree CPU (it can't be split)", e.Name, e.Overlapped, e.Tree.CPUUserMs)
		}
	}
}

// The workloads are sampled in bursts while a Pebble change a hook caused
// isn't ready, and for the linger time after it is. A change no hook caused,
// with the window closed, doesn't open it.
func TestBurstWindowLingersAfterTheHooksChanges(t *testing.T) {
	s := &sampler{
		linger: 30 * time.Second, pending: map[string]time.Time{},
		workloads: map[string]*container{"otelcol": {}}, runs: map[int]*activeRun{},
	}
	t0 := time.Now()
	s.noteChange(PebbleChange{Container: "otelcol", ID: "1", Ready: true, ReadyTime: t0}, false, t0)
	if s.burstOpen(t0) {
		t.Fatal("a change no hook caused opened the window")
	}
	// The hook ended with its replan still running.
	s.noteChange(PebbleChange{Container: "otelcol", ID: "2"}, true, t0)
	if !s.burstOpen(t0.Add(time.Minute)) {
		t.Fatal("window closed while the hook's change isn't ready")
	}
	ready := t0.Add(2 * time.Second)
	s.noteChange(PebbleChange{Container: "otelcol", ID: "2", Ready: true, ReadyTime: ready}, false, ready)
	if !s.burstOpen(ready.Add(29*time.Second)) || s.burstOpen(ready.Add(31*time.Second)) {
		t.Errorf("window should close 30 s after the change was ready, at %v", ready.Add(30*time.Second))
	}
}
