package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/query"
	"github.com/lucabello/juju-lens/internal/recording"
)

// UnitSummary is what one unit ran in the scope.
type UnitSummary struct {
	Unit     string `json:"unit"`
	Events   int    `json:"events"`
	Problems int    `json:"problems"` // events that didn't end clean
}

// Summary is the shape of a recording, or of part of it.
type Summary struct {
	Recording  string                 `json:"recording"`
	Model      string                 `json:"model"`
	Controller string                 `json:"controller,omitempty"`
	AttachMode string                 `json:"attach_mode,omitempty"` // what the recording captured: unit, host, host,unit or none
	Partial    *recording.ExportScope `json:"partial,omitempty"`     // set when the recording is a part of another
	Started    time.Time              `json:"started"`
	Ended      time.Time              `json:"ended,omitempty"`
	Events     int                    `json:"events"`
	LogLines   int                    `json:"log_lines"`
	Outcomes   map[string]int         `json:"outcomes"`
	Units      []UnitSummary          `json:"units"`
	Slowest    []EventRow             `json:"slowest"`
	Costliest  []EventRow             `json:"costliest,omitempty"` // profiled hooks with the most CPU

	rec string
}

// Summarize describes the events sc picks in src.
func Summarize(src *query.Source, rec string, sc Scope) (*Summary, error) {
	events, err := sc.Events(src)
	if err != nil {
		return nil, err
	}
	logs, err := src.Logs()
	if err != nil {
		return nil, err
	}
	s := &Summary{
		Recording: src.Recording.Dir, Model: src.Model.Name, Controller: src.Recording.Controller,
		AttachMode: src.Recording.AttachMode, Partial: src.Recording.Exported, Started: src.Recording.Started, Ended: src.Recording.Ended,
		Events: len(events), LogLines: len(logs), Outcomes: map[string]int{}, Slowest: []EventRow{}, rec: rec,
	}
	by := map[string]*UnitSummary{}
	for _, ev := range events {
		s.Outcomes[ev.Fail.String()]++
		if ev.Unit == "" {
			continue
		}
		u := by[ev.Unit]
		if u == nil {
			u = &UnitSummary{Unit: ev.Unit}
			by[ev.Unit] = u
		}
		u.Events++
		if ev.Fail.String() != "none" {
			u.Problems++
		}
	}
	for _, u := range by {
		s.Units = append(s.Units, *u)
	}
	sort.Slice(s.Units, func(i, j int) bool { return s.Units[i].Unit < s.Units[j].Unit })
	slow, err := ListEvents(src, EventsQuery{Scope: sc, Page: Page{Limit: 5}, Sort: SortDuration})
	if err != nil {
		return nil, err
	}
	s.Slowest = slow.Events
	cpu, err := ListEvents(src, EventsQuery{Scope: sc, Page: Page{Limit: 5}, Sort: SortCPU})
	if err != nil {
		return nil, err
	}
	for _, r := range cpu.Events {
		if r.Profiled && r.CPUMS > 0 {
			s.Costliest = append(s.Costliest, r)
		}
	}
	return s, nil
}

// WriteText prints the summary, and the commands to go on with.
func (s *Summary) WriteText(w io.Writer) {
	fmt.Fprintf(w, "recording %s, model %s", s.rec, s.Model)
	if s.Controller != "" {
		fmt.Fprintf(w, ", controller %s", s.Controller)
	}
	fmt.Fprintln(w)
	if !s.Started.IsZero() {
		end := "(still being written)"
		if !s.Ended.IsZero() {
			end = s.Ended.UTC().Format("15:04:05")
		}
		fmt.Fprintf(w, "captured %s → %s UTC", s.Started.UTC().Format("2006-01-02 15:04:05"), end)
		if s.AttachMode != "" {
			fmt.Fprintf(w, ", recorded with --attach %s", s.AttachMode)
		}
		fmt.Fprintln(w)
	}
	if p := s.Partial; p != nil {
		fmt.Fprintf(w, "a part of the recording, exported with %s\n", scopeWords(p))
	}
	fmt.Fprintf(w, "%d events, %d log lines; ended: %s\n", s.Events, s.LogLines, outcomeWords(s.Outcomes))

	if len(s.Units) > 0 {
		fmt.Fprintln(w, "\nunits:")
		for _, u := range s.Units {
			line := fmt.Sprintf("  %-18s %3d events", u.Unit, u.Events)
			if u.Problems > 0 {
				line += fmt.Sprintf(", %d not clean", u.Problems)
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(s.Slowest) > 0 {
		fmt.Fprintln(w, "\nslowest events:")
		for _, r := range s.Slowest {
			fmt.Fprintf(w, "  %s  %-14s %-30s %s  span %s\n", r.TS.UTC().Format("15:04:05.000"), dash(r.Unit), r.Name, durMS(float64(r.DurationMS)), r.SpanID)
		}
	}
	if len(s.Costliest) > 0 {
		fmt.Fprintln(w, "\nmost CPU in the charm's code (init and observers):")
		for _, r := range s.Costliest {
			fmt.Fprintf(w, "  %s  %-14s %-30s %s  span %s\n", r.TS.UTC().Format("15:04:05.000"), dash(r.Unit), r.Name, cpuCell(r.HookCost), r.SpanID)
		}
	}
	fmt.Fprintf(w, "\nnext: juju-lens inspect events %s --fail problem\n", s.rec)
	if len(s.Costliest) > 0 {
		fmt.Fprintf(w, "      juju-lens inspect costs %s\n", s.rec)
	}
	fmt.Fprintf(w, "      juju-lens inspect logs %s --level error,warning\n", s.rec)
}

func outcomeWords(o map[string]int) string {
	order := []string{"errored", "retried", "interrupted", "none"}
	var parts []string
	seen := map[string]bool{}
	for _, k := range order {
		if n := o[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
		seen[k] = true
	}
	var rest []string
	for k, n := range o {
		if !seen[k] && n > 0 {
			rest = append(rest, fmt.Sprintf("%s %d", k, n))
		}
	}
	sort.Strings(rest)
	parts = append(parts, rest...)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// WriteJSON prints the summary as JSON.
func (s *Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(s)
}

// scopeWords says what an export kept: "--unit grafana --since 17:56:00Z".
func scopeWords(p *recording.ExportScope) string {
	var parts []string
	for _, m := range p.Models {
		parts = append(parts, "--model "+m)
	}
	for _, u := range p.Units {
		parts = append(parts, "--unit "+u)
	}
	if !p.Since.IsZero() {
		parts = append(parts, "--since "+p.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if !p.Until.IsZero() {
		parts = append(parts, "--until "+p.Until.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return strings.Join(parts, " ")
}
