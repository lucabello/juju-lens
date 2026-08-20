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
	uniterOpRe        = regexp.MustCompile(`(?m)^op:\s*(\S+)`)
	uniterOpstepRe    = regexp.MustCompile(`(?m)^opstep:\s*(\S+)`)
	uniterHookKindRe  = regexp.MustCompile(`hook:\s*\n\s*kind:\s*(\S+)`)
	uniterRemoteAppRe = regexp.MustCompile(`remote-application:\s*(\S+)`)
	uniterStorageIDRe = regexp.MustCompile(`storage-id:\s*(\S+)`)
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

// HookMarker is the uniter operation encoded in one SetState payload. The viewer
// pairs run-hook..continue markers into hook runs, and uses RemoteApp/StorageID
// to reconstruct the charm-visible hook name (`<endpoint>-relation-changed`,
// `<storage>-storage-attached`) that the wire form leaves as a bare Kind.
type HookMarker struct {
	Kind      string // bare hook kind, e.g. "relation-changed" ("" unless op is run-hook)
	Op        string // "run-hook", "continue", …
	Opstep    string // "queued" | "pending" | "done": the uniter's progress through the op. Every hook run walks queued→pending→done once, even when nothing goes wrong; only a *second* "pending" (a retry loop re-entered after an error) is meaningful on its own (M10, M13)
	RemoteApp string // relation hooks: the remote application (peer relations: the app itself)
	StorageID string // storage hooks: e.g. "data/0"
}

// ParseHookMarker decodes the uniter-state blob in a SetState params payload.
// Returns a zero HookMarker (empty Op) for a payload that is not a uniter-state
// SetState.
func ParseHookMarker(params string) HookMarker {
	us := uniterStateBlob(params)
	if us == "" {
		return HookMarker{}
	}
	var hm HookMarker
	if m := uniterOpRe.FindStringSubmatch(us); m != nil {
		hm.Op = m[1]
	}
	if m := uniterOpstepRe.FindStringSubmatch(us); m != nil {
		hm.Opstep = m[1]
	}
	if m := uniterHookKindRe.FindStringSubmatch(us); m != nil {
		hm.Kind = m[1]
	}
	if m := uniterRemoteAppRe.FindStringSubmatch(us); m != nil {
		hm.RemoteApp = m[1]
	}
	if m := uniterStorageIDRe.FindStringSubmatch(us); m != nil {
		hm.StorageID = m[1]
	}
	return hm
}

// parseUniterState pulls the current op and, when running a hook, its kind out
// of a SetState params payload. Returns empty strings when the payload is not a
// uniter-state SetState.
func parseUniterState(params string) (hook, op string) {
	hm := ParseHookMarker(params)
	return hm.Kind, hm.Op
}

// uniterStateBlob decodes the (YAML) uniter-state string carried in a
// Uniter.SetState params payload, or "" when the payload has none.
func uniterStateBlob(params string) string {
	if params == "" {
		return ""
	}
	var a struct {
		Args []struct {
			UniterState string `json:"uniter-state"`
		} `json:"args"`
	}
	if err := json.Unmarshal([]byte(params), &a); err != nil || len(a.Args) == 0 {
		return ""
	}
	return a.Args[0].UniterState
}
