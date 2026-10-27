package narrative

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// This file turns the ops spans the tap recorded (internal/recording/ops.go)
// back into one tree per hook process, for the viewer's inspector and the
// export: charm init, the deferred backlog, each event in order, the
// observers it reached, and the hook tool and Pebble calls they made. The
// tree is format-agnostic; OpsLines lays it out as indented rows both
// consumers render, one with lipgloss styles and one as plain text.

// Marks on an OpsNode.
const (
	MarkDeferred   = "deferred"   // an observer deferred the event (on the event and on that observer)
	MarkReemitted  = "re-emitted" // a deferred event re-emitted from the backlog (on the event and its observers)
	MarkUnfinished = "unfinished" // the process ended, or the recording did, before this span did
	MarkRunning    = "running"    // root: the process was still running when the recording ended
	MarkKilled     = "killed"     // root: the process exited without the tap's proc_end (SIGKILL, OOM)
)

// OpsNode is one span of a hook process's tree. Error is the exception or
// failed exit the span ended with; a failure shows on every span it passed
// through, from the observer that raised to the root.
type OpsNode struct {
	Kind       string            `json:"kind"` // recording.OpsRoot, OpsInit, OpsBacklog, OpsEvent, OpsObserver, OpsTool or OpsPebble
	Name       string            `json:"name"`
	SpanID     string            `json:"span_id"`
	Start      time.Time         `json:"start"`
	Dur        time.Duration     `json:"-"`
	DurationMS float64           `json:"duration_ms"`
	Error      string            `json:"error,omitempty"`
	Marks      []string          `json:"marks,omitempty"`
	Attrs      map[string]string `json:"attrs,omitempty"` // every field the tap recorded for the span
	Cost       *OpsCost          `json:"cost,omitempty"`  // set by AttachCosts
	// Funcs is what the charm's own code did in an init or observer span, from
	// the stack samples (AttachCosts): the functions that ran and how long they
	// took. FuncsRestMS is the time not in any of them: ops, libraries, imports.
	Funcs []*FuncCall `json:"functions,omitempty"`
	// Ctor is the charm's constructor, when the samples show init is almost
	// all of it: charm init is shown as that function, with Funcs its calls.
	Ctor        string     `json:"constructor,omitempty"`
	CtorAt      string     `json:"constructor_at,omitempty"`
	CtorMem     int64      `json:"constructor_mem_growth_bytes,omitempty"` // memory the samples saw it take, counted as for the functions under it
	FuncsRestMS float64    `json:"outside_functions_ms,omitempty"`
	Children    []*OpsNode `json:"children,omitempty"`
}

// OpsIndex finds the span tree of a hook process among a model's spans.
type OpsIndex struct {
	byID     map[string]recording.SpanRow
	children map[string][]string // parent span id -> child span ids, by start
}

// NewOpsIndex indexes the ops spans among spans.
func NewOpsIndex(spans []recording.SpanRow) *OpsIndex {
	x := &OpsIndex{byID: map[string]recording.SpanRow{}, children: map[string][]string{}}
	var ops []recording.SpanRow
	for _, sp := range spans {
		if sp.Service == recording.ServiceOps {
			ops = append(ops, sp)
		}
	}
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].Start.Before(ops[j].Start) })
	for _, sp := range ops {
		x.byID[sp.SpanID] = sp
		if sp.ParentSpanID != "" {
			x.children[sp.ParentSpanID] = append(x.children[sp.ParentSpanID], sp.SpanID)
		}
	}
	return x
}

// Trees returns the tree of each root span (HookRun.OpsSpans: one per
// attempt), in the order given. The processes a hook started nest under it.
// A hook the tap missed has a root that stands in for it, with nothing
// recorded under it: it has no tree.
func (x *OpsIndex) Trees(roots []string) []*OpsNode {
	var out []*OpsNode
	for _, id := range roots {
		if sp, ok := x.byID[id]; ok && sp.Attrs["tap"] != recording.TapMissing {
			out = append(out, x.node(id))
		}
	}
	return out
}

