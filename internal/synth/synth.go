// Package synth generates deterministic recordings whose captured RPCs mimic
// the shape of the Juju API traffic a real controller emits during a workload.
// It exists so the viewer can be developed and tested without a live
// controller and without eBPF.
//
// In the eBPF/RPC approach a recording is a stream of wire.CapturedMessage
// values (one per line in raw/rpc/<model>/calls-*.jsonl): each Juju RPC is a
// write (request) followed by a read (response) sharing a request-id. synth
// produces exactly that stream, so the same pairing/extraction code path runs
// on synthetic and real recordings alike.
//
// M1 ships a single "trivial" scenario: two apps (grafana, prometheus) relating
// over the grafana-source endpoint, each committing a handful of hooks and
// setting unit + application status. That is enough to exercise the timeline,
// the apps sidebar, and the status pane.
package synth

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lucabello/juju-lens/internal/wire"
)

// Scenario is the identifier for a synth scenario.
type Scenario string

const (
	// ScenarioTrivial produces two applications, one relation, and a couple
	// dozen RPCs. Time origin defaults to 2026-07-03T14:30:12Z UTC.
	ScenarioTrivial Scenario = "trivial"
)

// KnownScenarios lists every scenario the tool currently supports.
func KnownScenarios() []Scenario { return []Scenario{ScenarioTrivial} }

// Options tune how a scenario is emitted. Any zero value falls back to a
// sensible default that keeps recordings deterministic across runs.
type Options struct {
	// Start is the wall-clock instant assigned to the first RPC. When zero, a
	// fixed 2026-07-03T14:30:12Z instant is used so recordings are
	// byte-for-byte identical between runs.
	Start time.Time
	// ModelName is the model every captured RPC is attributed to. Defaults to
	// "default".
	ModelName string
}

func (o Options) filled() Options {
	if o.Start.IsZero() {
		o.Start = time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	}
	if o.ModelName == "" {
		o.ModelName = "default"
	}
	return o
}

// Generate builds the ordered stream of captured RPC messages for a scenario.
// It does not touch the filesystem or the network; callers write the returned
// messages wherever they like. Messages are returned in capture order (each
// request immediately followed by its response) which is also the order the
// recorder would append them to the raw file.
func Generate(sc Scenario, opts Options) ([]wire.CapturedMessage, error) {
	switch sc {
	case ScenarioTrivial:
		return generateTrivial(opts.filled()), nil
	default:
		return nil, fmt.Errorf("synth: unknown scenario %q", sc)
	}
}

