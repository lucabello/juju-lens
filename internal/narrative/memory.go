package narrative

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/unit"
)

// This file reads what happened to memory around one hook: the unit's
// cgroups as the probe sampled them, the charm process's resident memory from
// the stack samples, the hooks and the Pebble changes near it. It has no
// rendering; the inspect command prints it.

const (
	// MemLead is how much of the time before the hook the readings cover.
	MemLead = 2 * time.Second
	// memBaseline is how often the probe samples cgroups between hooks.
	memBaseline = 5 * time.Second
	// MemMaxRises bounds how many jumps are listed per kind.
	MemMaxRises = 5
	// MemPathFrames bounds the charm functions named for a jump.
	MemPathFrames = 4
)

// MemPoint is one memory reading: a cgroup's or the charm process's. Prof and
// Stack say which profile and which of its stacks the process was in; -1 for
// a cgroup.
type MemPoint struct {
	T           time.Time
	Total       int64
	Anon, File  int64
	Split       bool
	Prof, Stack int
}

// MemSeries is one curve: a container's memory, or the charm processes' RSS.
type MemSeries struct {
	Name   string
	Limit  int64
	Points []MemPoint
	Events *index.MemSample // the last sample that had the limit event counters
}

// MemHook is a hook process on the unit.
type MemHook struct {
	Start, End time.Time
	Name       string
	This       bool // a process of the hook asked about
}

// Memory is the memory around one hook.
type Memory struct {
	Host               string
	From, To           time.Time // what was read
	HookStart, HookEnd time.Time
	Series             []MemSeries // the unit's containers, the charm's first
	RSS                MemSeries   // the charm processes
	Hooks              []MemHook
	Changes            []index.PebbleChange
	Profs              []recording.OpsProfile
	Peak               int64 // the charm cgroup's peak during the hook, as the probe measured it; 0 if it didn't
	PeakExact          bool  // the peak is exact, not polled between samples

	spanByID func(string) (recording.SpanRow, bool)
}