func (x *OpsIndex) node(id string) *OpsNode {
	sp := x.byID[id]
	kind := sp.Attrs["kind"]
	n := &OpsNode{
		Kind: kind, Name: sp.Name, SpanID: sp.SpanID, Start: sp.Start,
		Attrs: make(map[string]string, len(sp.Attrs)),
	}
	for k, v := range sp.Attrs {
		if k != "kind" {
			n.Attrs[k] = v
		}
	}
	if d := sp.End.Sub(sp.Start); d > 0 {
		n.Dur = d
	}
	n.DurationMS = float64(n.Dur.Microseconds()) / 1000
	if sp.StatusCode == "ERROR" {
		n.Error = sp.StatusMsg
	}
	a := sp.Attrs
	switch {
	case kind == recording.OpsEvent && a["reemitted"] == "true",
		kind == recording.OpsObserver && a["reemit"] == "true":
		n.Marks = append(n.Marks, MarkReemitted)
	}
	if a["deferred"] == "true" {
		n.Marks = append(n.Marks, MarkDeferred)
	}
	if a["unfinished"] == "true" {
		n.Marks = append(n.Marks, MarkUnfinished)
	}
	if kind == recording.OpsRoot {
		if a["open"] == "true" {
			n.Marks = append(n.Marks, MarkRunning)
		}
		if a["truncated"] == "true" {
			n.Marks = append(n.Marks, MarkKilled)
		}
	}
	var subs []*OpsNode
	for _, c := range x.children[id] {
		child := x.node(c)
		if hasMark(n.Marks, MarkReemitted) && child.Kind == recording.OpsObserver {
			child.Marks = dropMark(child.Marks, MarkReemitted) // said once, on the event
		}
		if child.Kind == recording.OpsRoot {
			subs = append(subs, child)
			continue
		}
		n.Children = append(n.Children, child)
	}
	for _, s := range subs {
		nest(n, s)
	}
	return n
}

// nest puts a subprocess's tree under the innermost span of its parent
// process that was running when it started: the observer (or charm init)
// that started it. The tap links a subprocess to its parent process only.
func nest(n, sub *OpsNode) {
	for _, c := range n.Children {
		switch c.Kind {
		case recording.OpsInit, recording.OpsBacklog, recording.OpsEvent, recording.OpsObserver:
			if !sub.Start.Before(c.Start) && sub.Start.Before(c.Start.Add(c.Dur)) {
				nest(c, sub)
				return
			}
		}
	}
	i := sort.Search(len(n.Children), func(i int) bool { return n.Children[i].Start.After(sub.Start) })
	n.Children = append(n.Children, nil)
	copy(n.Children[i+1:], n.Children[i:])
	n.Children[i] = sub
}

func hasMark(marks []string, mark string) bool {
	for _, m := range marks {
		if m == mark {
			return true
		}
	}
	return false
}

func dropMark(marks []string, mark string) []string {
	var out []string
	for _, m := range marks {
		if m != mark {
			out = append(out, m)
		}
	}
	return out
}

// SpanLabel names an ops span the way the tree's rows do: an observer by
// its class and method, the process by the hook it ran ("config-changed
// process"). It is "" for a span that isn't indexed.
func (x *OpsIndex) SpanLabel(id string) string {
	sp, ok := x.byID[id]
	if !ok {
		return ""
	}
	switch sp.Attrs["kind"] {
	case recording.OpsObserver:
		return ShortObserver(sp.Name)
	case recording.OpsInit:
		return "charm __init__"
	case recording.OpsRoot:
		if sp.Name == "" {
			return "process"
		}
		return sp.Name + " process"
	}
	return sp.Name
}

// Names returns the event and observer names in the trees of roots, for
// search: each event's kind and class ("config_changed",
// "ConfigChangedEvent") and each observer's qualified method.
func (x *OpsIndex) Names(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var walk func(id string)
	walk = func(id string) {
		sp := x.byID[id]
		switch sp.Attrs["kind"] {
		case recording.OpsEvent:
			add(sp.Name)
			add(sp.Attrs["ev_class"])
		case recording.OpsObserver:
			add(sp.Name)
		}
		for _, c := range x.children[id] {
			walk(c)
		}
	}
	for _, id := range roots {
		walk(id)
	}
	return out
}

// Kinds of OpsLine that stand for spans folded away in the compact layout.
const (
	OpsLineCalls  = "calls"    // hook tool and Pebble calls folded into one line
	OpsLineEvents = "events"   // events nothing observed, folded into one line
	OpsLineFunc   = "function" // a function of the charm's code that ran in an init or observer span
	OpsLineRest   = "rest"     // the time of such a span not in any function listed
)

// opsMaxFuncLines bounds the function rows under one init or observer span.
const opsMaxFuncLines = 14

// slowCall is how long a hook tool or Pebble call must take to keep its own
// line in the compact layout.
const slowCall = 50 * time.Millisecond

// OpsLine is one row of a laid-out tree. Kind is the span's kind, or
// OpsLineCalls / OpsLineEvents for a folded summary.
type OpsLine struct {
	Depth  int
	Kind   string
	Label  string
	Dur    time.Duration
	HasDur bool
	Loc    string   // for a function: where it is, "charm.py:93"; shown next to its name
	Detail string   // exit code, call arguments, share of the hook, time outside calls
	Marks  []string // the node's marks
	Failed bool     // the span ended in an error
	Error  string   // the error on one line, cut to 200 characters; on the innermost span it passed through only
	Cost   *OpsCost // what the span cost, when measured (AttachCosts)
}