// BootstrapStatus returns a `juju status --format=json` document describing the
// model's ground truth at recording start, for the given scenario. The recorder
// captures the equivalent from a live controller; synth ships one so the viewer
// shows real statuses from t0 instead of "unknown" (VISION M8).
//
// The trivial bootstrap deliberately differs from where the RPC deltas end up,
// so scrubbing the timeline shows state evolving: grafana/prometheus start in
// maintenance/installing and become active/waiting as their hooks run, while
// traefik — which has no RPCs in the scenario at all — is known *only* from the
// bootstrap, proving the seed is what keeps it off "unknown".
func BootstrapStatus(sc Scenario, opts Options) ([]byte, error) {
	if sc != ScenarioTrivial {
		return nil, fmt.Errorf("synth: no bootstrap for scenario %q", sc)
	}
	o := opts.filled()
	app := func(status, msg string, units map[string]any) map[string]any {
		return map[string]any{
			"application-status": map[string]any{"current": status, "message": msg},
			"units":              units,
		}
	}
	unit := func(status, msg string) map[string]any {
		return map[string]any{"workload-status": map[string]any{"current": status, "message": msg}}
	}
	// leaderUnit marks a unit as its application's leader, so the M8 bootstrap
	// seeds a leadership snapshot and the Status pane shows "(leader)" from t0.
	leaderUnit := func(status, msg string) map[string]any {
		u := unit(status, msg)
		u["leader"] = true
		return u
	}
	doc := map[string]any{
		"model": map[string]any{"name": o.ModelName},
		"applications": map[string]any{
			"grafana":    app("maintenance", "installing", map[string]any{"grafana/0": leaderUnit("maintenance", "installing")}),
			"prometheus": app("maintenance", "installing", map[string]any{"prometheus/0": leaderUnit("maintenance", "installing")}),
			"traefik":    app("active", "", map[string]any{"traefik/0": leaderUnit("active", "")}),
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// LogLine is one synthetic `juju debug-log` line: the instant it was emitted
// (for hour-bucketing into the raw layout) and the verbatim text a real
// debug-log stream would have written. Callers append Text to the per-model log
// file whose hour matches Ts, exactly as the recorder does for live output.
type LogLine struct {
	Ts   time.Time
	Text string
}

// DebugLog returns the synthetic debug-log stream for a scenario, interleaved
// with the RPC timeline so the viewer's merged Logs pane (VISION §7) has real
// content to fold events into. Lines are honestly *time-adjacent* to events, not
// span-tagged: the trivial scenario deliberately includes traefik/0 — a unit
// with no captured RPCs — so the pane shows a log source that no event ever
// "owns", which is the whole point of merging by time rather than by span.
func DebugLog(sc Scenario, opts Options) ([]LogLine, error) {
	if sc != ScenarioTrivial {
		return nil, fmt.Errorf("synth: no debug-log for scenario %q", sc)
	}
	o := opts.filled()
	var out []LogLine
	// at emits a line at Start+off. entity is a juju tag ("unit-grafana-0"); the
	// indexer resolves it back to a unit, so it must match the RPC unit tags.
	at := func(off time.Duration, entity, level, module, msg string) {
		ts := o.Start.Add(off)
		out = append(out, LogLine{
			Ts: ts,
			Text: fmt.Sprintf("%s: %s %s %s %s",
				entity, ts.UTC().Format("2006-01-02 15:04:05.000"), level, module, msg),
		})
	}
	const uniter = "juju.worker.uniter.operation"

	// grafana/0: install → config → start → relation → active. Timestamps track
	// the hook RPCs in generateTrivial so overlay correlation (unit+window) hits.
	at(20*time.Millisecond, "unit-grafana-0", "INFO", uniter, `ran "install" hook`)
	at(300*time.Millisecond, "unit-grafana-0", "DEBUG", "unit.grafana/0.juju-log", "Reconciling grafana configuration")
	at(700*time.Millisecond, "unit-grafana-0", "INFO", uniter, `ran "config-changed" hook`)
	at(1050*time.Millisecond, "unit-grafana-0", "INFO", uniter, `ran "start" hook`)
	at(1250*time.Millisecond, "unit-grafana-0", "WARNING", "unit.grafana/0.juju-log", "grafana-source: no datasource peers yet, waiting")
	at(1500*time.Millisecond, "unit-grafana-0", "INFO", uniter, `ran "grafana-source-relation-joined" hook`)

	// prometheus/0: starts ~50ms after grafana; ERROR line gives the Logs pane a
	// coloured level and the overlay something to correlate to the failed-lookup.
	at(90*time.Millisecond, "unit-prometheus-0", "INFO", uniter, `ran "install" hook`)
	at(760*time.Millisecond, "unit-prometheus-0", "INFO", uniter, `ran "config-changed" hook`)
	at(1120*time.Millisecond, "unit-prometheus-0", "ERROR", "unit.prometheus/0.juju-log", "scrape target grafana unreachable, will retry")
	at(1400*time.Millisecond, "unit-prometheus-0", "INFO", uniter, `ran "grafana-source-relation-joined" hook`)
	at(1900*time.Millisecond, "unit-prometheus-0", "DEBUG", "unit.prometheus/0.juju-log", "scrape_period updated to 60s")

	// traefik/0: known only from the M8 bootstrap — it has zero RPCs, so these
	// lines are log-only, proving the merged stream shows sources no event owns.
	at(400*time.Millisecond, "unit-traefik-0", "INFO", uniter, `ran "update-status" hook`)
	at(1600*time.Millisecond, "unit-traefik-0", "INFO", "unit.traefik/0.juju-log", "ingress ready for grafana, prometheus")

	sortLogLines(out)
	return out, nil
}

// sortLogLines orders by timestamp so the raw file is chronological (real
// debug-log output is), keeping hour-bucketing and later parsing stable.
func sortLogLines(ls []LogLine) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j-1].Ts.After(ls[j].Ts); j-- {
			ls[j-1], ls[j] = ls[j], ls[j-1]
		}
	}
}

// agent models one unit's API connection: a stable pid/conn and a monotonic
// request-id counter. Timestamps advance a shared cursor so the two agents
// interleave realistically on the timeline.
type agent struct {
	unit  string
	model string
	pid   int
	conn  uint64
	rid   uint64
	now   time.Time
}

