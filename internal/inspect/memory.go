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

// MemoryRow is what happened to one series of readings.
type MemoryRow struct {
	Name         string `json:"name"`
	BeforeBytes  int64  `json:"before_bytes"`
	HighestBytes int64  `json:"highest_bytes"`
	AfterBytes   int64  `json:"after_bytes"`
	LimitBytes   int64  `json:"limit_bytes,omitempty"`
	CacheBytes   int64  `json:"file_cache_bytes,omitempty"` // part of the memory at the end that is page cache
}

// MemoryJump is a stretch over which memory only went up.
type MemoryJump struct {
	Series    string   `json:"series"`
	GainBytes int64    `json:"gain_bytes"`
	FromBytes int64    `json:"from_bytes"`
	ToBytes   int64    `json:"to_bytes"`
	When      string   `json:"when"`             // relative to the hook
	During    string   `json:"during,omitempty"` // for the hook process: the observer or init it ran in
	Code      []string `json:"code,omitempty"`   // the charm's code, innermost first
	Pebble    string   `json:"pebble,omitempty"` // for a container: the Pebble change then
}

// MemoryReport is the memory around one hook.
type MemoryReport struct {
	Recording    string       `json:"recording"`
	Model        string       `json:"model"`
	SpanID       string       `json:"span_id"`
	Unit         string       `json:"unit"`
	Event        string       `json:"event"`
	Host         string       `json:"host"`
	HookStart    time.Time    `json:"hook_start"`
	HookEnd      time.Time    `json:"hook_end"`
	PeakBytes    int64        `json:"charm_container_peak_bytes,omitempty"`
	PeakExact    bool         `json:"charm_container_peak_exact,omitempty"`
	Rows         []MemoryRow  `json:"rows"`
	Jumps        []MemoryJump `json:"jumps,omitempty"`
	LimitReached []string     `json:"limit_events,omitempty"`

	rec string
	m   *narrative.Memory
}

// MemoryOf reads the memory around ev. It fails, saying what to do, for a
// hook with no memory readings.
func MemoryOf(src *query.Source, rec string, ev narrative.Event) (*MemoryReport, error) {
	m, err := src.Memory(ev)
	if err != nil {
		return nil, err
	}
	if m.Empty() {
		return nil, fmt.Errorf("no memory readings for %s %s: they come from the unit probe of a recording made with --attach unit", ev.Unit, ev.Summary)
	}
	r := &MemoryReport{
		Recording: src.Recording.Dir, Model: src.Model.Name, SpanID: ev.SpanID, Unit: ev.Unit, Event: ev.Summary,
		Host: m.Host, HookStart: m.HookStart, HookEnd: m.HookEnd, PeakBytes: m.Peak, PeakExact: m.PeakExact,
		rec: rec, m: m,
	}
	if pts := m.RSS.Points; len(pts) > 0 {
		row := MemoryRow{Name: "hook process", BeforeBytes: pts[0].Total, AfterBytes: pts[len(pts)-1].Total}
		for _, p := range pts {
			row.HighestBytes = max(row.HighestBytes, p.Total)
		}
		r.Rows = append(r.Rows, row)
	}
	for _, s := range m.Series {
		if len(s.Points) == 0 {
			continue
		}
		row := MemoryRow{Name: s.Name + " container", LimitBytes: s.Limit}
		for _, p := range s.Points {
			if !p.T.After(m.HookStart) {
				row.BeforeBytes = p.Total
			}
			if !p.T.Before(m.HookStart) {
				row.HighestBytes = max(row.HighestBytes, p.Total)
			}
		}
		if row.BeforeBytes == 0 {
			row.BeforeBytes = s.Points[0].Total
		}
		last := s.Points[len(s.Points)-1]
		row.AfterBytes = last.Total
		row.HighestBytes = max(row.HighestBytes, row.BeforeBytes, row.AfterBytes)
		if s.Name == "charm" && m.Peak > row.HighestBytes {
			row.HighestBytes = m.Peak
		}
		if last.Split {
			row.CacheBytes = last.File
		}
		r.Rows = append(r.Rows, row)
		if e := s.Events; e != nil && e.HighEvents+e.MaxEvents+e.OOMEvents+e.OOMKillEvents > 0 {
			r.LimitReached = append(r.LimitReached, fmt.Sprintf("%s container reached its memory limit %d times, ran out of memory %d times and had a process killed %d times",
				s.Name, e.HighEvents+e.MaxEvents, e.OOMEvents, e.OOMKillEvents))
		}
	}

	least := max(1<<20, narrative.MaxTotal(m.RSS.Points)/20)
	for _, j := range narrative.Rises(m.Window(m.RSS.Points), time.Second, least) {
		jump := MemoryJump{Series: "hook process", GainBytes: j.Size(), FromBytes: j.From.Total, ToBytes: j.To.Total, When: m.Rel(j.Biggest.T)}
		jump.During, jump.Code = m.StackAt(j.Biggest)
		if strings.HasPrefix(jump.During, "the process starting up") {
			jump.Code = nil
		}
		r.Jumps = append(r.Jumps, jump)
	}
	for _, s := range m.Series {
		least := max(4<<20, narrative.MaxTotal(s.Points)/10)
		for _, j := range narrative.Rises(m.Window(s.Points), 2*time.Second, least) {
			jump := MemoryJump{Series: s.Name + " container", GainBytes: j.Size(), FromBytes: j.From.Total, ToBytes: j.To.Total, When: m.Rel(j.Biggest.T)}
			if c, during, ok := m.ChangeAt(j.From.T, j.To.T); ok {
				jump.Pebble = pebbleWords(c.Container, c.Kind, c.Summary, c.Status, c.Err, c.Ready.Sub(c.Spawn), !c.Ready.IsZero(), during, j.From.T.Sub(c.Spawn))
			}
			r.Jumps = append(r.Jumps, jump)
		}
	}
	return r, nil
}

