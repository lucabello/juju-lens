//go:build linux

package unit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The resource sampler measures what each execution costs (see
// resources.go). It runs in the probe's goroutine: Run calls tick on its
// timer, and the spooler calls tapStarted, tapEnded and tapFinished as the
// tap's records go by, so nothing here needs a lock.

// container is one cgroup the sampler reads: the agent's own (local files),
// or a workload container's (through its Pebble socket: the cgroup stream,
// see cgstream.go, or the file API where the stream can't run).
type container struct {
	name   string
	dir    string
	read   filesReader
	pebble *pebbleClient // nil for the agent's cgroup

	seenChanges map[string]string // change id -> last status written
	// stream is the cgroup shell, nil while it isn't running; it is
	// (re)started at the first reading after nextStream. own are the
	// changes of its execs, which aren't the charm's doing.
	stream     *cgroupStream
	nextStream time.Time
	streamErr  string
	own        map[string]bool
}

// streamRetry is how long a workload's readings go through the file API
// after its cgroup stream failed to start or broke.
const streamRetry = time.Minute

// counters reads the cgroup's counters: a workload's through its stream,
// or the file API while the stream isn't running.
func (c *container) counters(logf func(string, ...any)) Counters {
	read := c.read
	if c.pebble != nil && c.streaming(logf) {
		read = c.stream.readFiles
	}
	cnt, err := readCounters(read, c.dir)
	if err != nil && c.stream != nil {
		logf("unit probe: %s: cgroup stream: %v; reading through the file API", c.name, err)
		c.stopStream()
		c.nextStream = time.Now().Add(streamRetry)
		cnt, err = readCounters(c.read, c.dir)
	}
	cnt.Container = c.name
	if err != nil {
		cnt.Error = err.Error()
	}
	return cnt
}

// streaming reports whether a workload's cgroup stream runs, starting it
// if it is time to try.
func (c *container) streaming(logf func(string, ...any)) bool {
	if c.stream != nil {
		return true
	}
	if time.Now().Before(c.nextStream) {
		return false
	}
	st, err := startCgroupStream(c.pebble, c.own)
	if err != nil {
		c.nextStream = time.Now().Add(streamRetry)
		if msg := err.Error(); msg != c.streamErr {
			c.streamErr = msg
			logf("unit probe: %s: no cgroup stream (%v): reading through the file API, without bursts", c.name, err)
		}
		return false
	}
	c.stream, c.streamErr = st, ""
	return true
}

func (c *container) stopStream() {
	if c.stream != nil {
		c.stream.close()
		c.stream = nil
	}
}

func (c *container) memCurrent() (uint64, error) {
	files, err := c.read(c.dir, []string{"memory.current"})
	b, ok := files["memory.current"]
	if !ok {
		if err == nil {
			err = fmt.Errorf("%s: not readable", filepath.Join(c.dir, "memory.current"))
		}
		return 0, err
	}
	return parseUint(b), nil
}

// activeRun is an execution in flight.
type activeRun struct {
	exec      Execution
	startTick uint64 // /proc start time, to tell a reused pid
	agent0    pstat
	self0     [2]float64 // probe user, sys CPU ms at start
	cg0       Counters
	cg0At     time.Time
	work0     map[string]Counters
	work0At   time.Time
	memPeak   map[string]uint64
	memN      map[string]int
	procs     map[int]*Proc
	order     []int
	procEnd   time.Time // the tap's proc_end, once it went by
	// peak is the agent cgroup's memory.peak, reset as the execution
	// started; nil where it can't be reset.
	peak *os.File
}

type sampler struct {
	procRoot, podDir, agentsDir string
	cgroupRoot                  string
	idleHz, runHz, maxProcs     int
	// tapProcEnd: the tap writes proc_end, so a tapped process without
	// one was killed.
	tapProcEnd bool
	emit       func(Record) error
	logf       func(string, ...any)
	// rehello writes a new hello after the agent restarted.
	rehello func(agent) error
	// flushTap forwards what is left of a spool file and finishes it, which
	// calls tapFinished. Run sets it while it streams the spool.
	flushTap func(file string) error

	self int
	boot time.Time
	page uint64

	agent        agent
	agentTick    uint64 // the agent's /proc start time
	cg           *container
	probeInGroup bool
	workloads    map[string]*container
	// onWorkload, when set, is called for each workload container found
	// after the sampler was made, with its Pebble socket.
	onWorkload func(name, socket string)

	runs map[int]*activeRun
	// ignored are the agent's children that aren't executions, until they
	// exit or exec something else.
	ignored map[int]ignoredProc

	// idleProcs is the agent cgroup's process list at the last full scan
	// that found nothing running. An idle tick only compares it: reading
	// the cgroup's counters every tick was most of the probe's idle CPU.
	idleProcs []byte
	ticks     uint64

	// The burst window: while it is open, the workload containers'
	// counters are written every BurstEvery. It is open while an execution
	// runs, while a Pebble change one caused isn't ready (for at most
	// maxPending), and until burstUntil: linger after the
	// last such change was ready.
	linger      time.Duration
	burstUntil  time.Time
	nextBurst   time.Time
	pending     map[string]time.Time // container/change id -> when first seen not ready
	nextChanges time.Time
	// peakReset: the agent cgroup's memory.peak can be reset (Linux 6.12
	// and later, on a cgroup filesystem the probe can write).
	peakReset bool
}

type ignoredProc struct {
	start uint64
	comm  string
}

func newSampler(cfg Config, ag agent, emit func(Record) error) *sampler {
	s := &sampler{
		procRoot: cfg.ProcRoot, podDir: cfg.PodDir, agentsDir: cfg.AgentsDir, cgroupRoot: cfg.CgroupRoot,
		idleHz: cfg.IdleHz, runHz: cfg.RunHz, maxProcs: cfg.MaxProcs, linger: cfg.Linger,
		emit: emit, logf: cfg.Logf,
		self: os.Getpid(), boot: bootTime(cfg.ProcRoot), page: uint64(os.Getpagesize()),
		workloads: map[string]*container{}, runs: map[int]*activeRun{}, ignored: map[int]ignoredProc{},
		pending: map[string]time.Time{},
	}
	s.setAgent(ag)
	s.refreshWorkloads(true)
	return s
}

func (s *sampler) setAgent(ag agent) {
	s.agent = ag
	if st, ok := s.readStat(ag.pid); ok {
		s.agentTick = st.start
	}
	dir := s.cgroupDirOf(ag.pid)
	name := "charm"
	if ag.substrate == "machine" {
		name = "agent"
	}
	s.cg = &container{name: name, dir: dir, read: readLocalFiles}
	s.probeInGroup = s.cgroupDirOf(s.self) == dir
	s.peakReset = false
	if f, err := resetPeak(dir); err == nil {
		_ = f.Close()
		s.peakReset = true
	}
	s.idleProcs = nil
}

// hello fills the sampler's part of a hello.
func (s *sampler) hello(h *Hello) {
	h.Cgroup, h.Workloads = s.cg.dir, s.workloadNames()
	h.IdleHz, h.RunHz, h.ProbeInCgroup = s.idleHz, s.runHz, s.probeInGroup
	h.LingerMs, h.MemPeakReset = s.linger.Milliseconds(), s.peakReset
}

// interval is how long until the next tick: short while an execution runs,
// and at most BurstEvery while the burst window is open.
func (s *sampler) interval() time.Duration {
	d := time.Second / time.Duration(s.idleHz)
	if len(s.runs) > 0 {
		d = time.Second / time.Duration(s.runHz)
	}
	if s.burstOpen(time.Now()) {
		d = min(d, BurstEvery)
	}
	return d
}

// refreshWorkloads (re)discovers the workload containers' Pebble sockets.
func (s *sampler) refreshWorkloads(initial bool) {
	if s.podDir == "" {
		return
	}
	socks, _ := filepath.Glob(filepath.Join(s.podDir, "*", "pebble.socket"))
	for _, sock := range socks {
		name := filepath.Base(filepath.Dir(sock))
		if _, ok := s.workloads[name]; ok {
			continue
		}
		pc := newPebbleClient(sock)
		// Pebble runs as the container's PID 1, so the cgroup files it
		// serves are the workload container's own.
		c := &container{
			name: name, dir: "/sys/fs/cgroup", read: pc.readFiles, pebble: pc,
			seenChanges: map[string]string{}, own: map[string]bool{},
		}
		// Changes that predate the probe are history, not activity.
		if chs, err := pc.changes(); err == nil {
			for _, ch := range chs {
				c.seenChanges[ch.ID] = ch.Status
			}
		}
		s.workloads[name] = c
		if !initial {
			s.logf("unit probe: new workload container %s", name)
			if s.onWorkload != nil {
				s.onWorkload(name, sock)
			}
		}
	}
}

// close stops the workloads' cgroup streams.
func (s *sampler) close() {
	for _, c := range s.workloads {
		c.stopStream()
	}
}

func (s *sampler) workloadNames() []string {
	names := make([]string, 0, len(s.workloads))
	for n := range s.workloads {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resources writes a snapshot of every cgroup's counters, the disks, and
// the probe's own usage, then any new Pebble changes.
func (s *sampler) resources() error {
	s.refreshWorkloads(false)
	res := &Resources{Containers: []Counters{s.cg.counters(s.logf)}}
	for _, n := range s.workloadNames() {
		res.Containers = append(res.Containers, s.workloads[n].counters(s.logf))
	}
	res.Disks = s.disks()
	u, sy := selfCPU()
	res.ProbeCPUMs = u + sy
	if st, ok := s.readStat(s.self); ok {
		res.ProbeRSSBytes = uint64(max(st.rss, 0)) * s.page
	}
	if err := s.emit(Record{Kind: KindResources, Resources: res}); err != nil {
		return err
	}
	return s.changes(false)
}

// changes writes the workload Pebble changes that are new or changed
// status, except the probe's own. caused says an execution just ended, so
// they are its doing; so are the ones that come by while the burst window
// is open.
func (s *sampler) changes(caused bool) error {
	for _, n := range s.workloadNames() {
		c := s.workloads[n]
		chs, err := c.pebble.changes()
		if err != nil {
			continue
		}
		now := time.Now()
		for _, ch := range chs {
			if c.seenChanges[ch.ID] == ch.Status || c.own[ch.ID] {
				continue
			}
			c.seenChanges[ch.ID] = ch.Status
			ch.Container = n
			if err := s.emit(Record{Kind: KindPebbleChange, PebbleChange: &ch}); err != nil {
				return err
			}
			s.noteChange(ch, caused, now)
		}
	}
	return nil
}

// tick is one /proc poll: it finds executions the tap doesn't see, notices
// the end of every execution, and samples the running ones. While nothing
// runs it first compares the agent cgroup's process list, which only
// changes when a process starts or ends, so an idle tick costs one small
// file read instead of a walk over /proc.
func (s *sampler) tick(now time.Time) error {
	s.ticks++
	if err := s.burst(now); err != nil {
		return err
	}
	if len(s.runs) == 0 && s.idleProcs != nil {
		if b, err := os.ReadFile(filepath.Join(s.cg.dir, "cgroup.procs")); err == nil && bytes.Equal(b, s.idleProcs) {
			return nil
		}
	}

	stats := map[int]pstat{}
	children := map[int][]int{}
	for _, pid := range listPIDs(s.procRoot) {
		if pid == s.self {
			continue
		}
		st, ok := s.readStat(pid)
		if !ok {
			continue
		}
		stats[pid] = st
		children[st.ppid] = append(children[st.ppid], pid)
	}
	agentStat, ok := stats[s.agent.pid]
	if !ok || agentStat.start != s.agentTick {
		return s.agentGone(now)
	}

	// Finish runs whose process is gone (or whose pid was reused).
	for pid, ar := range s.runs {
		if st, ok := stats[pid]; !ok || st.ppid != s.agent.pid || st.start != ar.startTick {
			if err := s.ended(pid, ar, now, agentStat); err != nil {
				return err
			}
		}
	}
	// Start runs for the agent's new children: the ones the tap didn't
	// report first. A child with no Juju execution environment is
	// skipped until it execs something else.
	for pid, ig := range s.ignored {
		if st, ok := stats[pid]; !ok || st.start != ig.start {
			delete(s.ignored, pid)
		}
	}
	for _, pid := range children[s.agent.pid] {
		st := stats[pid]
		if _, ok := s.runs[pid]; ok {
			continue
		}
		if ig, ok := s.ignored[pid]; ok && ig.comm == st.comm {
			continue
		}
		if s.start(pid, st, agentStat, now, SourceProc) == nil {
			s.ignored[pid] = ignoredProc{start: st.start, comm: st.comm}
		}
	}
	for pid, ar := range s.runs {
		s.sample(ar, pid, stats, children, now)
	}
	if len(s.runs) == 0 {
		if b, err := os.ReadFile(filepath.Join(s.cg.dir, "cgroup.procs")); err == nil {
			s.idleProcs = b
		}
	}
	return nil
}

// agentGone finishes what the agent was running and finds it again.
func (s *sampler) agentGone(now time.Time) error {
	for pid, ar := range s.runs {
		if err := s.ended(pid, ar, now, pstat{}); err != nil {
			return err
		}
	}
	ag, err := findAgent(s.procRoot)
	if err != nil {
		s.logf("unit probe: agent gone: %v", err)
		return nil
	}
	s.setAgent(ag)
	if s.rehello == nil {
		return nil
	}
	return s.rehello(ag)
}

// ended finishes a run whose process the poll found gone. A process the tap
// recorded is finished through the spooler, so its last records are read
// first and its execution precedes its spool_end.
func (s *sampler) ended(pid int, ar *activeRun, now time.Time, agentStat pstat) error {
	if ar.exec.TapFile != "" && s.flushTap != nil {
		if err := s.flushTap(ar.exec.TapFile); err != nil {
			return err
		}
		if s.runs[pid] != ar {
			return nil
		}
	}
	return s.finish(pid, now, agentStat)
}

// tapStarted is called when a spool file's proc_start goes by: the tap's
// start of a process. A child of the agent starts a run, unless the poll
// already did; a process another one started is part of that one's tree.
func (s *sampler) tapStarted(pid int, file string, t time.Time) {
	if ar := s.runs[pid]; ar != nil {
		if ar.exec.TapFile == "" {
			ar.exec.Source, ar.exec.TapFile, ar.exec.Start = SourceTap, file, t
		}
		return
	}
	st, ok := s.readStat(pid)
	if !ok || st.ppid != s.agent.pid {
		return
	}
	agentStat, _ := s.readStat(s.agent.pid)
	if ar := s.start(pid, st, agentStat, time.Now(), SourceTap); ar != nil {
		ar.exec.TapFile, ar.exec.Start = file, t
	}
}

// tapEnded is called when a spool file's proc_end goes by.
func (s *sampler) tapEnded(pid int, file string, t time.Time) {
	if ar := s.runs[pid]; ar != nil && ar.exec.TapFile == file {
		ar.procEnd = t
	}
}

// tapFinished is called as the spooler finishes a file, before its
// spool_end: the process has exited and been reaped.
func (s *sampler) tapFinished(pid int, file string) error {
	ar := s.runs[pid]
	if ar == nil || ar.exec.TapFile != file {
		return nil
	}
	agentStat, _ := s.readStat(s.agent.pid)
	return s.finish(pid, time.Now(), agentStat)
}

// start begins measuring a child of the agent, unless it has no Juju
// execution environment: then it returns nil.
func (s *sampler) start(pid int, st, agentStat pstat, now time.Time, source string) *activeRun {
	env := s.execEnv(pid)
	kind, name := execKind(env)
	if kind == "" {
		return nil
	}
	delete(s.ignored, pid)
	procStart := s.boot.Add(time.Duration(st.start) * time.Second / clkTck).UTC()
	e := Execution{
		Unit: env["unit_name"], Kind: kind, Name: name, Env: env, PID: pid,
		Command: truncate(strings.Join(cmdlineArgs(s.procRoot, pid), " "), 512),
		Source:  source, ProcStart: procStart, Start: procStart,
		LingerMs: s.linger.Milliseconds(),
	}
	if e.Unit == "" {
		if us := localUnits(s.agentsDir); len(us) == 1 {
			e.Unit = us[0]
		}
	}
	ar := &activeRun{
		exec: e, startTick: st.start, agent0: agentStat, self0: selfCPUPair(),
		memPeak: map[string]uint64{}, memN: map[string]int{}, procs: map[int]*Proc{},
	}
	// The cgroup baseline is read when the probe notices the process: up to
	// an idle poll (100 ms) after it started. The tree's CPU, from the
	// agent's counters, is exact regardless. So is the cgroup's peak where
	// memory.peak can be reset: from here on.
	if s.peakReset {
		if f, err := resetPeak(s.cg.dir); err == nil {
			ar.peak = f
		}
	}
	ar.cg0, ar.cg0At = s.cg.counters(s.logf), now
	ar.work0, ar.work0At = map[string]Counters{}, time.Now()
	for _, n := range s.workloadNames() {
		ar.work0[n] = s.workloads[n].counters(s.logf)
	}
	if len(s.runs) > 0 {
		ar.exec.Overlapped = true
		for _, o := range s.runs {
			o.exec.Overlapped = true
		}
	}
	s.runs[pid] = ar
	return ar
}

// sample records a running execution's process tree and memory.
func (s *sampler) sample(ar *activeRun, root int, stats map[int]pstat, children map[int][]int, now time.Time) {
	var rss uint64
	live := 0
	var walk func(int)
	walk = func(pid int) {
		st, ok := stats[pid]
		if !ok {
			return
		}
		live++
		r := uint64(max(st.rss, 0)) * s.page
		rss += r
		p := ar.procs[pid]
		if p == nil {
			if len(ar.procs) >= s.maxProcs {
				ar.exec.ProcessesTruncated++
				ar.procs[pid] = &Proc{PID: -1} // counted once, not listed
			} else {
				p = &Proc{
					PID: pid, PPID: st.ppid, comm: st.comm,
					Command: truncate(strings.Join(cmdlineArgs(s.procRoot, pid), " "), 256),
					Start:   s.boot.Add(time.Duration(st.start) * time.Second / clkTck).UTC(),
				}
				if p.Command == "" {
					p.Command = st.comm
				}
				ar.procs[pid] = p
				ar.order = append(ar.order, pid)
			}
		}
		if p != nil && p.PID > 0 {
			// A process that exec'd since it was first seen (dispatch
			// becoming the charm's Python) gets the command it runs now.
			if st.comm != p.comm {
				p.comm = st.comm
				if c := truncate(strings.Join(cmdlineArgs(s.procRoot, pid), " "), 256); c != "" {
					p.Command = c
				}
			}
			p.LastSeen = now.UTC()
			p.CPUMs = float64(st.utime+st.stime) * 1000 / clkTck
			p.PeakRSSBytes = max(p.PeakRSSBytes, r)
		}
		for _, c := range children[pid] {
			walk(c)
		}
	}
	walk(root)
	ar.exec.Tree.PeakRSSBytes = max(ar.exec.Tree.PeakRSSBytes, rss)
	ar.exec.Tree.MaxProcs = max(ar.exec.Tree.MaxProcs, live)
	s.sampleMemory(ar)
}

// sampleMemory records the agent cgroup's current memory for its peak, every
// tick. The workloads' peaks come from burst, about 10 times a second.
func (s *sampler) sampleMemory(ar *activeRun) {
	if m, err := s.cg.memCurrent(); err == nil {
		ar.memPeak[s.cg.name] = max(ar.memPeak[s.cg.name], m)
		ar.memN[s.cg.name]++
	}
}

// burstOpen reports whether the burst window is open; see sampler.
func (s *sampler) burstOpen(now time.Time) bool {
	return len(s.workloads) > 0 && (len(s.runs) > 0 || len(s.pending) > 0 || now.Before(s.burstUntil))
}

// maxPending is how long a Pebble change that isn't ready keeps the burst
// window open.
const maxPending = 5 * time.Minute

// burst writes the workload containers' counters every BurstEvery while
// the burst window is open, and takes the running executions' workload
// peaks from them. Only workloads with a cgroup stream are read: through
// the file API, every reading would log lines in the workload. While a
// change is pending, it polls Pebble's changes once a second. With the
// window closed it costs a time comparison.
func (s *sampler) burst(now time.Time) error {
	if !s.burstOpen(now) {
		return nil
	}
	if !now.Before(s.nextBurst) {
		// Ticks come every BurstEvery or more often; a little early
		// still counts, so jitter doesn't skip one.
		s.nextBurst = now.Add(BurstEvery * 3 / 4)
		res := &Resources{Burst: true}
		for _, n := range s.workloadNames() {
			c := s.workloads[n]
			if !c.streaming(s.logf) {
				continue
			}
			cnt := c.counters(s.logf)
			res.Containers = append(res.Containers, cnt)
			if cnt.Error != "" {
				continue
			}
			for _, ar := range s.runs {
				ar.memPeak[n] = max(ar.memPeak[n], cnt.MemBytes)
				ar.memN[n]++
			}
		}
		if len(res.Containers) > 0 {
			if err := s.emit(Record{Kind: KindResources, Resources: res}); err != nil {
				return err
			}
		}
	}
	if len(s.pending) == 0 || now.Before(s.nextChanges) {
		return nil
	}
	s.nextChanges = now.Add(time.Second)
	for k, spawn := range s.pending {
		if now.Sub(spawn) > maxPending {
			delete(s.pending, k)
		}
	}
	return s.changes(false)
}

// noteChange keeps the burst window open for a Pebble change an execution
// caused: until it is ready, then for the linger time.
func (s *sampler) noteChange(ch PebbleChange, caused bool, now time.Time) {
	key := ch.Container + "/" + ch.ID
	if _, ok := s.pending[key]; !ok && !caused && !s.burstOpen(now) {
		return
	}
	if !ch.Ready {
		if _, ok := s.pending[key]; !ok {
			s.pending[key] = now
		}
		return
	}
	delete(s.pending, key)
	ready := ch.ReadyTime
	if ready.IsZero() || ready.After(now) {
		ready = now
	}
	if until := ready.Add(s.linger); until.After(s.burstUntil) {
		s.burstUntil = until
	}
}

// resetPeak opens a cgroup's memory.peak and resets it: reads through the
// returned file give the peak since. It fails where the kernel can't reset
// it (before Linux 6.12: EINVAL) or the cgroup filesystem is read-only, as
// in a Kubernetes container.
func resetPeak(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "memory.peak"), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString("reset\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// finish writes a run's Execution, then any Pebble changes it caused.
func (s *sampler) finish(pid int, now time.Time, agentStat pstat) error {
	ar := s.runs[pid]
	delete(s.runs, pid)
	e := ar.exec
	switch {
	case e.Source == SourceTap && !ar.procEnd.IsZero():
		e.End = ar.procEnd
	case e.Source == SourceTap && s.tapProcEnd:
		e.End, e.Truncated = now.UTC(), true
		e.EndSlackMs = 1000 / float64(s.runHz)
	default:
		e.End = now.UTC()
		e.EndSlackMs = 1000 / float64(s.runHz)
	}
	if !e.Overlapped && agentStat.pid != 0 {
		e.Tree.CPUUserMs = float64(sub(ar.agent0.cutime, agentStat.cutime)) * 1000 / clkTck
		e.Tree.CPUSysMs = float64(sub(ar.agent0.cstime, agentStat.cstime)) * 1000 / clkTck
	}
	self1 := selfCPUPair()
	selfUser, selfSys := self1[0]-ar.self0[0], self1[1]-ar.self0[1]
	e.ProbeCPUMs = selfUser + selfSys

	e.Cgroup = usage(ar.cg0, s.cg.counters(s.logf), ar.cg0At, now, ar.memPeak[s.cg.name], ar.memN[s.cg.name])
	if ar.peak != nil {
		var b [32]byte
		if n, err := ar.peak.ReadAt(b[:], 0); n > 0 && (err == nil || errors.Is(err, io.EOF)) {
			if v := parseUint(b[:n]); v > 0 {
				e.Cgroup.MemPeakBytes, e.Cgroup.MemPeakSource = v, MemPeakReset
			}
		}
		_ = ar.peak.Close()
	}
	if s.probeInGroup {
		e.Cgroup.CPUUserMs = max(e.Cgroup.CPUUserMs-selfUser, 0)
		e.Cgroup.CPUSysMs = max(e.Cgroup.CPUSysMs-selfSys, 0)
		e.Cgroup.CPUMs = max(e.Cgroup.CPUMs-e.ProbeCPUMs, 0)
	}
	for _, n := range s.workloadNames() {
		w0, ok := ar.work0[n]
		if !ok {
			continue
		}
		w1 := s.workloads[n].counters(s.logf)
		e.Workloads = append(e.Workloads, usage(w0, w1, ar.work0At, time.Now(), ar.memPeak[n], ar.memN[n]))
	}
	for _, p := range ar.order {
		e.Processes = append(e.Processes, *ar.procs[p])
	}
	if err := s.emit(Record{Kind: KindExecution, Execution: &e}); err != nil {
		return err
	}
	// What the charm asked its workloads to do shows up as Pebble changes;
	// write them now so they sit next to the execution. The workloads are
	// sampled in bursts until the linger time after they are ready.
	return s.changes(true)
}

// --- /proc and cgroup helpers ----------------------------------------------

func (s *sampler) readStat(pid int) (pstat, bool) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", s.procRoot, pid))
	if err != nil {
		return pstat{}, false
	}
	st, err := parseProcStat(b)
	return st, err == nil
}

func (s *sampler) execEnv(pid int) map[string]string {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/environ", s.procRoot, pid))
	if err != nil {
		return map[string]string{}
	}
	return parseExecEnv(b)
}

func (s *sampler) cgroupDirOf(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", s.procRoot, pid))
	if err != nil {
		return s.cgroupRoot
	}
	return cgroupDir(b, s.cgroupRoot)
}

// disks reports the root filesystem and Juju storage mounts.
func (s *sampler) disks() []Disk {
	paths := []string{"/"}
	if b, err := os.ReadFile(s.procRoot + "/self/mountinfo"); err == nil {
		paths = append(paths, storageMounts(b)...)
	}
	var out []Disk
	for _, p := range paths {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(p, &fs); err != nil {
			continue
		}
		bs := uint64(fs.Bsize)
		out = append(out, Disk{
			Path: p, SizeBytes: fs.Blocks * bs,
			UsedBytes: (fs.Blocks - fs.Bfree) * bs, AvailBytes: fs.Bavail * bs,
		})
	}
	return out
}

// selfCPU returns the probe's own user and system CPU, in milliseconds.
func selfCPU() (user, sys float64) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	return float64(ru.Utime.Nano()) / 1e6, float64(ru.Stime.Nano()) / 1e6
}

func selfCPUPair() [2]float64 {
	u, s := selfCPU()
	return [2]float64{u, s}
}
