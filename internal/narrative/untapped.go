package narrative

import (
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// statusLogSlack is how far a hook process may start before the status log
// entry that announces it, or after the unit's next entry, and still be the
// process it announced. The agent sets "executing" just before it starts
// the hook; the entry and the process carry clocks of different hosts.
const statusLogSlack = 2 * time.Second

// UntappedSightings returns the hook and action runs that the status log
// (statusLog, from recording.LoadStatusLog) or an RPC run-hook bracket saw,
// but that no process the tap recorded accounts for. spans are the RPC and
// ops spans of the recording.
//
// A status log entry is accounted for by a tapped process of the same unit
// and hook that started while the entry was current. An RPC bracket is
// accounted for when a tapped process joined it (joinOps); one that a
// status log entry the tap missed overlaps is that entry, seen twice.
// update-status is never in the status log, so RPC alone can cross-check it.
func UntappedSightings(spans []recording.SpanRow, statusLog []recording.HookSighting) []recording.HookSighting {
	spanByID := make(map[string]recording.SpanRow, len(spans))
	var roots []recording.SpanRow
	for _, sp := range spans {
		spanByID[sp.SpanID] = sp
		if sp.Service != recording.ServiceOps || sp.Attrs["kind"] != recording.OpsRoot {
			continue
		}
		if e := sp.Attrs["exec"]; e == recording.ExecHook || e == recording.ExecAction {
			roots = append(roots, sp)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Start.Before(roots[j].Start) })

	claimed := make([]bool, len(roots))
	var out []recording.HookSighting
	for _, s := range statusLog {
		if i := matchRoot(roots, claimed, s); i >= 0 {
			claimed[i] = true
			continue
		}
		out = append(out, s)
	}
	nStatus := len(out)

	for _, r := range BuildHookRuns(spans) {
		rep, ok := spanByID[r.RepSpan]
		if len(r.OpsSpans) > 0 || !ok || rep.Service == recording.ServiceOps {
			continue
		}
		end := r.EndTs
		if end.IsZero() {
			end = r.StartTs
		}
		seen := false
		for i := range nStatus {
			s := &out[i]
			if s.Unit == r.Unit && s.Exec == recording.ExecHook && s.Kind == r.Kind &&
				overlaps(s.Start, s.End, r.StartTs.Add(-statusLogSlack), end.Add(statusLogSlack)) {
				s.SeenBy = append(s.SeenBy, recording.SeenByRPC)
				seen = true
			}
		}
		if seen {
			continue
		}
		out = append(out, recording.HookSighting{
			Model: rep.Model, Unit: r.Unit, Exec: recording.ExecHook, Kind: r.Kind,
			RemoteApp: r.RemoteApp, Start: r.StartTs, End: end,
			SeenBy: []string{recording.SeenByRPC}, RawFile: rep.RawFile,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// matchRoot returns the first unclaimed tapped process of the hook or
// action s announces, started while s was current, or -1.
func matchRoot(roots []recording.SpanRow, claimed []bool, s recording.HookSighting) int {
	name := s.Name
	if s.Exec == recording.ExecAction {
		name += " action"
	}
	lo, hi := s.Start.Add(-statusLogSlack), s.End.Add(statusLogSlack)
	for i, root := range roots {
		if claimed[i] || root.Unit != s.Unit || root.Attrs["exec"] != s.Exec || root.Hook != name {
			continue
		}
		if !root.Start.Before(lo) && !root.Start.After(hi) {
			return i
		}
	}
	return -1
}

func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return !aEnd.Before(bStart) && !bEnd.Before(aStart)
}
