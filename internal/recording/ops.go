package recording

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// ServiceOps is the Service of the spans built from what the tap recorded
// inside a charm (`record --attach unit`): one tree per hook process, next to
// the RPC spans in the same table.
const ServiceOps = "ops"

// Ops span kinds, in Attrs["kind"].
const (
	OpsRoot     = "root"     // a hook, action or other charm Python process
	OpsInit     = "init"     // charm init: from ops.main to the deferred events
	OpsBacklog  = "backlog"  // deferred events re-emitted before the hook's own event
	OpsEvent    = "event"    // one event: emitted, or re-emitted from the backlog
	OpsObserver = "observer" // one observer called for an event
	OpsTool     = "tool"     // a hook tool call, such as relation-get or status-set
	OpsPebble   = "pebble"   // a Pebble API call
)

// What a root span's process was, in Attrs["exec"].
const (
	ExecHook    = "hook"
	ExecAction  = "action"
	ExecProcess = "process" // a Python process a hook started, or one run by `juju exec`
)

// Memory attributes of the root, init, backlog, event and observer spans,
// from the process's resident memory the tap records as each span opens and
// closes, and its stack samples in between.
const (
	AttrRSSStart = "rss_start_bytes"
	AttrRSSEnd   = "rss_end_bytes"
	// AttrRSSPeak is the highest of the start, the end and the stack
	// samples taken within the span.
	AttrRSSPeak = "rss_peak_bytes"
	// AttrRSSHWMExact is "true" when the process's high-water mark rose
	// within the span: the peak is then that mark, exact, however short.
	AttrRSSHWMExact = "rss_hwm_exact"
)

// opsKey names one hook process: the spool file it wrote, in the unit files
// of one host (the <host> directory of raw/unit/<model>/<host>/).
type opsKey struct{ model, host, file string }

func (k opsKey) hostKey() string { return k.model + "\x00" + k.host }

