package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
)

// State kinds, as the user names them, and the snapshot kinds they read.
var stateKinds = map[string][]string{
	"status":     {"app-status", "unit-status", "agent-status"},
	"config":     {"config"},
	"databag":    {"databag"},
	"leadership": {"leadership"},
}

// StateKinds are the values StateQuery.Kind takes, besides "all".
var StateKinds = []string{"status", "config", "databag", "leadership"}

// StateQuery asks what Juju held at a time.
type StateQuery struct {
	Page
	Kind  string    // one of StateKinds, or "" for all of them
	At    time.Time // the time; zero is the end of the recording
	Span  string    // an event: the state when its hook had ended; replaces At
	Match []string  // only scopes containing one of these, ignoring case, such as "grafana"
	Width int       // cut each value at this many characters; 0 picks 200
}

// StateRow is the value one scope held.
type StateRow struct {
	Kind   string          `json:"kind"`
	Scope  string          `json:"scope"`
	Since  time.Time       `json:"since"` // when the value was recorded
	Origin string          `json:"origin,omitempty"`
	Value  json.RawMessage `json:"value"`
}

// StateList is the state at a time.
type StateList struct {
	Recording string     `json:"recording"`
	Model     string     `json:"model"`
	At        time.Time  `json:"at"`
	After     string     `json:"after,omitempty"` // with --span: the event
	Total     int        `json:"total"`
	Skip      int        `json:"skip,omitempty"`
	Missing   []string   `json:"kinds_without_records,omitempty"`
	State     []StateRow `json:"state"`

	width int
}

// ReadState runs q against src.
func ReadState(src *export.Source, q StateQuery) (StateList, error) {
	l := StateList{Recording: src.Recording.Dir, Model: src.Model.Name, State: []StateRow{}, width: q.Width}
	kinds := StateKinds
	if q.Kind != "" {
		if _, ok := stateKinds[q.Kind]; !ok {
			return l, fmt.Errorf("unknown --kind %q (want %s)", q.Kind, strings.Join(StateKinds, ", "))
		}
		kinds = []string{q.Kind}
	}
	at := q.At
	if q.Span != "" {
		ev, _, err := FindEvent(src, src.Recording.Dir, q.Span)
		if err != nil {
			return l, err
		}
		at = ev.TS.Add(ev.Dur)
		l.After = ev.Unit + " " + ev.Summary
	}
	if at.IsZero() {
		at = src.RecordingEnd()
	}
	l.At = at
	var rows []StateRow
	for _, k := range kinds {
		n := 0
		for _, snap := range stateKinds[k] {
			got, err := src.State(snap, at)
			if err != nil {
				return l, err
			}
			for _, r := range got {
				if len(q.Match) > 0 && !containsFold(r.Scope, q.Match) {
					continue
				}
				n++
				rows = append(rows, StateRow{Kind: snap, Scope: r.Scope, Since: r.Ts, Origin: r.Origin, Value: json.RawMessage(r.Body)})
			}
		}
		if n == 0 {
			l.Missing = append(l.Missing, k)
		}
	}
	lo, hi := q.bounds(len(rows))
	l.Total, l.Skip = len(rows), lo
	l.State = rows[lo:hi]
	return l, nil
}

// WriteText prints the state, one scope a line.
func (l StateList) WriteText(w io.Writer, rec string, p Page) {
	head := fmt.Sprintf("state of %s, model %s, as of %s UTC", rec, l.Model, l.At.UTC().Format("2006-01-02 15:04:05.000"))
	if l.After != "" {
		head += ", after " + l.After
	}
	fmt.Fprintln(w, head+"\n")
	width := l.width
	if width == 0 {
		width = 200
	}
	if len(l.State) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range l.State {
			v := compactJSON(r.Value)
			if len([]rune(v)) > width {
				v = string([]rune(v)[:width]) + "…"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Scope, r.Since.UTC().Format("15:04:05.000"), v)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, p.more(len(l.State), l.Total, "values"))
	for _, k := range l.Missing {
		if k == "databag" {
			fmt.Fprintln(w, "no relation data: a unit recording doesn't hold databag values; record with --attach host")
		} else {
			fmt.Fprintf(w, "no %s records match\n", k)
		}
	}
}

func compactJSON(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// WriteJSON prints the state as JSON.
func (l StateList) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(l)
}