// LoadMemory reads the memory around a hook of a unit, given the roots of the
// hook's processes (Event.OpsSpans). It returns a Memory with no hook
// start when the hook has no charm-side records (the recording wasn't made with
// --attach unit). spans are the model's spans.
func LoadMemory(db *index.DB, modelID int64, spans []recording.SpanRow, unitName string, opsSpans []string) (*Memory, error) {
	byID := make(map[string]recording.SpanRow, len(spans))
	for _, sp := range spans {
		byID[sp.SpanID] = sp
	}
	m := &Memory{spanByID: func(id string) (recording.SpanRow, bool) { sp, ok := byID[id]; return sp, ok }}
	if len(opsSpans) == 0 {
		return m, nil
	}
	for _, id := range opsSpans {
		sp, ok := byID[id]
		if !ok {
			continue
		}
		if m.HookStart.IsZero() || sp.Start.Before(m.HookStart) {
			m.HookStart = sp.Start
		}
		if sp.End.After(m.HookEnd) {
			m.HookEnd = sp.End
		}
	}
	if m.HookStart.IsZero() {
		return m, nil
	}
	m.Host, _ = db.SpanHost(opsSpans[0])
	linger := unit.DefaultLinger
	if root, ok := byID[opsSpans[0]]; ok {
		m.Peak, m.PeakExact = hookPeak(root.Attrs)
		if ms, err := strconv.ParseInt(root.Attrs[recording.AttrResLingerMs], 10, 64); err == nil && ms > 0 {
			linger = time.Duration(ms) * time.Millisecond
		}
	}
	m.From = m.HookStart.Add(-MemLead)
	// The readings go on for the probe's linger time after the hook, or after
	// the last Pebble change it started was ready, whichever is later: the
	// time the probe sampled workloads densely.
	last := m.HookEnd
	caused, _ := db.PebbleChanges(modelID, m.Host, m.HookStart, m.HookEnd)
	for _, c := range caused {
		if c.Ready.After(last) {
			last = c.Ready
		}
	}
	m.To = last.Add(linger)
	// From two baseline periods before, so the curves start at the left edge
	// rather than at the first reading in the window.
	samples, err := db.MemorySamples(modelID, m.Host, m.From.Add(-2*memBaseline), m.To)
	if err != nil {
		return nil, fmt.Errorf("reading memory samples: %w", err)
	}
	m.Changes, _ = db.PebbleChanges(modelID, m.Host, m.From, m.To)

	var lastSample time.Time
	by := map[string]*MemSeries{}
	var names []string
	for _, s := range samples {
		ser := by[s.Container]
		if ser == nil {
			ser = &MemSeries{Name: s.Container}
			by[s.Container] = ser
			names = append(names, s.Container)
		}
		ser.Points = append(ser.Points, MemPoint{T: s.TS, Total: s.MemBytes, Anon: s.Anon, File: s.File, Split: s.Split, Prof: -1, Stack: -1})
		if s.MaxBytes > 0 {
			ser.Limit = s.MaxBytes
		}
		if s.Events {
			e := s
			ser.Events = &e
		}
		if s.TS.After(lastSample) {
			lastSample = s.TS
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return names[i] == "charm" && names[j] != "charm" })
	for _, n := range names {
		m.Series = append(m.Series, *by[n])
	}

	// The unit's hook processes in the window, and their RSS.
	var roots []string
	for _, sp := range spans {
		if sp.Service != recording.ServiceOps || sp.ParentSpanID != "" || sp.Unit != unitName ||
			sp.Attrs["kind"] != recording.OpsRoot || sp.End.Before(m.From) || sp.Start.After(m.To) {
			continue
		}
		this := false
		for _, id := range opsSpans {
			this = this || id == sp.SpanID
		}
		name := sp.Name
		if name == "" {
			name = "process"
		}
		m.Hooks = append(m.Hooks, MemHook{Start: sp.Start, End: sp.End, Name: name, This: this})
		roots = append(roots, sp.SpanID)
	}
	m.Profs, _ = db.Profiles(roots)
	m.RSS = MemSeries{Name: "charm process RSS"}
	for pi, p := range m.Profs {
		for _, s := range p.Timeline {
			m.RSS.Points = append(m.RSS.Points, MemPoint{T: s.Time, Total: s.RSSBytes, Prof: pi, Stack: s.Stack})
			if s.Time.After(lastSample) {
				lastSample = s.Time
			}
		}
	}
	sort.SliceStable(m.RSS.Points, func(i, j int) bool { return m.RSS.Points[i].T.Before(m.RSS.Points[j].T) })
	end := m.HookEnd
	if lastSample.After(end) {
		end = lastSample
	}
	if end = end.Add(time.Second); end.Before(m.To) {
		m.To = end
	}
	return m, nil
}