// OpsLines lays out the trees of one hook run (one per attempt) as indented
// rows. With all unset the layout is compact: each hook tool or Pebble call
// keeps its own row only when it failed (hook tools), set a status, changed
// something in Pebble, or took at least 50ms; the rest of a span's calls fold
// into one row with counts. Events no observer handled fold the same way,
// and an empty deferred-events backlog is left out. With all set, every
// span gets its row.
func OpsLines(trees []*OpsNode, all bool) []OpsLine {
	var out []OpsLine
	for i, root := range trees {
		ln := lineFor(root, 0, nil)
		if len(trees) > 1 {
			ln.Label = fmt.Sprintf("attempt %d of %d: %s", i+1, len(trees), ln.Label)
			ln.Detail = joinNonEmpty(root.Start.UTC().Format("15:04:05.000"), ln.Detail)
		}
		out = append(out, ln)
		out = appendChildren(out, root, root, 1, all)
	}
	return out
}

func appendChildren(out []OpsLine, n, root *OpsNode, depth int, all bool) []OpsLine {
	var calls, quiet []*OpsNode
	for _, c := range n.Children {
		if !all {
			switch {
			case isCall(c) && !keepCall(c):
				calls = append(calls, c)
				continue
			case c.Kind == recording.OpsEvent && len(c.Children) == 0 && len(c.Marks) == 0 && c.Error == "":
				quiet = append(quiet, c)
				continue
			case c.Kind == recording.OpsBacklog && len(c.Children) == 0 && c.Error == "":
				continue
			}
		}
		out = append(out, lineFor(c, depth, root))
		out = appendFuncs(out, c, depth+1)
		out = appendChildren(out, c, root, depth+1, all)
	}
	if len(calls) > 0 {
		out = append(out, callsLines(calls, depth)...)
	}
	if len(quiet) > 0 {
		names := make([]string, len(quiet))
		for i, q := range quiet {
			names[i] = q.Name
		}
		out = append(out, OpsLine{
			Depth: depth, Kind: OpsLineEvents,
			Label:  fmt.Sprintf("%d %s without observers", len(quiet), plural(len(quiet), "event", "events")),
			Detail: strings.Join(names, ", "),
		})
	}
	return out
}

func isCall(n *OpsNode) bool { return n.Kind == recording.OpsTool || n.Kind == recording.OpsPebble }

// keepCall reports whether a call keeps its own row in the compact layout:
// a hook tool that failed, set a status or was slow. Pebble calls always fold
// into one row, with how many failed; the functions listed under init and
// observers say what they were for, and `c` lists every call.
func keepCall(n *OpsNode) bool {
	return n.Kind == recording.OpsTool && (n.Dur >= slowCall || n.Name == "status-set" || n.Error != "")
}

// callsLines folds calls into rows: one for the hook tool calls and one for
// the Pebble calls, each with how many, their total time, and the most
// frequent names.
func callsLines(calls []*OpsNode, depth int) []OpsLine {
	var tools, pebble []*OpsNode
	for _, c := range calls {
		if c.Kind == recording.OpsPebble {
			pebble = append(pebble, c)
		} else {
			tools = append(tools, c)
		}
	}
	var out []OpsLine
	if len(tools) > 0 {
		out = append(out, callsLine(tools, depth, "call"))
	}
	if len(pebble) > 0 {
		out = append(out, callsLine(pebble, depth, "Pebble call"))
	}
	return out
}

func callsLine(calls []*OpsNode, depth int, noun string) OpsLine {
	type group struct {
		name          string
		count, failed int
	}
	var groups []*group
	byName := map[string]*group{}
	var total time.Duration
	for _, c := range calls {
		total += c.Dur
		g := byName[c.Name]
		if g == nil {
			g = &group{name: c.Name}
			byName[c.Name] = g
			groups = append(groups, g)
		}
		g.count++
		if c.Error != "" {
			g.failed++
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].count > groups[j].count })
	var parts []string
	for i, g := range groups {
		if i == 4 {
			parts = append(parts, "…")
			break
		}
		p := g.name
		if g.count > 1 {
			p += " ×" + strconv.Itoa(g.count)
		}
		if g.failed > 0 {
			p += fmt.Sprintf(" (%d failed)", g.failed)
		}
		parts = append(parts, p)
	}
	return OpsLine{
		Depth: depth, Kind: OpsLineCalls, Dur: total, HasDur: true,
		Label:  fmt.Sprintf("%d %s", len(calls), plural(len(calls), noun, noun+"s")),
		Detail: strings.Join(parts, ", "),
	}
}