func (a *agent) advance(d time.Duration) { a.now = a.now.Add(d) }

// rpc appends a request/response pair for one Juju API call and returns the two
// messages. dur is how long the call took (request ts .. response ts).
func (a *agent) rpc(facade, method string, params any, dur time.Duration) []wire.CapturedMessage {
	a.rid++
	rid := a.rid
	reqTs := a.now
	respTs := a.now.Add(dur)
	a.now = respTs.Add(time.Millisecond)

	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := wire.CapturedMessage{
		Ts: reqTs, PID: a.pid, Dir: wire.DirWrite, Conn: a.conn,
		Model: a.model, Unit: a.unit,
		Msg: wire.Envelope{RequestID: rid, Type: facade, Version: 1, Request: method, Params: raw},
	}
	resp := wire.CapturedMessage{
		Ts: respTs, PID: a.pid, Dir: wire.DirRead, Conn: a.conn,
		Model: a.model, Unit: a.unit,
		Msg: wire.Envelope{RequestID: rid, Response: json.RawMessage(`{}`)},
	}
	return []wire.CapturedMessage{req, resp}
}

// --- param shapes (match internal/index/extract.go's decoder) ---

type entityStatus struct {
	Tag    string `json:"tag"`
	Status string `json:"status"`
	Info   string `json:"info,omitempty"`
}

func statusParams(tag, status, info string) map[string]any {
	return map[string]any{"entities": []entityStatus{{Tag: tag, Status: status, Info: info}}}
}

// commitHookParams is a compact stand-in for Uniter.CommitHookChanges args,
// matching the real Juju 3.6 wire shape ({"args":[{"tag":…, …}]}). It exists so
// the raw file and details pane show a realistic payload the databag extractor
// runs against.
func commitHookParams(unitTag, hook string, relationID int) map[string]any {
	return commitHookParamsWithDatabag(unitTag, hook, relationID, "", nil)
}

// commitHookParamsWithDatabag is commitHookParams that also carries a relation
// databag write, mirroring how a real CommitHookChanges embeds the unit's full
// relation settings after a `relation-set`. When settings is nil no databag is
// written. Crucially — like real Juju — it sends the *entire* databag each
// time, so re-committing identical settings must produce no databag change once
// the extractor dedups (VISION M7).
func commitHookParamsWithDatabag(unitTag, hook string, relationID int, relationTag string, settings map[string]string) map[string]any {
	arg := map[string]any{"tag": unitTag, "update-network-info": false, "hook": hook}
	if relationID >= 0 {
		arg["relation-id"] = relationID
	}
	if settings != nil {
		arg["relation-unit-settings"] = []map[string]any{{
			"relation": relationTag,
			"unit":     unitTag,
			"settings": settings,
		}}
	}
	return map[string]any{"args": []map[string]any{arg}}
}

// setStateParams builds a Uniter.SetState payload carrying a uniter-state YAML
// blob, matching the shape index.parseUniterState reads.
func setStateParams(uniterState string) map[string]any {
	return map[string]any{"args": []map[string]any{{"uniter-state": uniterState}}}
}

// runHookState / continueState are the two uniter-state markers a real uniter
// writes around a hook execution. index.LabelHooks brackets everything between
// them with the hook's name, which is what promotes an otherwise anonymous
// CommitHookChanges into a named "install" / "config-changed" timeline event.
func runHookState(kind string) string {
	return fmt.Sprintf("op: run-hook\nhook:\n  kind: %s\n", kind)
}

const continueState = "op: continue\n"

