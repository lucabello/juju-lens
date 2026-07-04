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
// matching the real Juju 3.6 wire shape ({"args":[{"tag":…, …}]}). M2 does not
// decode it (databag reconstruction lands in M4); it exists so the raw file and
// details pane show a realistic payload the M4 extractor can be built against.
func commitHookParams(unitTag, hook string, relationID int) map[string]any {
	arg := map[string]any{"tag": unitTag, "update-network-info": false, "hook": hook}
	if relationID >= 0 {
		arg["relation-id"] = relationID
	}
	return map[string]any{"args": []map[string]any{arg}}
}

func generateTrivial(opts Options) []wire.CapturedMessage {
	grafana := &agent{unit: "grafana/0", model: opts.ModelName, pid: 1001, conn: 0x61a, now: opts.Start}
	prometheus := &agent{unit: "prometheus/0", model: opts.ModelName, pid: 1002, conn: 0x62a, now: opts.Start.Add(50 * time.Millisecond)}

	var msgs []wire.CapturedMessage
	add := func(m []wire.CapturedMessage) { msgs = append(msgs, m...) }

	// grafana/0 installs, configures, starts, then forms the relation and
	// (as leader) sets both its unit status and the application status.
	add(grafana.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-grafana-0", "install", -1), 480*time.Millisecond))
	add(grafana.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-grafana-0", "config-changed", -1), 200*time.Millisecond))
	add(grafana.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-grafana-0", "start", -1), 300*time.Millisecond))
	add(grafana.rpc("Uniter", "EnterScope", map[string]any{"relation-id": 3, "unit-tag": "unit-grafana-0"}, 40*time.Millisecond))
	add(grafana.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-grafana-0", "grafana-source-relation-created", 3), 120*time.Millisecond))
	add(grafana.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-grafana-0", "grafana-source-relation-joined", 3), 420*time.Millisecond))
	add(grafana.rpc("Uniter", "SetStatus", statusParams("unit-grafana-0", "active", "Ready"), 8*time.Millisecond))
	add(grafana.rpc("Uniter", "SetApplicationStatus", statusParams("application-grafana", "active", "All units ready"), 8*time.Millisecond))

	// prometheus/0 installs, starts, joins the relation, reacts to grafana's
	// databag, and settles into "waiting".
	add(prometheus.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-prometheus-0", "install", -1), 500*time.Millisecond))
	add(prometheus.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-prometheus-0", "config-changed", -1), 200*time.Millisecond))
	add(prometheus.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-prometheus-0", "start", -1), 250*time.Millisecond))
	add(prometheus.rpc("Uniter", "EnterScope", map[string]any{"relation-id": 3, "unit-tag": "unit-prometheus-0"}, 40*time.Millisecond))
	add(prometheus.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-prometheus-0", "grafana-source-relation-joined", 3), 380*time.Millisecond))
	add(prometheus.rpc("Uniter", "CommitHookChanges", commitHookParams("unit-prometheus-0", "grafana-source-relation-changed", 3), 340*time.Millisecond))
	add(prometheus.rpc("Uniter", "SetStatus", statusParams("unit-prometheus-0", "waiting", "waiting for grafana"), 8*time.Millisecond))
	add(prometheus.rpc("Uniter", "SetApplicationStatus", statusParams("application-prometheus", "waiting", "waiting for peers"), 8*time.Millisecond))

	return msgs
}