// appendFuncs lists under an init or observer row the functions of the
// charm's code that ran in it and how long each took, as a call tree, then
// the time that wasn't in any of them.
func appendFuncs(out []OpsLine, n *OpsNode, depth int) []OpsLine {
	if len(n.Funcs) == 0 {
		return out
	}
	shown, total := 0, 0
	var walk func(fs []*FuncCall, depth int)
	walk = func(fs []*FuncCall, depth int) {
		for _, f := range fs {
			total++
			if shown >= opsMaxFuncLines {
				walk(f.Calls, depth+1)
				continue
			}
			shown++
			var marks []string
			if f.Waiting {
				marks = append(marks, "mostly "+orWaiting(f.WaitingOn))
			}
			out = append(out, OpsLine{
				Depth: depth, Kind: OpsLineFunc, Label: f.Name,
				Dur: time.Duration(f.WallMS * float64(time.Millisecond)), HasDur: true,
				Loc: path.Base(f.At), Marks: marks,
				Cost: &OpsCost{Profiled: true, Samples: 1, CPUMS: f.CPUMS, StackMemGrowthBytes: f.MemGrowthBytes, RSSSource: RSSFromStacks},
			})
			walk(f.Calls, depth+1)
		}
	}
	walk(n.Funcs, depth)
	if total > shown {
		out = append(out, OpsLine{Depth: depth, Kind: OpsLineRest,
			Label: fmt.Sprintf("… %d more %s (p lists them all)", total-shown, plural(total-shown, "function", "functions"))})
	}
	if n.FuncsRestMS >= 20 {
		out = append(out, OpsLine{
			Depth: depth, Kind: OpsLineRest, Dur: time.Duration(n.FuncsRestMS * float64(time.Millisecond)), HasDur: true,
			Label: "time outside these functions", Detail: "ops and libraries",
		})
	}
	return out
}

// lineFor is a node's own row. root is the process the node belongs to,
// for charm init's share of it; nil for a root.
func lineFor(n *OpsNode, depth int, root *OpsNode) OpsLine {
	a := n.Attrs
	ln := OpsLine{
		Depth: depth, Kind: n.Kind, Label: n.Name, Dur: n.Dur, HasDur: true,
		Marks: n.Marks, Failed: n.Error != "", Cost: n.Cost,
	}
	if ln.Failed && !childFailedWith(n, n.Error) {
		ln.Error = truncateRunes(strings.Join(strings.Fields(n.Error), " "), 200)
	}
	switch n.Kind {
	case recording.OpsRoot:
		if rc := a["rc"]; rc != "" {
			ln.Detail = "exit " + rc
		}
		if n.Name == "" {
			ln.Label = "process"
		}
	case recording.OpsInit:
		// Init isn't a function: it is the charm's constructor, and the time
		// ops takes to set up around it.
		ln.Label = "charm __init__"
		if n.Ctor != "" {
			ln.Label, ln.Loc = n.Ctor, path.Base(n.CtorAt)
			if n.Cost != nil {
				// Memory taken by the functions under it is counted as the samples
				// saw it, so the constructor's figure must be too.
				c := *n.Cost
				c.RSSSource, c.RSSStartBytes, c.RSSEndBytes = RSSFromSamples, 0, n.CtorMem
				ln.Cost = &c
			}
		}
		var parts []string
		if root != nil && root.Dur > 0 {
			parts = append(parts, fmt.Sprintf("%d%% of the hook", n.Dur*100/root.Dur))
		}
		ln.Detail = joinNonEmpty(append(parts, selfTime(n))...)
	case recording.OpsBacklog:
		if c := a["count"]; c != "" {
			ln.Label = fmt.Sprintf("%s (%s)", n.Name, c)
		}
	case recording.OpsObserver:
		ln.Label = ShortObserver(n.Name)
		ln.Detail = selfTime(n)
	case recording.OpsTool:
		ln.Label = toolLabel(n)
	case recording.OpsPebble:
		ln.Label = pebbleLabel(n)
	}
	return ln
}

// childFailedWith reports whether a child of n failed with err: the error
// then shows on the innermost span, where it was raised.
func childFailedWith(n *OpsNode, err string) bool {
	for _, c := range n.Children {
		if c.Error == err {
			return true
		}
	}
	return false
}