// hookPeak is the charm cgroup's memory peak during the hook, as the probe
// measured it, and whether it is exact (memory.peak reset at the hook's start,
// or a new lifetime high) or polled between samples. 0 when not measured.
func hookPeak(a map[string]string) (int64, bool) {
	v, err := strconv.ParseInt(a["res_cgroup_mem_peak_bytes"], 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, a["res_cgroup_mem_peak_exact"] == "true"
}

// Empty reports whether there is nothing to say about the hook's memory.
func (m *Memory) Empty() bool {
	return m.HookStart.IsZero() || len(m.Series) == 0 && len(m.RSS.Points) == 0
}

// Window keeps the readings from the window the memory covers.
func (m *Memory) Window(pts []MemPoint) []MemPoint {
	var out []MemPoint
	for _, p := range pts {
		if !p.T.Before(m.From) && !p.T.After(m.To) {
			out = append(out, p)
		}
	}
	return out
}

// Rel says when t was relative to the hook: "2.8 s before the hook", "1.2 s
// into the hook", "4.0 s after the hook ended".
func (m *Memory) Rel(t time.Time) string {
	d := func(x time.Duration) string { return fmt.Sprintf("%.1f s", x.Seconds()) }
	switch {
	case t.Before(m.HookStart):
		return d(m.HookStart.Sub(t)) + " before the hook"
	case t.After(m.HookEnd):
		return d(t.Sub(m.HookEnd)) + " after the hook ended"
	}
	return d(t.Sub(m.HookStart)) + " into the hook"
}

// MemRise is a stretch over which a reading only went up.
type MemRise struct {
	From, To MemPoint // the readings it began and ended at
	Biggest  MemPoint // the reading the largest single step ended at
}

// Size is how much memory the rise gained.
func (r MemRise) Size() int64 { return r.To.Total - r.From.Total }

// Rises finds the stretches of pts in which memory only went up, with
// readings no more than gap apart, that gained at least least bytes, biggest
// first. A reading of another process, or a fall, ends a stretch.
func Rises(pts []MemPoint, gap time.Duration, least int64) []MemRise {
	var out []MemRise
	var cur *MemRise
	var bestStep int64
	flush := func() {
		if cur != nil && cur.Size() >= least {
			out = append(out, *cur)
		}
		cur, bestStep = nil, 0
	}
	for i := 1; i < len(pts); i++ {
		prev, p := pts[i-1], pts[i]
		step := p.Total - prev.Total
		if step <= 0 || p.Prof != prev.Prof || p.T.Sub(prev.T) > gap {
			flush()
			continue
		}
		if cur == nil {
			cur = &MemRise{From: prev, To: p, Biggest: p}
		}
		cur.To = p
		if step > bestStep {
			bestStep, cur.Biggest = step, p
		}
	}
	flush()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size() > out[j].Size() })
	if len(out) > MemMaxRises {
		out = out[:MemMaxRises]
	}
	return out
}

// MaxTotal is the highest reading of pts.
func MaxTotal(pts []MemPoint) int64 {
	var v int64
	for _, p := range pts {
		v = max(v, p.Total)
	}
	return v
}

// StackAt says where the charm was at a reading of its process: the span it
// ran in, and its stack: the innermost frame, then the frames of the charm's
// code that called it, innermost first.
func (m *Memory) StackAt(p MemPoint) (where string, frames []string) {
	if p.Prof < 0 || p.Prof >= len(m.Profs) {
		return "", nil
	}
	prof := m.Profs[p.Prof]
	if p.Stack < 0 || p.Stack >= len(prof.Stacks) {
		return "", nil
	}
	stack := prof.Stacks[p.Stack]
	if sp, ok := m.spanByID(stack.SpanID); ok {
		switch sp.Attrs["kind"] {
		case recording.OpsObserver:
			where = "observer " + ShortObserver(sp.Name)
		case recording.OpsInit:
			where = "the charm's __init__"
		case recording.OpsRoot:
			where = "the process starting up, before ops (importing Python modules)"
		default:
			where = sp.Name
		}
	}
	charm := 0
	for i, f := range stack.Frames {
		if f < 0 || f >= len(prof.Frames) {
			continue
		}
		isCharm := IsCharmFile(prof.Frames[f].File)
		if IsNoiseFrame(prof.Frames[f]) || i > 0 && !isCharm {
			continue
		}
		if i > 0 {
			if charm++; charm > MemPathFrames {
				break
			}
		}
		fn := FrameName(prof.Frames[f])
		if len(frames) > 0 && frames[len(frames)-1] == fn {
			continue
		}
		frames = append(frames, fn)
	}
	return where, frames
}

// ChangeAt says what the workload was doing between t0 and t1: the Pebble
// change running then (during is true), or else the last one before.
func (m *Memory) ChangeAt(t0, t1 time.Time) (c index.PebbleChange, during, ok bool) {
	var running []index.PebbleChange
	var before *index.PebbleChange
	for i, ch := range m.Changes {
		if ch.Spawn.Before(t1) && (ch.Ready.IsZero() || !ch.Ready.Before(t0)) {
			running = append(running, ch)
		}
		if ch.Spawn.Before(t0) {
			before = &m.Changes[i]
		}
	}
	switch {
	case len(running) > 0:
		return running[0], true, true
	case before != nil:
		return *before, false, true
	}
	return index.PebbleChange{}, false, false
}