func generateTrivial(opts Options) []wire.CapturedMessage {
	grafana := &agent{unit: "grafana/0", model: opts.ModelName, pid: 1001, conn: 0x61a, now: opts.Start}
	prometheus := &agent{unit: "prometheus/0", model: opts.ModelName, pid: 1002, conn: 0x62a, now: opts.Start.Add(50 * time.Millisecond)}

	// The relation tag both units write databags on. Its key (tag minus the
	// "relation-" prefix) becomes the "databag:<key>:<entity>" snapshot scope,
	// so it deliberately contains no ':' the viewer would mis-split on.
	const rel = "relation-grafana.grafana-source#prometheus.grafana-source"

	var msgs []wire.CapturedMessage
	add := func(m []wire.CapturedMessage) { msgs = append(msgs, m...) }
	// hook brackets a hook's RPCs with the SetState run-hook/continue markers a
	// real uniter writes, so index.LabelHooks attributes the enclosed commit to
	// the hook and it surfaces as a named timeline event. body does the actual
	// commit(s) that ran inside the hook.
	hook := func(a *agent, kind string, body func()) {
		add(a.rpc("Uniter", "SetState", setStateParams(runHookState(kind)), 4*time.Millisecond))
		body()
		add(a.rpc("Uniter", "SetState", setStateParams(continueState), 4*time.Millisecond))
	}
	commit := func(a *agent, unitTag, hookName string, relationID int, dur time.Duration) {
		add(a.rpc("Uniter", "CommitHookChanges", commitHookParams(unitTag, hookName, relationID), dur))
	}
	commitDatabag := func(a *agent, unitTag, hookName string, settings map[string]string, dur time.Duration) {
		add(a.rpc("Uniter", "CommitHookChanges",
			commitHookParamsWithDatabag(unitTag, hookName, 3, rel, settings), dur))
	}

	// grafana/0 installs, configures, starts, then forms the relation, writes
	// its databag on relation-joined, and (as leader) sets both its unit status
	// and the application status.
	hook(grafana, "install", func() { commit(grafana, "unit-grafana-0", "install", -1, 480*time.Millisecond) })
	hook(grafana, "config-changed", func() { commit(grafana, "unit-grafana-0", "config-changed", -1, 200*time.Millisecond) })
	hook(grafana, "start", func() { commit(grafana, "unit-grafana-0", "start", -1, 300*time.Millisecond) })
	add(grafana.rpc("Uniter", "EnterScope", map[string]any{"relation-id": 3, "unit-tag": "unit-grafana-0"}, 40*time.Millisecond))
	hook(grafana, "grafana-source-relation-created", func() {
		commit(grafana, "unit-grafana-0", "grafana-source-relation-created", 3, 120*time.Millisecond)
	})
	hook(grafana, "grafana-source-relation-joined", func() {
		commitDatabag(grafana, "unit-grafana-0", "grafana-source-relation-joined",
			map[string]string{"ingress-address": "10.1.2.3", "grafana_uid": "cos"}, 420*time.Millisecond)
	})
	add(grafana.rpc("Uniter", "SetStatus", statusParams("unit-grafana-0", "active", "Ready"), 8*time.Millisecond))
	add(grafana.rpc("Uniter", "SetApplicationStatus", statusParams("application-grafana", "active", "All units ready"), 8*time.Millisecond))

	// prometheus/0 installs, starts, joins the relation, writes its databag,
	// then re-runs relation-changed committing the *same* settings — the second
	// commit must dedup to no databag change (M7) — and finally changes a single
	// key on a later relation-changed, a real one-key delta.
	pDatabag := map[string]string{"ingress-address": "10.2.3.4", "scrape_period": "30s"}
	hook(prometheus, "install", func() { commit(prometheus, "unit-prometheus-0", "install", -1, 500*time.Millisecond) })
	hook(prometheus, "config-changed", func() { commit(prometheus, "unit-prometheus-0", "config-changed", -1, 200*time.Millisecond) })
	hook(prometheus, "start", func() { commit(prometheus, "unit-prometheus-0", "start", -1, 250*time.Millisecond) })
	add(prometheus.rpc("Uniter", "EnterScope", map[string]any{"relation-id": 3, "unit-tag": "unit-prometheus-0"}, 40*time.Millisecond))
	hook(prometheus, "grafana-source-relation-joined", func() {
		commitDatabag(prometheus, "unit-prometheus-0", "grafana-source-relation-joined", pDatabag, 380*time.Millisecond)
	})
	hook(prometheus, "grafana-source-relation-changed", func() {
		commitDatabag(prometheus, "unit-prometheus-0", "grafana-source-relation-changed", pDatabag, 340*time.Millisecond)
	})
	hook(prometheus, "grafana-source-relation-changed", func() {
		commitDatabag(prometheus, "unit-prometheus-0", "grafana-source-relation-changed",
			map[string]string{"ingress-address": "10.2.3.4", "scrape_period": "60s"}, 300*time.Millisecond)
	})
	add(prometheus.rpc("Uniter", "SetStatus", statusParams("unit-prometheus-0", "waiting", "waiting for grafana"), 8*time.Millisecond))
	add(prometheus.rpc("Uniter", "SetApplicationStatus", statusParams("application-prometheus", "waiting", "waiting for peers"), 8*time.Millisecond))

	return msgs
}
