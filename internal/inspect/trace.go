package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/query"
)

// TraceQuery asks for the trace of one process of a hook.
type TraceQuery struct {
	Attempt     int // which process, from 1; 0 is the first
	Detail      narrative.Detail
	All         bool          // list every span, call and function, however short
	Depth       int           // at most this many levels below the process; 0 for all
	MinDuration time.Duration // leave out rows shorter than this
	Limit       int           // at most this many rows; 0 for all
}

// TraceRowJSON is a row of a trace.
type TraceRowJSON struct {
	Depth       int      `json:"depth"`
	Kind        string   `json:"kind"`
	Label       string   `json:"label"`
	Loc         string   `json:"loc,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	SpanID      string   `json:"span_id,omitempty"`
	StartMS     float64  `json:"start_ms"` // from the start of the process
	DurationMS  float64  `json:"duration_ms"`
	CPUMS       float64  `json:"cpu_ms,omitempty"`
	MemBytes    int64    `json:"mem_bytes,omitempty"` // memory the row took, when known
	WaitingOn   string   `json:"waiting_on,omitempty"`
	Error       string   `json:"error,omitempty"`
	Marks       []string `json:"marks,omitempty"`
	HiddenBelow int      `json:"short_rows_left_out,omitempty"`
}

// TraceReport is the trace of one process of a hook.
type TraceReport struct {
	Recording  string         `json:"recording"`
	Model      string         `json:"model"`
	SpanID     string         `json:"span_id"`
	Unit       string         `json:"unit"`
	Event      string         `json:"event"`
	Attempt    int            `json:"attempt"`
	Attempts   int            `json:"attempts"`
	Process    string         `json:"process"`
	DurationMS float64        `json:"duration_ms"`
	ImportMS   float64        `json:"importing_python_ms,omitempty"`
	Total      int            `json:"total_rows"`
	Rows       []TraceRowJSON `json:"rows"`

	rec string
}

// TraceOf builds the trace of ev.
func TraceOf(src *query.Source, rec string, ev narrative.Event, q TraceQuery) (*TraceReport, error) {
	if q.Attempt < 1 {
		return nil, fmt.Errorf("--attempt counts from 1")
	}
	tr, attempts, hook, err := src.Trace(ev, q.Attempt, narrative.TraceOptions{Detail: q.Detail, All: q.All})
	if err != nil {
		return nil, err
	}
	rep := &TraceReport{
		Recording: src.Recording.Dir, Model: src.Model.Name, SpanID: ev.SpanID, Unit: ev.Unit, Event: ev.Summary,
		Attempt: q.Attempt, Attempts: attempts, Process: hook,
		DurationMS: ms(tr.To.Sub(tr.From)), ImportMS: tr.ImportMS, Rows: []TraceRowJSON{}, rec: rec,
	}
	limit := q.Limit
	if limit == 0 {
		limit = int(^uint(0) >> 1)
	}
	for _, r := range tr.Rows {
		if q.Depth > 0 && r.Depth > q.Depth {
			continue
		}
		if q.MinDuration > 0 && r.Dur() < q.MinDuration {
			continue
		}
		rep.Total++
		if len(rep.Rows) >= limit {
			continue
		}
		row := TraceRowJSON{
			Depth: r.Depth, Kind: r.Kind, Label: r.Label, Loc: r.Loc, Detail: r.Detail, SpanID: r.SpanID,
			StartMS: ms(r.Start.Sub(tr.From)), DurationMS: ms(r.Dur()), CPUMS: r.CPUMS,
			WaitingOn: r.WaitingOn, Error: r.Error, Marks: r.Marks, HiddenBelow: r.Hidden,
		}
		if v, ok := r.Mem(); ok {
			row.MemBytes = v
		}
		rep.Rows = append(rep.Rows, row)
	}
	return rep, nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func fmtMS(v float64) string {
	return narrative.FormatOpsDur(time.Duration(v * float64(time.Millisecond)))
}

// WriteText prints the trace, one row a line, indented by depth.
func (t *TraceReport) WriteText(w io.Writer) {
	fmt.Fprintf(w, "trace of %s %s (span %s), process %d of %d (%s), %s\n", t.Unit, t.Event, t.SpanID,
		t.Attempt, t.Attempts, t.Process, fmtMS(t.DurationMS))
	if t.ImportMS > 0 {
		fmt.Fprintf(w, "%s of it importing Python modules before ops started, which the trace leaves out\n", fmtMS(t.ImportMS))
	}
	fmt.Fprintf(w, "\n%8s %8s %8s %8s  %s\n", "START", "TOOK", "CPU", "MEMORY", "SPAN OR FUNCTION")
	for _, r := range t.Rows {
		cpu, mem := "", ""
		if r.CPUMS > 0 {
			cpu = fmtMS(r.CPUMS)
		}
		if r.MemBytes >= 1<<20 || r.MemBytes <= -(1<<20) {
			mem = narrative.FormatBytes(r.MemBytes)
			if r.MemBytes > 0 {
				mem = "+" + mem
			}
		}
		label := r.Label
		if r.Loc != "" {
			label += " (" + r.Loc + ")"
		}
		var notes []string
		if r.Detail != "" {
			notes = append(notes, r.Detail)
		}
		if r.WaitingOn != "" {
			notes = append(notes, "waiting: "+r.WaitingOn)
		}
		if r.Error != "" {
			notes = append(notes, "error: "+r.Error)
		}
		notes = append(notes, r.Marks...)
		line := fmt.Sprintf("%8s %8s %8s %8s  %s%s", fmtMS(r.StartMS), fmtMS(r.DurationMS), cpu, mem, strings.Repeat("  ", r.Depth), label)
		if len(notes) > 0 {
			line += "  · " + strings.Join(notes, " · ")
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
		if r.HiddenBelow > 0 {
			what := "short calls or events"
			if r.HiddenBelow == 1 {
				what = "short call or event"
			}
			fmt.Fprintf(w, "%36s%s%d %s left out (--all shows them)\n", "", strings.Repeat("  ", r.Depth+1), r.HiddenBelow, what)
		}
	}
	if t.Total > len(t.Rows) {
		fmt.Fprintf(w, "\nshowing %d of %d rows; --limit %d shows more, --depth and --min-duration narrow them\n", len(t.Rows), t.Total, t.Total)
	}
	if t.Attempts > 1 {
		fmt.Fprintf(w, "\nthe hook ran in %d processes; --attempt N shows another\n", t.Attempts)
	}
	fmt.Fprintf(w, "next: juju-lens inspect memory %s %s\n", t.rec, t.SpanID)
}

// WriteJSON prints the trace as JSON.
func (t *TraceReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(t)
}
