package narrative

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// opsJoinSlack is how far a hook process may start before, or end after,
// the run-hook..continue bracket of the same hook and still be joined to it.
// The two clocks are on the same host; this covers the time between the
// uniter writing its marker and starting the process.
const opsJoinSlack = time.Second

// joinOps folds the hook processes the tap recorded (ops root spans) into
// the hook runs derived from RPC markers: a process joins the run of the
// same unit, hook kind and remote application whose bracket it overlaps
// most. A process no run claims becomes a run of its own; consecutive
// processes of the same hook on a unit, after one that failed, are Juju
// retrying it and fold into one run, as the RPC brackets do.
//
// Every hook and action root ends up in exactly one run's OpsSpans, so a
// run leads to its processes and a process leads back to its run.
func joinOps(runs []HookRun, ops []recording.SpanRow) []HookRun {
	if len(ops) == 0 {
		return runs
	}
	byTrace := map[string][]string{}
	var roots []recording.SpanRow
	for _, sp := range ops {
		byTrace[sp.TraceID] = append(byTrace[sp.TraceID], sp.SpanID)
		if sp.Attrs["kind"] != recording.OpsRoot {
			continue
		}
		if e := sp.Attrs["exec"]; e == recording.ExecHook || e == recording.ExecAction {
			roots = append(roots, sp)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Start.Before(roots[j].Start) })

	nRPC := len(runs)
	lastFailed := map[int]bool{} // run -> its latest process failed
	latest := map[string]int{}   // unit -> its latest ops-only run, while no other run came after it
	for _, root := range roots {
		env := opsEnv(root)
		if i := matchRPCRun(runs[:nRPC], root, env); i >= 0 {
			r := &runs[i]
			r.OpsSpans = append(r.OpsSpans, root.SpanID)
			r.SpanIDs = append(r.SpanIDs, byTrace[root.TraceID]...)
			// A hook the tap missed that only RPC saw is named by its bare
			// kind, which says no more than the run's.
			if r.Name == "" && root.Attrs["seen_by"] != recording.SeenByRPC {
				r.Name = root.Hook
			}
			r.setTap(root)
			lastFailed[i] = root.StatusCode == "ERROR"
			delete(latest, root.Unit)
			continue
		}
		failed := root.StatusCode == "ERROR"
		open := root.Attrs["open"] == "true"
		if i, ok := latest[root.Unit]; ok && runs[i].Name == root.Hook && lastFailed[i] && !runs[i].Open {
			r := &runs[i]
			r.Retries++
			r.RepSpan = root.SpanID
			r.OpsSpans = append(r.OpsSpans, root.SpanID)
			r.SpanIDs = append(r.SpanIDs, byTrace[root.TraceID]...)
			r.EndTs, r.Open = root.End, open
			r.setTap(root)
			lastFailed[i] = failed
			continue
		}
		kind := root.Attrs["hook_kind"]
		if kind == "" {
			kind = root.Hook // an action
		}
		runs = append(runs, HookRun{
			Unit: root.Unit, App: appOf(root.Unit), Kind: kind, Name: root.Hook,
			RemoteApp: env["remote_app"], StorageID: env["storage_id"],
			StartTs: root.Start, EndTs: root.End, RepSpan: root.SpanID,
			OpsSpans: []string{root.SpanID}, SpanIDs: byTrace[root.TraceID], Open: open,
			Tap: root.Attrs["tap"], TapReason: root.Attrs["tap_reason"],
		})
		latest[root.Unit] = len(runs) - 1
		lastFailed[len(runs)-1] = failed
	}
	// Each process in a bracket is an attempt: some Juju versions write the
	// run-hook marker once however often they retry, so the processes can
	// show retries the markers don't.
	for i := 0; i < nRPC; i++ {
		r := &runs[i]
		if n := len(r.OpsSpans) - 1; n > r.Retries {
			r.Retries = n
			r.classify()
		}
		if r.Open && lastFailed[i] {
			r.Fail = FailErrored // failed, and not retried yet
		}
	}
	for i := nRPC; i < len(runs); i++ {
		r := &runs[i]
		switch {
		case r.Open:
			r.EndTs = time.Time{}
			if r.Retries > 0 {
				r.Fail = FailErrored
			}
		case lastFailed[i]:
			r.Fail = FailErrored
		case r.Retries > 0:
			r.Fail = FailRetried
		}
	}
	return runs
}

// setTap takes the tap state of a process root, unless an earlier process
// of the run already set one.
func (r *HookRun) setTap(root recording.SpanRow) {
	if r.Tap == "" && root.Attrs["tap"] != "" {
		r.Tap, r.TapReason = root.Attrs["tap"], root.Attrs["tap_reason"]
	}
}

// matchRPCRun returns the RPC-derived run a hook process ran in, or -1.
func matchRPCRun(runs []HookRun, root recording.SpanRow, env map[string]string) int {
	if root.Attrs["exec"] != recording.ExecHook {
		return -1 // actions aren't run-hook brackets
	}
	kind, remote := root.Attrs["hook_kind"], env["remote_app"]
	end := root.End
	if end.Before(root.Start) {
		end = root.Start
	}
	best, bestOverlap := -1, time.Duration(-1)
	for i := range runs {
		r := &runs[i]
		if r.Unit != root.Unit || r.Kind != kind {
			continue
		}
		if remote != "" && r.RemoteApp != "" && remote != r.RemoteApp {
			continue
		}
		lo := r.StartTs.Add(-opsJoinSlack)
		hi := r.EndTs.Add(opsJoinSlack)
		if r.Open || r.EndTs.IsZero() {
			hi = end.Add(opsJoinSlack) // still open: it covers the process
		}
		s, e := root.Start, end
		if lo.After(s) {
			s = lo
		}
		if hi.Before(e) {
			e = hi
		}
		if ov := e.Sub(s); ov >= 0 && ov > bestOverlap {
			best, bestOverlap = i, ov
		}
	}
	return best
}

// opsEnv returns the JUJU_* environment the tap recorded on a root span,
// keyed without the prefix and in lower case ("remote_app").
func opsEnv(root recording.SpanRow) map[string]string {
	var env map[string]string
	_ = json.Unmarshal([]byte(root.Attrs["env"]), &env)
	return env
}