// selfTime is the time a span spent outside the spans under it, when that
// is a noticeable part of it: Python the tap doesn't see into.
func selfTime(n *OpsNode) string {
	if len(n.Children) == 0 {
		return ""
	}
	self := n.Dur
	for _, c := range n.Children {
		self -= c.Dur
	}
	if self < 10*time.Millisecond || self*10 < n.Dur {
		return ""
	}
	return FormatOpsDur(self) + " of it Python code, not calls"
}

// ShortObserver drops the module from an observer's qualified name:
// "charms.loki_k8s.v1.loki_push_api.LokiPushApiConsumer._on_lifecycle_event"
// reads "LokiPushApiConsumer._on_lifecycle_event".
func ShortObserver(obs string) string {
	parts := strings.Split(obs, ".")
	if len(parts) <= 2 {
		return obs
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// toolLabel is a hook tool call with its arguments, which the tap already
// cut down to keys and byte sizes; status-set reads as the status it set.
func toolLabel(n *OpsNode) string {
	a := n.Attrs
	if n.Name == "status-set" && a["status"] != "" {
		s := "status-set "
		if a["app"] == "true" {
			s += "(app) "
		}
		s += a["status"]
		if a["message"] != "" {
			s += fmt.Sprintf(" %q", a["message"])
		}
		return truncateRunes(s, 60)
	}
	var args []string
	_ = json.Unmarshal([]byte(a["args"]), &args)
	var keep []string
	for _, arg := range args {
		if arg == "--" || strings.HasPrefix(arg, "--format") {
			continue
		}
		keep = append(keep, arg)
	}
	return truncateRunes(strings.TrimSpace(n.Name+" "+strings.Join(keep, " ")), 60)
}

// pebbleLabel is a Pebble call with what it acted on: the command run, the
// layer or service action, or the file path.
func pebbleLabel(n *OpsNode) string {
	a := n.Attrs
	var extra string
	switch {
	case a["command"] != "":
		extra = a["command"]
	case a["action"] != "":
		extra = a["action"]
		var svcs []string
		if json.Unmarshal([]byte(a["services"]), &svcs) == nil && len(svcs) > 0 {
			extra += " " + strings.Join(svcs, ",")
		}
	case a["query"] != "":
		var q map[string]any
		if json.Unmarshal([]byte(a["query"]), &q) == nil {
			if p, ok := q["path"].(string); ok {
				extra = p
			}
		}
	}
	return truncateRunes(strings.TrimSpace(n.Name+" "+extra), 60)
}

// FormatOpsDur formats a span's duration: tenths of a millisecond below
// 10ms, milliseconds below a second, then seconds.
func FormatOpsDur(d time.Duration) string {
	switch {
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// OpsLabelWidth is the column a tree's durations line up at: the widest
// indented label, up to max.
func OpsLabelWidth(lines []OpsLine, max int) int {
	w := 0
	for _, l := range lines {
		n := 2*l.Depth + len([]rune(l.Label))
		if l.Loc != "" {
			n += 2 + len([]rune(l.Loc))
		}
		if n > w {
			w = n
		}
	}
	if w > max {
		w = max
	}
	return w
}

// FormatOpsLines renders lines as plain text, durations in a column. When
// some lines have a cost, its columns follow the duration, under a header.
func FormatOpsLines(lines []OpsLine) []string {
	w := OpsLabelWidth(lines, 64)
	costs := OpsHasCost(lines)
	out := make([]string, 0, len(lines)+1)
	if costs {
		out = append(out, strings.TrimRight(OpsCostHeader(w), " "))
	}
	for _, l := range lines {
		s := strings.Repeat("  ", l.Depth) + l.Label
		if l.Loc != "" {
			s += "  " + l.Loc
		}
		if pad := w - len([]rune(s)); pad > 0 {
			s += strings.Repeat(" ", pad)
		}
		if l.HasDur {
			s += fmt.Sprintf("  %7s", FormatOpsDur(l.Dur))
		} else {
			s += strings.Repeat(" ", 9)
		}
		if costs {
			s += OpsCostColumns(l)
		}
		var tail []string
		if l.Detail != "" {
			tail = append(tail, l.Detail)
		}
		tail = append(tail, l.Marks...)
		switch {
		case l.Error != "":
			tail = append(tail, "✗ "+l.Error)
		case l.Failed:
			tail = append(tail, "✗")
		}
		if len(tail) > 0 {
			s += "  " + strings.Join(tail, " · ")
		}
		out = append(out, strings.TrimRight(s, " "))
	}
	return out
}

func joinNonEmpty(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// orWaiting is what a function was waiting for, or just "waiting".
func orWaiting(what string) string {
	if what == "" {
		return "waiting"
	}
	return what
}