func pebbleWords(container, kind, summary, status, errText string, took time.Duration, ready, during bool, ago time.Duration) string {
	s := fmt.Sprintf("%s %s: %s (%s", container, kind, summary, status)
	if ready {
		s += ", took " + narrative.FormatOpsDur(took)
	}
	s += ")"
	if errText != "" {
		s += " " + errText
	}
	if during {
		return "during Pebble change " + s
	}
	return "last Pebble change " + narrative.FormatOpsDur(ago) + " before: " + s
}

func changeWords(r MemoryRow) string {
	diff := r.AfterBytes - r.BeforeBytes
	b := narrative.FormatBytes
	switch {
	case diff > -(1<<20) && diff < 1<<20:
		return "stayed around " + b(r.BeforeBytes)
	case diff > 0:
		return fmt.Sprintf("grew from %s to %s (+%s)", b(r.BeforeBytes), b(r.AfterBytes), b(diff))
	}
	return fmt.Sprintf("fell from %s to %s (-%s)", b(r.BeforeBytes), b(r.AfterBytes), b(-diff))
}

// WriteText prints the report.
func (r *MemoryReport) WriteText(w io.Writer) {
	fmt.Fprintf(w, "memory around %s %s (span %s) on %s, model %s\n", r.Unit, r.Event, r.SpanID, r.Host, r.Model)
	fmt.Fprintf(w, "the hook ran %s → %s UTC (%.1f s); readings from 2 s before it until its workloads settled\n\n",
		r.HookStart.UTC().Format("15:04:05.0"), r.HookEnd.UTC().Format("15:04:05.0"), r.HookEnd.Sub(r.HookStart).Seconds())
	for _, row := range r.Rows {
		fmt.Fprintf(w, "- the %s %s", row.Name, changeWords(row))
		if row.HighestBytes-max(row.BeforeBytes, row.AfterBytes) >= 4<<20 {
			fmt.Fprintf(w, ", and was as high as %s in between", narrative.FormatBytes(row.HighestBytes))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n%-24s %8s %8s %8s\n", "", "before", "highest", "after")
	for _, row := range r.Rows {
		fmt.Fprintf(w, "%-24s %8s %8s %8s\n", row.Name, narrative.FormatBytes(row.BeforeBytes), narrative.FormatBytes(row.HighestBytes), narrative.FormatBytes(row.AfterBytes))
	}
	fmt.Fprintln(w, "\nhook process: only the charm's Python process. Containers: everything in them, with the file cache the kernel keeps and can take back.")
	if r.PeakBytes > 0 {
		kind := "polled between samples"
		if r.PeakExact {
			kind = "exact"
		}
		fmt.Fprintf(w, "charm container, highest: the probe's own record of the peak (%s), which can be higher than any single reading.\n", kind)
	}
	for _, row := range r.Rows {
		if row.CacheBytes > 0 {
			fmt.Fprintf(w, "%s: %s of its memory at the end is file cache.\n", row.Name, narrative.FormatBytes(row.CacheBytes))
		}
	}
	section := ""
	for _, j := range r.Jumps {
		if j.Series != section {
			section = j.Series
			if section == "hook process" {
				fmt.Fprintf(w, "\nwhat took memory in the hook process (the biggest jumps; code is innermost first):\n")
			} else {
				fmt.Fprintf(w, "\nwhen the %s grew:\n", section)
			}
		}
		fmt.Fprintf(w, "  +%s  %s → %s  %s\n", narrative.FormatBytes(j.GainBytes), narrative.FormatBytes(j.FromBytes), narrative.FormatBytes(j.ToBytes), j.When)
		if j.During != "" {
			fmt.Fprintf(w, "      during %s\n", j.During)
		}
		if len(j.Code) > 0 {
			fmt.Fprintf(w, "      code: %s\n", strings.Join(j.Code, " ← "))
		}
		if j.Pebble != "" {
			fmt.Fprintf(w, "      %s\n", j.Pebble)
		}
	}
	for _, l := range r.LimitReached {
		fmt.Fprintf(w, "\nthe %s.\n", l)
	}
	fmt.Fprintf(w, "\nnext: juju-lens inspect profile %s --span %s --metric mem tree\n", r.rec, r.SpanID)
}

// WriteJSON prints the report as JSON.
func (r *MemoryReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}