// tapRec holds the tap record fields the tree is built from. Every field,
// these included, also goes into the span's Attrs, except the clock, kind,
// span number and memory fields.
type tapRec struct {
	T        int64             `json:"t"`
	M        int64             `json:"m"`
	K        string            `json:"k"`
	PPID     int               `json:"ppid"`
	Argv     []string          `json:"argv"`
	Env      map[string]string `json:"env"`
	Ev       string            `json:"ev"`
	EvKind   string            `json:"ev_kind"`
	Cls      string            `json:"cls"`
	Depth    int               `json:"depth"`
	Obs      string            `json:"obs"`
	Reemit   bool              `json:"reemit"`
	Deferred bool              `json:"deferred"`
	DurNs    int64             `json:"dur_ns"`
	Tool     string            `json:"tool"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Status   json.RawMessage   `json:"status"`
	RC       *int              `json:"rc"`
	Exc      string            `json:"exc"`
	ExcMsg   string            `json:"exc_msg"`
	// Sp numbers the span a record opens; stack samples name it.
	Sp *int `json:"sp"`
	// RSS and HWM are the process's resident memory and its high-water
	// mark, in bytes, on the records that open and close spans.
	RSS *int64 `json:"rss"`
	HWM *int64 `json:"hwm"`
}

type opsLine struct {
	rec    tapRec
	fields map[string]json.RawMessage
	raw    json.RawMessage // stacks records only, which have no fields
	rel    string
	offset int64
}

// isStacksKind reports whether a tap record kind carries stack samples.
func isStacksKind(k string) bool { return k == "stacks" || k == "stacks_end" }

type opsProc struct {
	key      opsKey
	unit     string
	pid      int
	lines    []opsLine
	agentPID int             // the host's agent, from the session's hello; 0 if unknown
	parent   *opsKey         // the tapped process that started this one
	hook     string          // its spans' Hook, settled at proc_start
	res      *unit.Execution // what it cost, from the probe; nil if not measured
}

// OpsAssembler groups the tap records of raw/unit/ by hook process and turns
// each finished process into a span tree. Feed it every line of the unit
// files in order; a process's spans come out with its spool_end record, and
// its stack samples, if it took any, from TakeProfiles.
type OpsAssembler struct {
	procs    map[opsKey]*opsProc
	agentPID map[string]int            // model + host -> agent pid
	running  map[string]map[int]opsKey // model + host -> pid -> process not ended yet
	profiles []OpsProfile              // built, not taken yet
}

// NewOpsAssembler returns an empty assembler.
func NewOpsAssembler() *OpsAssembler {
	return &OpsAssembler{
		procs:    map[opsKey]*opsProc{},
		agentPID: map[string]int{},
		running:  map[string]map[int]opsKey{},
	}
}

// TakeProfiles returns the profiles of the processes built since the last
// call.
func (a *OpsAssembler) TakeProfiles() []OpsProfile {
	p := a.profiles
	a.profiles = nil
	return p
}

// Feed takes one line of raw/unit/<model>/<host>/, read from rel at offset,
// and returns the spans of the process it completed, if any. An execution
// the probe measured goes onto the root of its process, whose spool_end
// follows it; one the tap didn't record is a root span of its own.
func (a *OpsAssembler) Feed(model string, line []byte, rel string, offset int64) []SpanRow {
	var r unit.Record
	if err := json.Unmarshal(line, &r); err != nil {
		return nil
	}
	host := hostOf(rel)
	switch {
	case r.Kind == unit.KindHello && r.Hello != nil:
		// Each session's hello names the agent hooks are started by: a
		// Python process with another parent is a subprocess. The previous
		// session's processes that never ended were cut off with it.
		hk := model + "\x00" + host
		a.agentPID[hk] = r.Hello.AgentPID
		return a.flush(hk, endCut)
	case r.Kind == unit.KindTap && r.Tap != nil && len(r.Tap.Record) > 0:
		a.feedTap(model, host, r.Tap, rel, offset)
	case r.Kind == unit.KindSpoolEnd && r.SpoolEnd != nil:
		p := a.procs[opsKey{model, host, r.SpoolEnd.File}]
		if p == nil || !a.isRunning(p) {
			return nil
		}
		delete(a.running[p.key.hostKey()], p.pid)
		end := endSpool
		if r.SpoolEnd.Truncated {
			end = endKilled
		}
		return a.build(p, end)
	case r.Kind == unit.KindExecution && r.Execution != nil:
		e := r.Execution
		if e.TapFile != "" {
			if p := a.procs[opsKey{model, host, e.TapFile}]; p != nil && a.isRunning(p) {
				p.res = e
				return nil
			}
		}
		return []SpanRow{execRoot(model, host, e, rel, offset)}
	}
	return nil
}

func (a *OpsAssembler) feedTap(model, host string, t *unit.Tap, rel string, offset int64) {
	var rec tapRec
	if err := json.Unmarshal(t.Record, &rec); err != nil {
		return
	}
	ln := opsLine{rec: rec, rel: rel, offset: offset}
	if isStacksKind(rec.K) {
		// Read once the tree is built; none of it goes into attributes.
		ln.raw = t.Record
	} else if err := json.Unmarshal(t.Record, &ln.fields); err != nil {
		return
	}
	key := opsKey{model, host, t.File}
	hk := key.hostKey()
	p := a.procs[key]
	if p == nil {
		p = &opsProc{key: key, unit: t.Unit, pid: t.PID, agentPID: a.agentPID[hk]}
		a.procs[key] = p
		if a.running[hk] == nil {
			a.running[hk] = map[int]opsKey{}
		}
		a.running[hk][t.PID] = key
	}
	if rec.K == "proc_start" {
		parentHook := ""
		if rec.PPID != 0 && rec.PPID != p.agentPID {
			if pk, ok := a.running[hk][rec.PPID]; ok && pk != key {
				p.parent = &pk
				parentHook = a.procs[pk].hook
			}
		}
		// Now, while a parent still runs: a subprocess usually ends first.
		exec, name := execOf(rec, p.agentPID)
		p.hook = hookOf(exec, name, parentHook)
	}
	p.lines = append(p.lines, ln)
}

// isRunning reports whether p has records but no spool_end yet.
func (a *OpsAssembler) isRunning(p *opsProc) bool {
	k, ok := a.running[p.key.hostKey()][p.pid]
	return ok && k == p.key
}

// Pending returns the spans of the processes that have no spool_end yet:
// still running when the recording ended. Their roots carry open=true.
// Call it once, after the last Feed.
func (a *OpsAssembler) Pending() []SpanRow {
	return a.flush("", endOpen)
}

// flush builds the processes of host hk (every host when "") that have no
// spool_end yet, ending as end says.
func (a *OpsAssembler) flush(hk string, end procEnd) []SpanRow {
	var open []*opsProc
	for _, p := range a.procs {
		if (hk == "" || p.key.hostKey() == hk) && a.isRunning(p) {
			open = append(open, p)
		}
	}
	// Parents first, so a subprocess finds its parent's hook name.
	sort.Slice(open, func(i, j int) bool {
		if ti, tj := open[i].lines[0].rec.T, open[j].lines[0].rec.T; ti != tj {
			return ti < tj
		}
		return open[i].key.file < open[j].key.file
	})
	var out []SpanRow
	for _, p := range open {
		out = append(out, a.build(p, end)...)
	}
	for _, p := range open {
		delete(a.running[p.key.hostKey()], p.pid)
	}
	return out
}

// procEnd is how the records of a process ended.
type procEnd int

const (
	endOpen   procEnd = iota // no spool_end: still running when the recording ended
	endSpool                 // spool_end: it exited
	endKilled                // spool_end, with no proc_end: it was killed
	endCut                   // its unit probe session ended, and a new one started, first
)

// LoadOps reads every unit probe file under raw/unit/ and returns the span
// trees of the hook processes in them, ordered by start time, and the stack
// samples of those that took any.
func LoadOps(root string) ([]SpanRow, []OpsProfile, error) {
	a := NewOpsAssembler()
	var out []SpanRow
	err := eachUnitLine(root, func(model, rel string, line []byte, off int64) {
		out = append(out, a.Feed(model, line, rel, off)...)
	})
	if err != nil {
		return nil, nil, err
	}

	out = append(out, a.Pending()...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	profiles := a.TakeProfiles()
	sort.SliceStable(profiles, func(i, j int) bool { return profiles[i].Start.Before(profiles[j].Start) })
	return out, profiles, nil
}

// UnitFileModel returns the model segment of a unit probe file's path
// relative to the recording, raw/unit/<model>/<host>/unit-<hour>.jsonl.
func UnitFileModel(rel string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 5 || parts[0] != "raw" || parts[1] != "unit" {
		return "", false
	}
	return parts[2], true
}

// hostOf returns the host segment of a unit probe file's relative path.
func hostOf(rel string) string {
	return path.Base(path.Dir(filepath.ToSlash(rel)))
}

// opsNode is a span under construction.
type opsNode struct {
	row    SpanRow
	kind   string
	ev     string // event path, for event nodes
	obs    string // observer, for observer nodes
	startM int64
	open   bool
	// synthetic: an event span made up to group the observers a deferred
	// event was re-emitted to; the tap records no emit for those.
	synthetic bool
	lastEndM  int64
	// The process's memory as the span opened and closed; for a synthetic
	// span, lastMem is the memory at lastEndM.
	memStart, memEnd, lastMem memReading
}

// memReading is the process's memory at one tap record.
type memReading struct {
	rss, hwm int64
	ok       bool // the record had them
}

func readingOf(r tapRec) memReading {
	if r.RSS == nil {
		return memReading{}
	}
	m := memReading{rss: *r.RSS, ok: true}
	if r.HWM != nil {
		m.hwm = *r.HWM
	}
	return m
}

// opsTree replays one process's records into spans.
type opsTree struct {
	p      *opsProc
	t0, m0 int64 // the process's wall and monotonic clocks at its first record
	nodes  []*opsNode
	stack  []*opsNode
	init   *opsNode
	leaf   *opsNode // the last hook tool span, which a status_set completes
	root   *opsNode
	// cur is the memory on the record being replayed, last the latest
	// one any record had.
	cur, last memReading
}

// build turns a process's records into its span tree.
func (a *OpsAssembler) build(p *opsProc, end procEnd) []SpanRow {
	if len(p.lines) == 0 {
		return nil
	}
	tr := &opsTree{p: p, t0: p.lines[0].rec.T, m0: p.lines[0].rec.M}
	traceID, parentID := opsTraceID(p.key), ""
	if p.parent != nil {
		if pp := a.procs[*p.parent]; pp != nil {
			traceID, parentID = opsTraceID(pp.key), opsSpanID(pp.key, 0)
		}
	}
	first := p.lines[0]
	tr.cur = readingOf(first.rec)
	tr.root = tr.newNode(OpsRoot, "", first.rec.M, first, traceID, parentID)
	tr.stack = []*opsNode{tr.root}

	procEnded := false
	lastM := first.rec.M
	var warns []json.RawMessage
	// The spans the tap numbered, for its stack samples; 0 is the process.
	numbered := map[int]*opsNode{0: tr.root}
	number := func(r tapRec, n *opsNode) {
		if r.Sp != nil && n != nil {
			numbered[*r.Sp] = n
		}
	}
	var prof *profileBuilder
	for _, ln := range p.lines {
		r := ln.rec
		lastM = r.M
		if tr.cur = readingOf(r); tr.cur.ok {
			tr.last = tr.cur
		}
		switch r.K {
		case "proc_start":
			mergeAttrs(tr.root.row.Attrs, ln.fields, nil)
			tr.name(r)
		case "ops_loaded":
			mergeAttrs(tr.root.row.Attrs, ln.fields, map[string]string{
				"version": "ops_version", "path": "ops_path", "patched": "ops_patched",
			})
		case "dispatch_start":
			tr.init = tr.push(OpsInit, "charm init", r.M, ln)
			number(r, tr.init)
		case "reemit_start":
			tr.endInit(r.M)
			number(r, tr.push(OpsBacklog, "deferred events", r.M, ln))
		case "reemit_backlog":
			tr.endInit(r.M)
			tr.popTo(func(n *opsNode) bool { return n.kind == OpsBacklog }, r.M, ln.fields)
		case "emit":
			if r.Depth == 0 && (r.Cls == "juju" || r.Cls == "action") {
				tr.endInit(r.M) // ops versions that don't re-emit first
			}
			n := tr.push(OpsEvent, r.EvKind, r.M, ln)
			n.ev = r.Ev
			number(r, n)
		case "emit_end":
			tr.popTo(func(n *opsNode) bool { return n.kind == OpsEvent && n.ev == r.Ev }, r.M, ln.fields)
		case "observer_start":
			if r.Reemit {
				tr.reemitted(r, ln)
			}
			n := tr.push(OpsObserver, r.Obs, r.M, ln)
			n.obs = r.Obs
			number(r, n)
		case "observer_end":
			n := tr.popTo(func(n *opsNode) bool { return n.kind == OpsObserver && n.obs == r.Obs }, r.M, ln.fields)
			if n != nil && r.Deferred {
				tr.markDeferred(r.Ev)
			}
		case "defer":
			tr.markDeferred(r.Ev)
		case "hook_tool":
			tr.leaf = tr.leafNode(OpsTool, r.Tool, r, ln)
		case "status_set":
			if l := tr.leaf; l != nil && l.row.Name == "status-set" && l.row.Attrs["status"] == "" {
				mergeAttrs(l.row.Attrs, ln.fields, map[string]string{"rc": ""})
			} else {
				tr.leafNode(OpsTool, "status-set", r, ln)
			}
		case "pebble":
			tr.leafNode(OpsPebble, strings.TrimSpace(r.Method+" "+r.Path), r, ln)
		case "tap_warn":
			if b, err := json.Marshal(ln.fields); err == nil {
				warns = append(warns, b)
			}
		case "dispatch_end":
			if r.Exc != "" && r.Exc != "SystemExit" && tr.init != nil && tr.init.open {
				// The charm raised while it was being constructed.
				tr.init.row.Attrs["exc"], tr.init.row.Attrs["exc_msg"] = r.Exc, r.ExcMsg
			}
			tr.endInit(r.M)
			tr.popTo(func(n *opsNode) bool { return n == tr.root }, r.M, nil)
			setExit(tr.root.row.Attrs, r)
		case "proc_end":
			tr.popTo(func(n *opsNode) bool { return n == tr.root }, r.M, nil)
			tr.close(tr.root, r.M)
			setExit(tr.root.row.Attrs, r)
			procEnded = true
		case "stacks":
			if prof == nil {
				prof = &profileBuilder{}
			}
			prof.add(ln.raw, tr)
		case "stacks_end":
			if prof == nil {
				prof = &profileBuilder{}
			}
			prof.end(ln.raw)
		}
	}
	if tr.root.row.Name == "" {
		// No proc_start: the tap's process feature was off.
		tr.name(tapRec{})
	}
	if len(warns) > 0 {
		b, _ := json.Marshal(warns)
		tr.root.row.Attrs["tap_warn"] = string(b)
	}

	// Close what the process never closed, at its last record. The root
	// says why (open or truncated, below); the others are just unfinished,
	// with no memory at their end.
	tr.cur = memReading{}
	for _, n := range tr.nodes {
		if n.open {
			tr.close(n, lastM)
			if n != tr.root {
				n.row.Attrs["unfinished"] = "true"
			}
		}
	}
	if !tr.root.memEnd.ok {
		tr.root.memEnd = tr.last
	}
	root := &tr.root.row
	// Python can still log through juju-log while it shuts down, after the
	// tap's atexit record: the process lasts until its last record.
	if end := tr.wall(lastM); end.After(root.End) {
		root.End = end
	}
	switch {
	case procEnded:
	case end == endOpen:
		root.Attrs["open"] = "true"
	case end == endKilled:
		root.Attrs["truncated"] = "true"
		root.Attrs["tap"], root.Attrs["tap_reason"] = TapTruncated, ReasonKilled
	case end == endCut:
		root.Attrs["truncated"] = "true"
		root.Attrs["tap"], root.Attrs["tap_reason"] = TapTruncated, ReasonSessionCut
	}
	root.StatusCode, root.StatusMsg = rootStatus(root.Attrs, procEnded, end)
	if p.res != nil {
		for k, v := range resAttrs(p.res) {
			root.Attrs[k] = v
		}
	}
	var timeline []ProfileSample
	if prof != nil {
		timeline = prof.p.Timeline
	}
	tr.memAttrs(timeline)
	if prof != nil {
		a.profiles = append(a.profiles, prof.finish(tr.root, numbered))
	}
	p.lines = nil // only its key and hook are needed now, by its subprocesses

	out := make([]SpanRow, len(tr.nodes))
	for i, n := range tr.nodes {
		n.row.Hook = root.Hook
		if n.kind != OpsRoot {
			n.row.StatusCode, n.row.StatusMsg = childStatus(n)
		}
		out[i] = n.row
	}
	return out
}

// execOf tells from proc_start what a process is: a hook or an action when
// the unit's agent started it, else another Python process. name is the
// hook or action name.
func execOf(r tapRec, agentPID int) (exec, name string) {
	env := r.Env
	exec = ExecProcess
	dispatch := env["dispatch_path"]
	switch {
	case strings.HasPrefix(dispatch, "hooks/"):
		exec, name = ExecHook, strings.TrimPrefix(dispatch, "hooks/")
	case strings.HasPrefix(dispatch, "actions/"):
		exec, name = ExecAction, strings.TrimPrefix(dispatch, "actions/")
	case env["hook_name"] != "":
		exec, name = ExecHook, env["hook_name"]
	case env["action_name"] != "":
		exec, name = ExecAction, env["action_name"]
	}
	// A subprocess of a hook inherits its JUJU_* environment.
	if agentPID != 0 && r.PPID != 0 && r.PPID != agentPID {
		exec = ExecProcess
	}
	return exec, name
}

// hookOf is the Hook of a process's spans: the hook, the action, or for
// another process the hook of the process that started it.
func hookOf(exec, name, parentHook string) string {
	switch exec {
	case ExecHook:
		return name
	case ExecAction:
		return name + " action"
	}
	return parentHook
}

// name fills the root's name, exec and model from proc_start. The Hook was
// settled as the record arrived (feedTap), while the parent still ran.
func (tr *opsTree) name(r tapRec) {
	root := &tr.root.row
	env := r.Env
	exec, name := execOf(r, tr.p.agentPID)
	if m := env["model_name"]; m != "" {
		for _, n := range tr.nodes {
			n.row.Model = m
		}
	}
	for _, n := range tr.nodes {
		n.row.ModelUUID = env["model_uuid"]
	}
	root.Attrs["exec"] = exec
	root.Hook = tr.p.hook
	switch exec {
	case ExecHook:
		root.Name = name
		root.Attrs["hook_kind"] = bareHookKind(name, env)
	case ExecAction:
		root.Name = name + " action"
	default:
		root.Name = "python"
		if len(r.Argv) > 1 {
			root.Name = "python " + path.Base(r.Argv[1])
		}
	}
}

// bareHookKind strips the endpoint, storage or container prefix Juju adds
// to some hook names ("db-relation-changed" -> "relation-changed"), giving
// the kind the uniter's run-hook marker carries.
func bareHookKind(name string, env map[string]string) string {
	prefix := ""
	switch {
	case env["relation"] != "":
		prefix = env["relation"]
	case env["storage_id"] != "":
		prefix = env["storage_id"]
		if i := strings.IndexByte(prefix, '/'); i >= 0 {
			prefix = prefix[:i]
		}
	case env["workload_name"] != "":
		prefix = env["workload_name"]
	}
	if prefix != "" && strings.HasPrefix(name, prefix+"-") {
		return name[len(prefix)+1:]
	}
	return name
}

func (tr *opsTree) wall(m int64) time.Time {
	return time.Unix(0, tr.t0+(m-tr.m0)).UTC()
}

func (tr *opsTree) newNode(kind, name string, startM int64, ln opsLine, traceID, parentID string) *opsNode {
	p := tr.p
	n := &opsNode{
		kind: kind, startM: startM, open: true, memStart: tr.cur,
		row: SpanRow{
			TraceID:      traceID,
			SpanID:       opsSpanID(p.key, len(tr.nodes)),
			ParentSpanID: parentID,
			Name:         name,
			Start:        tr.wall(startM),
			Service:      ServiceOps,
			Model:        p.key.model,
			App:          appOf(p.unit),
			Unit:         p.unit,
			RawFile:      ln.rel,
			RawOffset:    ln.offset,
			Attrs:        map[string]string{"kind": kind},
		},
	}
	if len(tr.nodes) > 0 {
		n.row.Model, n.row.ModelUUID = tr.root.row.Model, tr.root.row.ModelUUID
	}
	tr.nodes = append(tr.nodes, n)
	return n
}

func (tr *opsTree) top() *opsNode { return tr.stack[len(tr.stack)-1] }

func (tr *opsTree) push(kind, name string, startM int64, ln opsLine) *opsNode {
	n := tr.newNode(kind, name, startM, ln, tr.root.row.TraceID, tr.top().row.SpanID)
	mergeAttrs(n.row.Attrs, ln.fields, nil)
	tr.stack = append(tr.stack, n)
	return n
}

// close ends n at endM, with the memory on the record that ended it; a
// synthetic span ends with its last observer.
func (tr *opsTree) close(n *opsNode, endM int64) {
	n.open = false
	n.row.End = tr.wall(endM)
	n.memEnd = tr.cur
	if n.synthetic {
		n.memEnd = n.lastMem
	}
	for i := len(tr.stack) - 1; i >= 0; i-- {
		if p := tr.stack[i]; p != n && p.synthetic && p.open && endM > p.lastEndM {
			p.lastEndM, p.lastMem = endM, tr.cur
			break
		}
	}
}

// memAttrs gives the root, init, the backlog, events and observers their
// memory attributes (AttrRSSStart and the rest), from the readings at
// their ends and the stack samples' timeline, in time order.
func (tr *opsTree) memAttrs(timeline []ProfileSample) {
	for _, n := range tr.nodes {
		switch n.kind {
		case OpsRoot, OpsInit, OpsBacklog, OpsEvent, OpsObserver:
		default:
			continue
		}
		if !n.memStart.ok {
			continue
		}
		a := n.row.Attrs
		a[AttrRSSStart] = strconv.FormatInt(n.memStart.rss, 10)
		peak := n.memStart.rss
		if e := n.memEnd; e.ok {
			a[AttrRSSEnd] = strconv.FormatInt(e.rss, 10)
			peak = max(peak, e.rss)
			if e.hwm > n.memStart.hwm {
				peak = max(peak, e.hwm)
				a[AttrRSSHWMExact] = "true"
			}
		}
		i := sort.Search(len(timeline), func(i int) bool { return !timeline[i].Time.Before(n.row.Start) })
		for ; i < len(timeline) && !timeline[i].Time.After(n.row.End); i++ {
			peak = max(peak, timeline[i].RSSBytes)
		}
		a[AttrRSSPeak] = strconv.FormatInt(peak, 10)
	}
}

// popTo closes the open spans down to and including the innermost one match
// selects, at endM, merging fields into it. A span above it lost its end
// record and closes with it. Nothing closes when no open span matches.
func (tr *opsTree) popTo(match func(*opsNode) bool, endM int64, fields map[string]json.RawMessage) *opsNode {
	for i := len(tr.stack) - 1; i >= 0; i-- {
		n := tr.stack[i]
		if !match(n) {
			continue
		}
		for j := len(tr.stack) - 1; j >= i; j-- {
			if c := tr.stack[j]; c.synthetic {
				tr.close(c, max(c.lastEndM, c.startM))
			} else if c != tr.root {
				tr.close(c, endM)
			}
		}
		mergeAttrs(n.row.Attrs, fields, nil)
		if n == tr.root {
			tr.stack = tr.stack[:1] // the root stays open until proc_end
			return n
		}
		tr.stack = tr.stack[:i]
		return n
	}
	return nil
}

// endInit closes charm init: the charm is constructed once ops starts
// re-emitting deferred events or emitting the hook's event.
func (tr *opsTree) endInit(m int64) {
	if tr.init != nil && tr.init.open {
		tr.popTo(func(n *opsNode) bool { return n == tr.init }, m, nil)
	}
}

// reemitted puts an observer that a deferred event is re-emitted to under
// an event span for that event, opening one when the event changes.
func (tr *opsTree) reemitted(r tapRec, ln opsLine) {
	if t := tr.top(); t.synthetic {
		if t.ev == r.Ev {
			return
		}
		tr.popTo(func(n *opsNode) bool { return n == t }, max(t.lastEndM, t.startM), nil)
	}
	if tr.top().kind != OpsBacklog {
		return
	}
	n := tr.newNode(OpsEvent, r.EvKind, r.M, ln, tr.root.row.TraceID, tr.top().row.SpanID)
	n.ev, n.synthetic = r.Ev, true
	for _, k := range []string{"ev", "ev_kind", "ev_class", "cls"} {
		if v, ok := ln.fields[k]; ok {
			n.row.Attrs[k] = attrValue(v)
		}
	}
	n.row.Attrs["reemitted"] = "true"
	tr.stack = append(tr.stack, n)
}

// markDeferred marks the innermost open span of event ev as deferred.
func (tr *opsTree) markDeferred(ev string) {
	for i := len(tr.stack) - 1; i >= 0; i-- {
		if n := tr.stack[i]; n.kind == OpsEvent && n.ev == ev {
			n.row.Attrs["deferred"] = "true"
			return
		}
	}
}

// leafNode adds a span for a call the tap records once, as it returns.
func (tr *opsTree) leafNode(kind, name string, r tapRec, ln opsLine) *opsNode {
	n := tr.newNode(kind, name, r.M-max(r.DurNs, 0), ln, tr.root.row.TraceID, tr.top().row.SpanID)
	mergeAttrs(n.row.Attrs, ln.fields, nil)
	tr.close(n, r.M)
	return n
}

// mergeAttrs copies a record's fields into attrs, renamed through rename
// (an empty new name drops the field). The record's clock, kind, span
// number and memory fields are left out: the span carries its own times,
// kind and memory attributes.
func mergeAttrs(attrs map[string]string, fields map[string]json.RawMessage, rename map[string]string) {
	for k, v := range fields {
		switch k {
		case "t", "m", "k", "dur_ns", "sp", "rss", "hwm":
			continue
		}
		if to, ok := rename[k]; ok {
			if to == "" {
				continue
			}
			k = to
		}
		attrs[k] = attrValue(v)
	}
}

// attrValue renders a JSON value as an attribute: strings unquoted, the
// rest compact JSON.
func attrValue(v json.RawMessage) string {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		return string(v)
	}
	return buf.String()
}

// setExit records how dispatch or the process ended on the root.
func setExit(attrs map[string]string, r tapRec) {
	if r.RC != nil {
		attrs["rc"] = strconv.Itoa(*r.RC)
	}
	if r.Exc != "" {
		attrs["exc"], attrs["exc_msg"] = r.Exc, r.ExcMsg
	}
}

func rootStatus(attrs map[string]string, procEnded bool, end procEnd) (string, string) {
	switch {
	case attrs["rc"] != "" && attrs["rc"] != "0":
		if attrs["exc"] != "" {
			return "ERROR", attrs["exc"] + ": " + attrs["exc_msg"]
		}
		return "ERROR", "exit status " + attrs["rc"]
	case procEnded:
		return "OK", ""
	case end == endOpen:
		return "", "still running when the recording ended"
	case end == endKilled:
		return "ERROR", "killed before it exited (no proc_end)"
	case end == endCut:
		return "", "its records stop where its unit probe session ended"
	}
	return "", "" // the tap's process records were off: no exit status
}

func childStatus(n *opsNode) (string, string) {
	a := n.row.Attrs
	switch {
	case a["exc"] != "":
		return "ERROR", a["exc"] + ": " + a["exc_msg"]
	case n.kind == OpsTool && a["rc"] != "" && a["rc"] != "0":
		return "ERROR", "exit status " + a["rc"]
	case n.kind == OpsPebble && len(a["status"]) == 3 && a["status"] >= "400":
		return "ERROR", "HTTP " + a["status"]
	case a["unfinished"] == "true":
		return "", ""
	}
	return "OK", ""
}

// opsTraceID derives a process's trace id from its spool file, so
// rebuilding from raw/ gives the same ids.
func opsTraceID(k opsKey) string {
	h := fnv.New128a()
	h.Write([]byte("ops\x00" + k.model + "\x00" + k.host + "\x00" + k.file))
	return hex.EncodeToString(h.Sum(nil))
}

// opsSpanID is the id of the seq-th span built for a process (0: the root).
func opsSpanID(k opsKey, seq int) string {
	h := fnv.New64a()
	h.Write([]byte("ops\x00" + k.model + "\x00" + k.host + "\x00" + k.file + "\x00"))
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(seq))
	h.Write(buf[:])
	return hex.EncodeToString(h.Sum(nil))
}

// appOf extracts the application from a unit name ("grafana/0" -> "grafana").
func appOf(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}
