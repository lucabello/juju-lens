package synth

import (
	"fmt"
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// This file gives ScenarioOps what the unit probe measures besides the tap's
// span records: the stacks the tap samples every 10ms in each hook process,
// tagged with the span running, and the probe's resource and Pebble change
// records. The charm's RSS jumps while init compresses its dashboards; the
// workload's memory drops when a replan restarts it, then climbs past where
// it was, after the hook has ended.

const (
	sampleEvery   = 10 * time.Millisecond
	stacksFlush   = time.Second
	resEvery      = 5 * time.Second
	resDenseEvery = 100 * time.Millisecond
	resLinger     = 30 * time.Second
	mib           = 1 << 20
)

// synthFrame is one Python function: file, qualified name, first line.
type synthFrame struct {
	file, fn string
	line     int
}

var (
	frMain     = []synthFrame{{"src/charm.py", "<module>", 1}, {"ops/_main.py", "main", 553}, {"ops/_main.py", "_Manager.__init__", 397}}
	frInit     = []synthFrame{{"src/charm.py", "OpenTelemetryCollectorK8sCharm.__init__", 93}, {"src/charm.py", "OpenTelemetryCollectorK8sCharm._reconcile", 101}}
	frRun      = []synthFrame{{"ops/_main.py", "_Manager.run", 533}, {"ops/framework.py", "Framework._reemit", 959}}
	frPebble   = []synthFrame{{"ops/pebble.py", "Client._request_raw", 2205}, {"socket.py", "SocketIO.readinto", 693}}
	frTool     = []synthFrame{{"ops/hookcmds/_utils.py", "run", 40}, {"subprocess.py", "Popen.communicate", 1165}}
	frConfig   = []synthFrame{{"src/config_builder.py", "ConfigBuilder.build", 212}, {"yaml/__init__.py", "dump", 290}}
	frDashload = []synthFrame{{"src/integrations.py", "forward_dashboards", 419}, {"lib/charms/grafana_k8s/v0/grafana_dashboard.py", "CharmedDashboard.load_dashboards_from_dir", 992}, {"cosl/utils.py", "LZMABase64.compress", 16}, {"lzma.py", "compress", 319}}
)

// tapProfile is what the tap's stack sampler collects in one process.
type tapProfile struct {
	m0       int64 // monotonic ns sampling started
	next     time.Time
	rss      int64
	rssStart int64
	rssPeak  int64
	cpu      int64
	samples  int
	frames   map[synthFrame]int
	frameOrd []synthFrame
	stacks   map[string]int
	stackDef [][]any // [sp, [frame ids innermost first]]
	totals   map[int]*[4]int64
	tl       []int64
	f0, s0   int // frames and stacks already written
	flushed  time.Time
}

func (p *tapProc) startProfile() {
	h := p.h
	p.prof = &tapProfile{
		m0: h.now.UnixNano() - h.mono0, next: h.now, rss: 18 * mib, rssStart: 18 * mib, rssPeak: 18 * mib,
		frames: map[synthFrame]int{}, stacks: map[string]int{}, totals: map[int]*[4]int64{}, flushed: h.now,
	}
}

// path is the call stack now, outermost first.
func (p *tapProc) path() []synthFrame {
	out := append([]synthFrame{}, frMain...)
	switch {
	case p.initOpen:
		out = append(out, frInit...)
	case p.obs != "":
		out = append(out, frRun...)
		out = append(out, synthFrame{"src/charm.py", p.obs, 300})
	case len(p.spans) > 0:
		out = append(out, frRun...)
	}
	return append(out, p.leaf...)
}

// sample takes the samples due by to: one every 10ms in the innermost span
// open, with the stack path gives. While the process waits on a call it
// uses little CPU; otherwise it uses all of it, and memory grows with
// growth per sample.
func (p *tapProc) sample(to time.Time) {
	pr := p.prof
	if pr == nil {
		return
	}
	sp := 0
	if n := len(p.spans); n > 0 {
		sp = p.spans[n-1]
	}
	for ; !pr.next.After(to); pr.next = pr.next.Add(sampleEvery) {
		path := p.path()
		ids := make([]int, len(path))
		for i, f := range path {
			id, ok := pr.frames[f]
			if !ok {
				id = len(pr.frameOrd)
				pr.frames[f] = id
				pr.frameOrd = append(pr.frameOrd, f)
			}
			ids[len(path)-1-i] = id // innermost first
		}
		key := fmt.Sprint(sp, ids)
		sid, ok := pr.stacks[key]
		if !ok {
			sid = len(pr.stackDef)
			pr.stacks[key] = sid
			pr.stackDef = append(pr.stackDef, []any{sp, ids})
		}
		cpu := int64(sampleEvery)
		grow := int64(24 << 10)
		if len(p.leaf) > 0 && p.leaf[0] != frDashload[0] && p.leaf[0] != frConfig[0] {
			cpu, grow = int64(300*time.Microsecond), 0
		}
		if len(p.leaf) > 0 && p.leaf[0] == frDashload[0] {
			grow = p.growth
		}
		pr.rss += grow
		pr.rssPeak = max(pr.rssPeak, pr.rss)
		pr.cpu += cpu
		pr.samples++
		t := pr.totals[sid]
		if t == nil {
			t = &[4]int64{}
			pr.totals[sid] = t
		}
		t[0]++
		t[1] += int64(sampleEvery)
		t[2] += cpu
		t[3] += grow
		m := pr.next.UnixNano() - p.h.mono0
		pr.tl = append(pr.tl, (m-pr.m0)/1000, int64(sid), pr.rss>>10)
		p.h.rssLog = append(p.h.rssLog, rssAt{pr.next, pr.rss})
	}
	if to.Sub(pr.flushed) >= stacksFlush {
		p.flushStacks()
	}
}

// flushStacks writes a stacks record: what is new since the last one.
func (p *tapProc) flushStacks() {
	pr := p.prof
	if pr == nil || len(pr.tl) == 0 {
		return
	}
	frames := make([][]any, 0, len(pr.frameOrd)-pr.f0)
	for _, f := range pr.frameOrd[pr.f0:] {
		frames = append(frames, []any{p.h.charmDir() + "/" + f.file, f.fn, f.line})
	}
	ids := make([]int, 0, len(pr.totals))
	for id := range pr.totals {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	totals := make([][5]int64, 0, len(ids))
	for _, id := range ids {
		t := pr.totals[id]
		totals = append(totals, [5]int64{int64(id), t[0], t[1], t[2], t[3]})
	}
	p.rec("stacks", map[string]any{
		"m0": pr.m0, "f0": pr.f0, "frames": frames, "s0": pr.s0, "stacks": pr.stackDef[pr.s0:],
		"totals": totals, "tl": pr.tl,
	})
	pr.f0, pr.s0 = len(pr.frameOrd), len(pr.stackDef)
	pr.totals, pr.tl, pr.flushed = map[int]*[4]int64{}, nil, p.h.now
}

// endProfile writes the last stacks and stacks_end, as the tap does at exit.
func (p *tapProc) endProfile() {
	pr := p.prof
	if pr == nil {
		return
	}
	p.flushStacks()
	p.rec("stacks_end", map[string]any{
		"samples": pr.samples, "dropped": 0, "interval_ns": int64(sampleEvery),
		"rss_start": pr.rssStart, "rss_peak": pr.rssPeak, "cpu_ns": pr.samples * 40_000,
	})
}

// spanRec numbers the spans the tap numbers for its stack samples, as the
// tap does: a record that opens one carries its number, sp.
func (p *tapProc) spanRec(kind string, fields map[string]any) map[string]any {
	open := func() {
		if fields == nil {
			fields = map[string]any{}
		}
		p.nextSp++
		fields["sp"] = p.nextSp
		p.spans = append(p.spans, p.nextSp)
	}
	endInit := func() {
		if p.initOpen {
			p.initOpen = false
			p.spans = p.spans[:len(p.spans)-1]
		}
	}
	switch kind {
	case "dispatch_start":
		open()
		p.initOpen = true
	case "reemit_start":
		endInit()
		open()
	case "emit":
		if fields["depth"] == 0 && fields["cls"] == "juju" {
			endInit()
		}
		open()
	case "observer_start":
		open()
		p.obsStack = append(p.obsStack, p.obs)
		p.obs, _ = fields["obs"].(string)
	case "observer_end":
		p.obs = p.obsStack[len(p.obsStack)-1]
		p.obsStack = p.obsStack[:len(p.obsStack)-1]
		fallthrough
	case "reemit_backlog", "emit_end":
		endInit()
		if len(p.spans) > 0 {
			p.spans = p.spans[:len(p.spans)-1]
		}
	}
	return fields
}

// rssAt is the RSS of a hook process at one sample, for the charm
// container's memory.
type rssAt struct {
	t   time.Time
	rss int64
}

// hookSpan is when a hook process ran.
type hookSpan struct{ start, end time.Time }

// resources appends the probe's resource and Pebble change records, from
// the hello to 40s past the last record: every 5s, and every 100ms while a
// hook runs and for 30s after it.
func (h *tapHost) resources() {
	start := h.out[0].Rec.TS
	end := h.now.Add(40 * time.Second)
	var times []time.Time
	for t := start; t.Before(end); {
		times = append(times, t)
		step := resEvery
		for _, hs := range h.hooks {
			if !t.Before(hs.start.Add(-resEvery)) && t.Before(hs.end.Add(resLinger)) {
				step = resDenseEvery
			}
		}
		next := t.Add(step)
		for _, hs := range h.hooks { // don't step over a hook's start
			if hs.start.After(t) && hs.start.Before(next) {
				next = hs.start
			}
		}
		t = next
	}
	var charmCPU, workCPU uint64
	prev := start
	for _, t := range times {
		dt := uint64(t.Sub(prev).Microseconds())
		prev = t
		charm := int64(22 * mib)
		for _, hs := range h.hooks {
			if !t.Before(hs.start) && !t.After(hs.end) {
				charm += h.rssBefore(t)
				charmCPU += dt * 9 / 10
			}
		}
		workCPU += dt / 50
		h.out = append(h.out, UnitRecord{Host: h.host, Rec: unit.Record{Kind: unit.KindResources, TS: t, Resources: &unit.Resources{
			Containers: []unit.Counters{
				{Container: "charm", CPUUsec: charmCPU, CPUUserUsec: charmCPU * 4 / 5, CPUSysUsec: charmCPU / 5, MemBytes: uint64(charm)},
				{Container: "otelcol", CPUUsec: workCPU, CPUUserUsec: workCPU, MemBytes: uint64(h.workloadMem(t)), MemMaxBytes: 1 << 30},
			},
			ProbeCPUMs: float64(t.Sub(start).Milliseconds()) / 200, ProbeRSSBytes: 12 * mib,
		}}})
	}
	for i, c := range h.changes {
		id := fmt.Sprint(24 + i)
		spawn := unit.PebbleChange{Container: "otelcol", ID: id, Kind: c.kind, Summary: c.summary, Status: "Doing", Spawn: c.at}
		done := spawn
		done.Status, done.Ready, done.ReadyTime = "Done", true, c.at.Add(c.took)
		h.out = append(h.out,
			UnitRecord{Host: h.host, Rec: unit.Record{Kind: unit.KindPebbleChange, TS: c.at.Add(50 * time.Millisecond), PebbleChange: &spawn}},
			UnitRecord{Host: h.host, Rec: unit.Record{Kind: unit.KindPebbleChange, TS: done.ReadyTime.Add(50 * time.Millisecond), PebbleChange: &done}})
	}
	sort.SliceStable(h.out, func(i, j int) bool { return h.out[i].Rec.TS.Before(h.out[j].Rec.TS) })
}

// rssBefore is the RSS of the last hook process sample at or before t.
func (h *tapHost) rssBefore(t time.Time) int64 {
	i := sort.Search(len(h.rssLog), func(i int) bool { return h.rssLog[i].t.After(t) })
	if i == 0 {
		return 0
	}
	return h.rssLog[i-1].rss
}

// workloadMem is the collector's memory at t: steady at 140MiB until a
// change restarts it; then, once the change is ready, it starts from 40MiB,
// climbs to 230MiB over 8s as it fills its queues and page cache, and
// settles at 190MiB over the next 20s.
func (h *tapHost) workloadMem(t time.Time) int64 {
	mem := int64(140 * mib)
	for _, c := range h.changes {
		ready := c.at.Add(c.took)
		switch {
		case t.Before(c.at):
		case t.Before(ready):
			mem = 140 * mib
		default:
			d := t.Sub(ready)
			up := min(float64(d)/float64(8*time.Second), 1)
			down := min(max(float64(d-8*time.Second)/float64(20*time.Second), 0), 1)
			mem = int64(40*mib + up*190*mib - down*40*mib)
		}
	}
	return mem
}

// synthChange is a Pebble change a hook's replan or restart started.
type synthChange struct {
	at            time.Time
	took          time.Duration
	kind, summary string
}
