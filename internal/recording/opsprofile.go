package recording

import (
	"encoding/json"
	"time"
)

// OpsProfile is the stack samples the tap took in one hook process (tap
// feature "stacks"): every 10 ms, the main thread's stack, attributed to
// the span that was running, the innermost of the process's events,
// observers, charm init or the process itself.
type OpsProfile struct {
	RootSpanID string // the process's ops root span
	Model      string
	Unit       string
	Start      time.Time // when sampling started, as Python started
	IntervalNs int64     // 0 when the process never wrote stacks_end
	// Samples is how many samples were taken; Dropped of them are only on
	// the Timeline, with stack -1, because the stack table was full.
	Samples, Dropped int
	RSSStart         int64 // resident memory when sampling started, bytes
	RSSPeak          int64 // the highest of the samples, bytes
	// SamplerCPUNs is the CPU time the sampler itself used: its thread, and
	// its last write at exit.
	SamplerCPUNs int64
	// Complete: the process wrote stacks_end as it exited. A process that
	// was killed, or still ran when the recording ended, keeps what it
	// wrote up to its last stacks record, about a second before.
	Complete bool
	Frames   []ProfileFrame
	Stacks   []ProfileStack // by stack id
	Timeline []ProfileSample
}

// ProfileFrame is one Python function.
type ProfileFrame struct {
	File string
	Func string // qualified name on Python 3.11 and later
	Line int    // the function's first line
}

// ProfileStack is one distinct call stack within one span, and the samples
// that saw it.
type ProfileStack struct {
	SpanID  string
	Frames  []int // indexes into OpsProfile.Frames, innermost first
	Samples int
	WallNs  int64
	CPUNs   int64 // the main thread's CPU time
	// MemBytes is how much resident memory grew between the sample before
	// and each sample with this stack: where memory was taken, not held.
	MemBytes int64
}

// ProfileSample is one sample on the timeline.
type ProfileSample struct {
	Time     time.Time
	Stack    int // index into OpsProfile.Stacks; -1 when it wasn't kept
	RSSBytes int64
}

// profileBuilder gathers a process's stacks records while its tree is
// built; spans are resolved once the tree is complete.
type profileBuilder struct {
	p    OpsProfile
	span []int // the span number of each stack
}

// stacksRec is a stacks record: what is new since the process's previous
// one.
type stacksRec struct {
	M0     int64               `json:"m0"`
	F0     int                 `json:"f0"`
	Frames [][]json.RawMessage `json:"frames"` // [file, func, line]
	S0     int                 `json:"s0"`
	Stacks [][]json.RawMessage `json:"stacks"` // [span, [frame, ...]]
	Totals [][5]int64          `json:"totals"` // [stack, samples, wall ns, cpu ns, memory growth]
	TL     []int64             `json:"tl"`     // µs since m0, stack, RSS in KiB; flattened
}

type stacksEndRec struct {
	Samples    int   `json:"samples"`
	Dropped    int   `json:"dropped"`
	IntervalNs int64 `json:"interval_ns"`
	RSSStart   int64 `json:"rss_start"`
	RSSPeak    int64 `json:"rss_peak"`
	CPUNs      int64 `json:"cpu_ns"`
}

// add folds in one stacks record. Frame and stack ids continue from the
// previous record; a record that went missing leaves placeholder frames
// and stacks in their place, so later ids still line up.
func (b *profileBuilder) add(raw json.RawMessage, tr *opsTree) {
	var r stacksRec
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	if b.p.Start.IsZero() {
		b.p.Start = tr.wall(r.M0)
	}
	if r.F0 >= 0 {
		b.p.Frames = resize(b.p.Frames, r.F0, ProfileFrame{Func: "<lost>"})
	}
	for _, f := range r.Frames {
		var fr ProfileFrame
		if len(f) == 3 {
			_ = json.Unmarshal(f[0], &fr.File)
			_ = json.Unmarshal(f[1], &fr.Func)
			_ = json.Unmarshal(f[2], &fr.Line)
		}
		b.p.Frames = append(b.p.Frames, fr)
	}
	if r.S0 >= 0 {
		b.p.Stacks = resize(b.p.Stacks, r.S0, ProfileStack{})
		b.span = resize(b.span, r.S0, 0)
	}
	for _, s := range r.Stacks {
		var st ProfileStack
		sp := 0
		if len(s) == 2 {
			_ = json.Unmarshal(s[0], &sp)
			_ = json.Unmarshal(s[1], &st.Frames)
		}
		b.p.Stacks = append(b.p.Stacks, st)
		b.span = append(b.span, sp)
	}
	for _, t := range r.Totals {
		if id := int(t[0]); id >= 0 && id < len(b.p.Stacks) {
			st := &b.p.Stacks[id]
			st.Samples += int(t[1])
			st.WallNs += t[2]
			st.CPUNs += t[3]
			st.MemBytes += t[4]
		}
	}
	for i := 0; i+2 < len(r.TL); i += 3 {
		b.p.Timeline = append(b.p.Timeline, ProfileSample{
			Time:     tr.wall(r.M0 + r.TL[i]*1000),
			Stack:    int(r.TL[i+1]),
			RSSBytes: r.TL[i+2] << 10,
		})
	}
}

// end takes the stacks_end record.
func (b *profileBuilder) end(raw json.RawMessage) {
	var r stacksEndRec
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	b.p.Complete = true
	b.p.Samples, b.p.Dropped = r.Samples, r.Dropped
	b.p.IntervalNs = r.IntervalNs
	b.p.RSSStart, b.p.RSSPeak = r.RSSStart, r.RSSPeak
	b.p.SamplerCPUNs = r.CPUNs
}

// finish links each stack to the span its span number names, or to the
// root when that span's record never arrived.
func (b *profileBuilder) finish(root *opsNode, spans map[int]*opsNode) OpsProfile {
	p := b.p
	for i := range p.Stacks {
		n := spans[b.span[i]]
		if n == nil {
			n = root
		}
		p.Stacks[i].SpanID = n.row.SpanID
	}
	p.RootSpanID, p.Model, p.Unit = root.row.SpanID, root.row.Model, root.row.Unit
	if !p.Complete {
		// What the records written so far say.
		p.Samples, p.Dropped = len(p.Timeline), 0
		for _, s := range p.Timeline {
			if s.Stack < 0 {
				p.Dropped++
			}
			p.RSSPeak = max(p.RSSPeak, s.RSSBytes)
		}
	}
	return p
}

// resize makes s exactly n long, padding with fill.
func resize[T any](s []T, n int, fill T) []T {
	if len(s) >= n {
		return s[:n]
	}
	for len(s) < n {
		s = append(s, fill)
	}
	return s
}
