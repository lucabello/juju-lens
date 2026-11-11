// Package inspect answers questions about a recording as bounded text, or as
// JSON, for an agent (or a person) that would rather ask than read every file
// of an export. It sits on export.Source, so it agrees with the export on
// what a hook cost.
package inspect

import (
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/narrative"
)

// Scope picks events. A zero Scope picks them all. Within one field the
// values are alternatives; across fields they all have to hold.
type Scope struct {
	Units []string  // a unit ("grafana/0") or an application ("grafana")
	Names []string  // part of the event name, such as "config-changed"; case doesn't matter
	Kinds []string  // "hook", "action", "status", ...; see export.KindLabel
	Fail  []string  // a fail state, or "problem" for any but "none"
	Since time.Time // events from this time on
	Until time.Time // events up to this time
}

// FailStates are the values Scope.Fail takes besides "problem".
var FailStates = []string{"none", "errored", "retried", "rpc-warn", "interrupted", "lost-continue"}

// EventKinds are the values Scope.Kinds takes.
var EventKinds = []string{"hook", "action", "status", "relation", "leader", "secret", "settle"}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Validate rejects a Fail or Kinds value that matches nothing, which would
// otherwise read as "no such events".
func (sc Scope) Validate() error {
	for _, f := range sc.Fail {
		if f != "problem" && !oneOf(f, FailStates) {
			return fmt.Errorf("unknown --fail %q (want problem or one of %s)", f, strings.Join(FailStates, ", "))
		}
	}
	for _, k := range sc.Kinds {
		if !oneOf(k, EventKinds) {
			return fmt.Errorf("unknown --kind %q (want one of %s)", k, strings.Join(EventKinds, ", "))
		}
	}
	return nil
}

// Events returns the events of src that the scope picks, in time order.
func (sc Scope) Events(src *export.Source) ([]narrative.Event, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	var out []narrative.Event
	for _, ev := range src.Events(export.Options{Units: sc.Units, Since: sc.Since, Until: sc.Until}) {
		if len(sc.Kinds) > 0 && !oneOf(export.KindLabel(ev.Kind), sc.Kinds) {
			continue
		}
		if len(sc.Names) > 0 && !containsFold(ev.Summary, sc.Names) {
			continue
		}
		if len(sc.Fail) > 0 {
			f := ev.Fail.String()
			if !oneOf(f, sc.Fail) && !(oneOf("problem", sc.Fail) && f != "none") {
				continue
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

func containsFold(s string, subs []string) bool {
	s = strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// Page is the part of a list a command shows.
type Page struct {
	Limit int // at most this many rows; 0 means all
	Skip  int // leave out this many first
}

func (p Page) bounds(n int) (lo, hi int) {
	lo = min(max(p.Skip, 0), n)
	hi = n
	if p.Limit > 0 {
		hi = min(lo+p.Limit, n)
	}
	return lo, hi
}

// more is the line a bounded list ends with: how much it shows and how to get
// the rest.
func (p Page) more(shown, total int, noun string) string {
	lo, hi := p.bounds(total)
	if total == 0 {
		return fmt.Sprintf("no %s match", noun)
	}
	if lo == 0 && hi == total {
		if total == 1 {
			noun = strings.TrimSuffix(noun, "s")
		}
		return fmt.Sprintf("%d %s", total, noun)
	}
	line := fmt.Sprintf("showing %d-%d of %d %s", lo+1, hi, total, noun)
	if hi < total {
		line += fmt.Sprintf("; --skip %d for more", hi)
	}
	return line
}
