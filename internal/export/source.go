package export

import (
	"fmt"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// Source is an opened recording, indexed and with its events derived: what
// both Build and the inspect commands query. Opening it loads every span
// once; asking it for one event's detail or cost then touches only that
// event. Close releases the index.
type Source struct {
	Recording RecordingInfo
	Model     index.Model

	db       *index.DB
	spans    []recording.SpanRow
	spanByID map[string]recording.SpanRow
	events   []narrative.Event
	ops      *narrative.OpsIndex
}

// OpenSource opens the index of the recording at dir, which must exist
// already, and derives the events of one model: the one named, or the only
// one the recording has.
func OpenSource(dir, model string) (*Source, error) {
	man, err := recording.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("loading manifest at %s: %w", dir, err)
	}
	db, err := index.Open(recording.NewLayout(dir).IndexDB())
	if err != nil {
		return nil, fmt.Errorf("opening index (run `juju-lens index %s`): %w", dir, err)
	}
	mm, err := resolveModel(db, model)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	spans, err := db.Spans(mm.ID)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("loading spans: %w", err)
	}
	s := &Source{
		Recording: RecordingInfo{Dir: dir, Controller: man.Controller.Name, Started: man.Started, Ended: man.Ended},
		Model:     mm,
		db:        db,
		spans:     spans,
		spanByID:  make(map[string]recording.SpanRow, len(spans)),
		ops:       narrative.NewOpsIndex(spans),
	}
	for _, sp := range spans {
		s.spanByID[sp.SpanID] = sp
	}
	databagSpans, _ := db.ProducingSpanIDs(mm.ID, string(index.KindDatabag))
	unitStatusBySpan, _ := db.StatusBySpan(mm.ID, string(index.KindUnitStatus))
	appStatusBySpan, _ := db.StatusBySpan(mm.ID, string(index.KindAppStatus))
	s.events = narrative.BuildEventsWithStatus(spans, databagSpans, narrative.StatusMapFromSnapshots(unitStatusBySpan, appStatusBySpan))
	return s, nil
}

// Close releases the index.
func (s *Source) Close() error { return s.db.Close() }

// Events returns the model's events in the default view (hooks and settle
// markers, not the verbose-only RPC transitions), narrowed to opts' units
// and time window, in time order.
func (s *Source) Events(opts Options) []narrative.Event {
	return filterEvents(s.events, unitSet(opts.Units), opts.Since, opts.Until)
}

// Event finds one event by its span id.
func (s *Source) Event(spanID string) (narrative.Event, bool) {
	for _, ev := range s.events {
		if ev.SpanID == spanID && !ev.VerboseOnly {
			return ev, true
		}
	}
	return narrative.Event{}, false
}

// Detail is everything the export knows of ev: the details/<span_id>.json.
func (s *Source) Detail(ev narrative.Event) EventDetail {
	return buildEventDetail(s.db, s.Model.ID, ev, s.spanByID[ev.SpanID], s.ops, false)
}

// HookCost is what the charm's init and observers cost in one hook, added
// up, and where in the charm's code the time and memory went. Profiled is
// false for a hook with no stack samples: its other fields are then the
// resident-memory readings alone, or zero.
type HookCost struct {
	Profiled    bool    `json:"profiled"`
	CPUMS       float64 `json:"cpu_ms,omitempty"`
	MemGrowth   int64   `json:"mem_growth_bytes,omitempty"`
	RSSPeak     int64   `json:"rss_peak_bytes,omitempty"`
	TopFrame    string  `json:"top_frame,omitempty"`
	TopMemFrame string  `json:"top_memory_frame,omitempty"`
}

// Cost is the cost of ev without the rest of its detail, which is
// cheaper to ask for many events.
func (s *Source) Cost(ev narrative.Event) HookCost {
	if len(ev.OpsSpans) == 0 {
		return HookCost{}
	}
	d := EventDetail{Ops: s.ops.Trees(ev.OpsSpans)}
	addCosts(s.db, &d, s.ops)
	c := d.cost()
	hc := HookCost{
		CPUMS: c.cpuMS, MemGrowth: c.memGrowth, RSSPeak: c.rssPeak,
		TopFrame: topFrame(d.TopStacks), TopMemFrame: topFrame(d.TopMemStacks),
	}
	for _, sc := range d.Costs {
		if sc.Profiled {
			hc.Profiled = true
		}
	}
	return hc
}

