package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/index"
)

// LogsQuery picks log lines. A zero query picks them all.
type LogsQuery struct {
	Page
	Units  []string
	Levels []string // DEBUG, INFO, WARNING, ERROR, ... ; case doesn't matter
	Grep   []string // lines whose text contains one of these, ignoring case
	Source string   // "debug-log", "k8s" or "journal"
	Since  time.Time
	Until  time.Time
	// Span narrows the lines to those correlated with this event, or span
	// inside it, together with the same unit's lines within Window of the hook.
	Span   string
	Window time.Duration
	// Width cuts the text of a line in the text output; 0 picks 240.
	Width int
}

// logLevels are the levels a log line has.
var logLevels = []string{"TRACE", "DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}

// LogRow is a log line.
type LogRow struct {
	TS      time.Time `json:"ts"`
	Source  string    `json:"source"`
	Unit    string    `json:"unit,omitempty"`
	Entity  string    `json:"entity,omitempty"`
	Level   string    `json:"level,omitempty"`
	Module  string    `json:"module,omitempty"`
	Body    string    `json:"body"`
	SpanID  string    `json:"span_id,omitempty"`
	Matched string    `json:"matched,omitempty"` // with --span: "span" for a line the index tied to the event, "window" for one near it
}

// LogList is a page of log lines.
type LogList struct {
	Recording string   `json:"recording"`
	Model     string   `json:"model"`
	Event     string   `json:"event,omitempty"` // with --span: the event the lines are about
	Total     int      `json:"total"`
	Skip      int      `json:"skip,omitempty"`
	Logs      []LogRow `json:"logs"`

	width int
}

// ListLogs runs q against src.
func ListLogs(src *export.Source, q LogsQuery) (LogList, error) {
	var rows []index.LogRow
	list := LogList{Recording: src.Recording.Dir, Model: src.Model.Name, Logs: []LogRow{}, width: q.Width}
	for _, l := range q.Levels {
		if !containsEq(l, logLevels) {
			return list, fmt.Errorf("unknown --level %q (want one of %s)", l, strings.Join(logLevels, ", "))
		}
	}
	if q.Source != "" && q.Source != "debug-log" && q.Source != "k8s" && q.Source != "journal" {
		return list, fmt.Errorf("unknown --source %q (want debug-log, k8s or journal)", q.Source)
	}
	if q.Span != "" {
		ev, _, err := FindEvent(src, src.Recording.Dir, q.Span)
		if err != nil {
			return list, err
		}
		win := q.Window
		if win == 0 {
			win = 2 * time.Second
		}
		if rows, err = src.EventLogs(ev, win, 100000); err != nil {
			return list, err
		}
		list.Event = ev.Unit + " " + ev.Summary
	} else {
		var err error
		if rows, err = src.Logs(); err != nil {
			return list, err
		}
	}
	units := map[string]bool{}
	for _, u := range q.Units {
		units[u] = true
	}
	var keep []index.LogRow
	for _, r := range rows {
		if len(units) > 0 && !units[r.Unit] && !units[appOfUnit(r.Unit)] && !units[r.Entity] {
			continue
		}
		if len(q.Levels) > 0 && !containsEq(r.Level, q.Levels) {
			continue
		}
		if len(q.Grep) > 0 && !containsFold(r.Body, q.Grep) {
			continue
		}
		if q.Source != "" && r.Source != q.Source {
			continue
		}
		if !q.Since.IsZero() && r.Ts.Before(q.Since) || !q.Until.IsZero() && r.Ts.After(q.Until) {
			continue
		}
		keep = append(keep, r)
	}
	lo, hi := q.bounds(len(keep))
	list.Total, list.Skip = len(keep), lo
	for _, r := range keep[lo:hi] {
		list.Logs = append(list.Logs, LogRow{
			TS: r.Ts, Source: r.Source, Unit: r.Unit, Entity: r.Entity, Level: r.Level, Module: r.Module,
			Body: r.Body, SpanID: r.SpanID, Matched: matchedFor(q.Span, r),
		})
	}
	return list, nil
}

func matchedFor(span string, r index.LogRow) string {
	if span == "" {
		return ""
	}
	return r.Matched
}

func containsEq(v string, set []string) bool {
	for _, s := range set {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func appOfUnit(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}

// WriteText prints the lines and how many there are.
func (l LogList) WriteText(w io.Writer, rec string, p Page) {
	head := fmt.Sprintf("logs of %s, model %s", rec, l.Model)
	if l.Event != "" {
		head += ", around " + l.Event
	}
	fmt.Fprintln(w, head+"\n")
	width := l.width
	if width == 0 {
		width = 240
	}
	cut := 0
	if len(l.Logs) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range l.Logs {
			who := r.Unit
			if who == "" {
				who = r.Entity
			}
			body := strings.ReplaceAll(strings.TrimRight(r.Body, "\n"), "\n", " ⏎ ")
			if len([]rune(body)) > width {
				body = string([]rune(body)[:width]) + "…"
				cut++
			}
			mark := ""
			if r.Matched == "span" {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s%s\t%s\n", r.TS.UTC().Format("15:04:05.000"), dash(who), dash(r.Level), mark, r.Source, body)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, p.more(len(l.Logs), l.Total, "log lines"))
	if l.Event != "" {
		fmt.Fprintln(w, "* a line the index tied to the event by its span id; the others are the same unit's lines near the hook")
	}
	if cut > 0 {
		fmt.Fprintf(w, "%d long lines cut at %d characters (--width, or --json for all of it)\n", cut, width)
	}
}

// WriteJSON prints the lines as JSON.
func (l LogList) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(l)
}
