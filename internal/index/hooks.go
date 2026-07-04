package index

import (
	"encoding/json"
	"regexp"

	"github.com/lucabello/juju-lens/internal/recording"
)

// The uniter records its current operation in Uniter.SetState's uniter-state
// blob (a YAML document). A hook execution is bracketed by
// `op: run-hook` with a `hook: {kind: <name>}` at its start and `op: continue`
// when it finishes, so we can attribute every RPC a unit made between those
// markers to the hook that was running — which is what turns the timeline from
// a list of "Uniter.CommitHookChanges" into a readable "config-changed" /
// "relation-changed" sequence (VISION §6.2 mockup).
var (
	uniterOpRe       = regexp.MustCompile(`(?m)^op:\s*(\S+)`)
	uniterHookKindRe = regexp.MustCompile(`hook:\s*\n\s*kind:\s*(\S+)`)
)

// LabelHooks fills SpanRow.Hook for spans that don't already have one, by
// replaying each unit's SetState operations in time order. spans must be sorted
// by start time (as recording.LoadSpans returns them). It mutates spans in
// place.
func LabelHooks(spans []recording.SpanRow) {
	byUnit := map[string][]int{}
	for i := range spans {
		if spans[i].Unit != "" {
			byUnit[spans[i].Unit] = append(byUnit[spans[i].Unit], i)
		}
	}
	for _, idxs := range byUnit {
		current := ""
		for _, i := range idxs {
			sp := &spans[i]
			if sp.Attrs["method"] == "SetState" {
				if hook, op := parseUniterState(sp.Attrs["params"]); op != "" {
					if op == "run-hook" && hook != "" {
						current = hook
					} else {
						current = "" // op: continue (or anything not a hook) ends it
					}
				}
			}
			if sp.Hook == "" {
				sp.Hook = current
			}
		}
	}
}

// parseUniterState pulls the current op and, when running a hook, its kind out
// of a SetState params payload. Returns empty strings when the payload is not a
// uniter-state SetState.
func parseUniterState(params string) (hook, op string) {
	if params == "" {
		return "", ""
	}
	var a struct {
		Args []struct {
			UniterState string `json:"uniter-state"`
		} `json:"args"`
	}
	if err := json.Unmarshal([]byte(params), &a); err != nil || len(a.Args) == 0 {
		return "", ""
	}
	us := a.Args[0].UniterState
	if m := uniterOpRe.FindStringSubmatch(us); m != nil {
		op = m[1]
	}
	if m := uniterHookKindRe.FindStringSubmatch(us); m != nil {
		hook = m[1]
	}
	return hook, op
}