// Profile gathers the stack samples of every process that ran the events,
// as one profile, with each stack tagged by the span it ran in.
func (s *Source) Profile(events []narrative.Event) (*narrative.Profile, error) {
	var roots []string
	for _, ev := range events {
		roots = append(roots, narrative.ProcessRoots(s.ops.Trees(ev.OpsSpans))...)
	}
	profs, err := s.db.Profiles(roots)
	if err != nil {
		return nil, err
	}
	return narrative.NewProfile(profs, nil), nil
}

// SpanLabel names an ops span as the trace does: an observer by class and
// method, charm init as "charm __init__".
func (s *Source) SpanLabel(id string) string { return s.ops.SpanLabel(id) }

// SpanEvent finds the event whose hook process contains the ops span with
// this id, so a span id from a profile can be traced back to its hook.
func (s *Source) SpanEvent(spanID string) (narrative.Event, bool) {
	sp, ok := s.spanByID[spanID]
	if !ok {
		return narrative.Event{}, false
	}
	for sp.ParentSpanID != "" {
		next, ok := s.spanByID[sp.ParentSpanID]
		if !ok {
			break
		}
		sp = next
	}
	for _, ev := range s.events {
		for _, root := range ev.OpsSpans {
			if root == sp.SpanID {
				return ev, true
			}
		}
	}
	return narrative.Event{}, false
}

// SpanProfile is the profile of one span: a whole hook when spanID is the
// event's span id, or charm init or an observer (or any ops span, such as one
// of the span ids in top_stacks) and the spans under it. It returns the event
// the span belongs to and a label for the span.
func (s *Source) SpanProfile(spanID string) (*narrative.Profile, narrative.Event, string, error) {
	if ev, ok := s.Event(spanID); ok {
		p, err := s.Profile([]narrative.Event{ev})
		return p, ev, ev.Unit + " " + ev.Summary, err
	}
	ev, ok := s.SpanEvent(spanID)
	if !ok {
		return nil, narrative.Event{}, "", fmt.Errorf("no event or span with id %q in this recording", spanID)
	}
	trees := s.ops.Trees(ev.OpsSpans)
	var found *narrative.OpsNode
	var walk func(n *narrative.OpsNode)
	walk = func(n *narrative.OpsNode) {
		if n.SpanID == spanID {
			found = n
			return
		}
		for _, c := range n.Children {
			if found == nil {
				walk(c)
			}
		}
	}
	for _, t := range trees {
		walk(t)
	}
	if found == nil {
		return nil, ev, "", fmt.Errorf("span %q is in the hook %s %s but not in its charm-side tree", spanID, ev.Unit, ev.Summary)
	}
	profs, err := s.db.Profiles(narrative.ProcessRoots(trees))
	if err != nil {
		return nil, ev, "", err
	}
	label := ev.Unit + " " + ev.Summary + ", " + s.ops.SpanLabel(spanID)
	return narrative.NewProfile(profs, narrative.SubtreeSpans(found)), ev, label, nil
}

// Memory reads the memory around ev: the unit's cgroups, the charm
// process's resident memory, the hooks and the Pebble changes near it.
func (s *Source) Memory(ev narrative.Event) (*narrative.Memory, error) {
	return narrative.LoadMemory(s.db, s.Model.ID, s.spans, ev)
}

// Trace lays out one process of the hook ev (attempt counts from 1) along
// time: its spans, slow calls, and the functions of the charm's code. It
// returns how many processes ran the hook, and the name of this one.
func (s *Source) Trace(ev narrative.Event, attempt int, o narrative.TraceOptions) (tr *narrative.Trace, attempts int, hook string, err error) {
	trees := s.ops.Trees(ev.OpsSpans)
	if len(trees) == 0 {
		return nil, 0, "", fmt.Errorf("no charm-side records for %s %s: they come from a recording made with --attach unit", ev.Unit, ev.Summary)
	}
	if attempt < 1 || attempt > len(trees) {
		return nil, len(trees), "", fmt.Errorf("--attempt %d: the hook ran in %d %s", attempt, len(trees), pluralWord(len(trees), "process", "processes"))
	}
	profs, err := s.db.Profiles(narrative.ProcessRoots(trees))
	if err != nil {
		return nil, 0, "", err
	}
	narrative.AttachCosts(trees, profs)
	return narrative.BuildTrace(trees[attempt-1], profs, o), len(trees), trees[attempt-1].Name, nil
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
