package synth

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// ScenarioOps is the charm side of a recording only, as `record --attach
// unit` makes on a Kubernetes model it can't attach eBPF to: what the tap
// recorded inside two units of an aggregator charm shaped like
// opentelemetry-collector-k8s. It has no RPCs; its hooks come from the
// tap's records alone.
const ScenarioOps Scenario = "ops"

// UnitRecord is one line of a unit probe session: raw/unit/<model>/<Host>/.
type UnitRecord struct {
	Host string
	Rec  unit.Record
}

// UnitRecords returns the unit probe records of a scenario, each host's in
// the order its probe would write them. Only ScenarioOps has any.
//
// otelcol/0 runs a config-changed that spends most of its time in charm
// init, as the real charm does; a relation-changed whose observer defers
// the event; an update-status that re-emits it from the backlog, where it
// emits a custom event whose observer starts a Python subprocess; and a
// config-changed that raises in an observer twice before it succeeds.
// otelcol/1 runs a config-changed, then an update-status killed before it
// exits.
func UnitRecords(sc Scenario, opts Options) ([]UnitRecord, error) {
	if sc != ScenarioOps {
		return nil, fmt.Errorf("synth: no unit records for scenario %q", sc)
	}
	o := opts.filled()
	var out []UnitRecord
	for i, h := range []func(*tapHost){opsLeader, opsFollower} {
		host := &tapHost{
			model: o.ModelName, unit: fmt.Sprintf("otelcol/%d", i), host: fmt.Sprintf("otelcol-%d", i),
			now: o.Start.Add(time.Duration(i) * 50 * time.Millisecond), nextPID: 100,
		}
		host.mono0 = host.now.UnixNano() - int64(i+1)*400_000*int64(time.Second)
		host.hello()
		h(host)
		host.resources()
		out = append(out, host.out...)
	}
	return out, nil
}

// opsBootstrap is the `juju status` of ScenarioOps at the start: otelcol
// with two active units, otelcol/0 the leader.
func opsBootstrap(o Options) ([]byte, error) {
	unit := func(leader bool) map[string]any {
		u := map[string]any{"workload-status": map[string]any{"current": "active", "message": ""}}
		if leader {
			u["leader"] = true
		}
		return u
	}
	doc := map[string]any{
		"model": map[string]any{"name": o.ModelName},
		"applications": map[string]any{
			"otelcol": map[string]any{
				"application-status": map[string]any{"current": "active", "message": ""},
				"units":              map[string]any{"otelcol/0": unit(true), "otelcol/1": unit(false)},
			},
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// The charm's observers, by qualified name.
const (
	obsResources = "charms.observability_libs.v0.kubernetes_compute_resources_patch.KubernetesComputeResourcesPatch._on_config_changed"
	obsDashboard = "charms.grafana_k8s.v0.grafana_dashboard.GrafanaDashboardProvider._update_all_dashboards_from_dir"
	obsLoki      = "charms.loki_k8s.v1.loki_push_api.LokiPushApiProvider._on_logging_relation_changed"
	obsRules     = "charm.OpenTelemetryCollectorK8sCharm._on_alert_rules_changed"
	obsStatus    = "charm.OpenTelemetryCollectorK8sCharm._on_update_status"
	obsCommit    = "ops.framework.StoredStateData.on_commit"
)

func opsLeader(h *tapHost) {
	h.at(2 * time.Second)
	h.dirty = true // the config changed: the replan restarts the collector
	h.hook("config-changed", nil, func(p *tapProc) int {
		p.reconcile(1450 * time.Millisecond)
		p.backlog(nil)
		ev := p.event("config_changed", "ConfigChangedEvent", "juju")
		p.emit(ev, 0, []string{obsResources, obsDashboard}, func() {
			p.observe(ev, obsResources, false, 0, func() (bool, string, string) {
				p.logs(14)
				return false, "", ""
			})
			p.observe(ev, obsDashboard, false, 0, func() (bool, string, string) {
				p.advance(12 * time.Millisecond)
				p.logs(1)
				return false, "", ""
			})
		})
		p.lifecycle()
		return 0
	})

	h.at(9 * time.Second)
	var deferred tapEvent
	loki := map[string]string{"relation": "receive-loki-logs", "relation_id": "7", "remote_app": "loki", "remote_unit": "loki/0"}
	h.hook("receive-loki-logs-relation-changed", loki, func(p *tapProc) int {
		p.reconcile(980 * time.Millisecond)
		p.backlog(nil)
		deferred = p.event("receive_loki_logs_relation_changed", "RelationChangedEvent", "juju")
		p.emit(deferred, 0, []string{obsLoki}, func() {
			p.observe(deferred, obsLoki, false, 0, func() (bool, string, string) {
				p.tool("relation-get", []string{"-r", "receive-loki-logs:7", "-", "loki/0"}, 3*time.Millisecond, 0)
				p.pebble("GET", "/v1/services", 1*time.Millisecond, 200, map[string]any{"query": map[string]any{"names": "otelcol"}})
				p.logs(1)
				return true, "", "" // the workload isn't up yet
			})
		})
		p.lifecycle()
		return 0
	})

	h.at(21 * time.Second)
	h.hook("update-status", nil, func(p *tapProc) int {
		p.reconcile(1100 * time.Millisecond)
		p.backlog(func() {
			p.observe(deferred, obsLoki, true, 0, func() (bool, string, string) {
				p.tool("relation-get", []string{"-r", "receive-loki-logs:7", "-", "loki/0"}, 3*time.Millisecond, 0)
				rules := p.event("alert_rules_changed", "AlertRulesChangedEvent", "custom")
				rules.path = "OpenTelemetryCollectorK8sCharm/LokiPushApiProvider[receive-loki-logs]/on/alert_rules_changed" + rules.handle()
				p.emit(rules, 1, []string{obsRules}, func() {
					p.observe(rules, obsRules, false, 1, func() (bool, string, string) {
						p.subprocess([]string{"/usr/bin/python3", "/var/lib/juju/agents/unit-otelcol-0/charm/src/render_rules.py"}, 240*time.Millisecond)
						p.pebble("POST", "/v1/files", 2*time.Millisecond, 200, map[string]any{"req_stream": true})
						p.pebble("POST", "/v1/services", 9*time.Millisecond, 202, map[string]any{"action": "restart", "services": []string{"otelcol"}, "req_bytes": 48})
						p.pebble("GET", "/v1/changes/31/wait", 640*time.Millisecond, 200, map[string]any{"query": map[string]any{"timeout": "4.000s"}})
						return false, "", ""
					})
				})
				return false, "", ""
			})
		})
		ev := p.event("update_status", "UpdateStatusEvent", "juju")
		p.emit(ev, 0, []string{obsStatus}, func() {
			p.observe(ev, obsStatus, false, 0, func() (bool, string, string) {
				p.pebble("GET", "/v1/checks", 1*time.Millisecond, 200, nil)
				return false, "", ""
			})
		})
		p.lifecycle()
		return 0
	})

	// A bad config value: the resource patch rejects it, Juju retries the
	// hook, and the third attempt runs after the value was fixed.
	for i, at := range []time.Duration{40 * time.Second, 45 * time.Second, 56 * time.Second} {
		fails := i < 2
		h.at(at)
		h.hook("config-changed", nil, func(p *tapProc) int {
			p.reconcile(1300 * time.Millisecond)
			p.backlog(nil)
			ev := p.event("config_changed", "ConfigChangedEvent", "juju")
			exc, msg := "", ""
			if fails {
				exc, msg = "ValueError", `invalid memory limit "2Gb": use a quantity such as "2Gi"`
			}
			p.emitErr(ev, 0, []string{obsResources, obsDashboard}, func() (string, string) {
				p.observe(ev, obsResources, false, 0, func() (bool, string, string) {
					p.logs(3)
					p.tool("config-get", []string{"--format=json"}, 5*time.Millisecond, 0)
					return false, exc, msg
				})
				if fails {
					return exc, msg
				}
				p.observe(ev, obsDashboard, false, 0, func() (bool, string, string) {
					p.advance(12 * time.Millisecond)
					return false, "", ""
				})
				return "", ""
			})
			if fails {
				p.exc, p.excMsg = exc, msg
				return 1
			}
			p.lifecycle()
			return 0
		})
	}
}

func opsFollower(h *tapHost) {
	h.at(2 * time.Second)
	h.dirty = true
	h.hook("config-changed", nil, func(p *tapProc) int {
		p.reconcile(1380 * time.Millisecond)
		p.backlog(nil)
		ev := p.event("config_changed", "ConfigChangedEvent", "juju")
		p.emit(ev, 0, []string{obsResources}, func() {
			p.observe(ev, obsResources, false, 0, func() (bool, string, string) {
				p.logs(14)
				return false, "", ""
			})
		})
		p.lifecycle()
		return 0
	})

	// Killed while it waits on the workload: no proc_end.
	h.at(30 * time.Second)
	p := h.start("update-status", nil)
	p.reconcileUntilKilled()
	h.hooks = append(h.hooks, hookSpan{p.start, h.now})
	h.out = append(h.out, h.record(unit.Record{Kind: unit.KindSpoolEnd, SpoolEnd: &unit.SpoolEnd{
		Host: h.host, Unit: h.unit, PID: p.pid, File: p.file, Lines: int64(p.lines), Truncated: true,
	}}))
}

// tapHost is one unit probe session: one Kubernetes pod.
type tapHost struct {
	model, unit, host string
	now               time.Time
	mono0             int64 // monotonic clock = wall ns - mono0
	nextPID           int
	contexts          int
	handles           int // ops event handles, which survive across hooks in the unit's store
	out               []UnitRecord
	hooks             []hookSpan    // when each hook process ran
	rssLog            []rssAt       // the hook processes' RSS, from their stack samples
	changes           []synthChange // the Pebble changes the hooks started
	dirty             bool          // the next replan restarts the workload
}

const agentPID = 17

func (h *tapHost) at(d time.Duration) {
	if t := h.out[0].Rec.TS.Add(d); t.After(h.now) {
		h.now = t
	}
}

func (h *tapHost) record(r unit.Record) UnitRecord {
	r.TS = h.now
	return UnitRecord{Host: h.host, Rec: r}
}

func (h *tapHost) hello() {
	h.out = append(h.out,
		h.record(unit.Record{Kind: unit.KindHello, Hello: &unit.Hello{
			Version: "synth", PID: 9, Substrate: "kubernetes", Agent: "containeragent",
			AgentPID: agentPID, Units: []string{h.unit},
		}}),
		h.record(unit.Record{Kind: unit.KindLoader, Loader: &unit.Loader{
			Event: unit.LoaderInstalled, Unit: h.unit, Loader: "_juju_lens_tap.pth",
			Dir: h.charmDir() + "/venv/lib/python3.12/site-packages",
		}}),
	)
}

func (h *tapHost) charmDir() string {
	return "/var/lib/juju/agents/unit-" + strings.ReplaceAll(h.unit, "/", "-") + "/charm"
}

// hook runs one hook process: body records what the charm did and returns
// the exit code.
func (h *tapHost) hook(name string, env map[string]string, body func(*tapProc) int) {
	p := h.start(name, env)
	rc := body(p)
	p.finish(rc)
	h.hooks = append(h.hooks, hookSpan{p.start, h.now})
}

// start records a hook process's start, up to dispatch.
func (h *tapHost) start(hook string, env map[string]string) *tapProc {
	h.contexts++
	e := map[string]string{
		"unit_name": h.unit, "model_name": h.model, "model_uuid": "00000000-0000-4000-8000-0000000000aa",
		"dispatch_path": "hooks/" + hook, "hook_name": hook, "charm_dir": h.charmDir(),
		"context_id": fmt.Sprintf("%s-%d", h.unit, h.contexts), "version": "3.6.12",
	}
	for k, v := range env {
		e[k] = v
	}
	p := h.proc(agentPID, []string{h.charmDir() + "/venv/bin/python", h.charmDir() + "/src/charm.py"}, e)
	p.startProfile()
	p.rec("ops_loaded", map[string]any{
		"version": "3.7.1", "path": h.charmDir() + "/venv/lib/python3.12/site-packages/ops/__init__.py",
		"otel": true, "ops_tracing": true,
		"patched": []string{"pebble.Client._request_raw", "ops.hookcmds._utils.subprocess", "Framework._reemit", "Framework._emit", "EventBase.defer", "ops._main.main"},
	})
	p.dispatch = h.now
	p.rec("dispatch_start", nil)
	return p
}

// proc records a tapped Python process's proc_start.
func (h *tapHost) proc(ppid int, argv []string, env map[string]string) *tapProc {
	h.nextPID++
	p := &tapProc{h: h, pid: h.nextPID, start: h.now}
	p.file = fmt.Sprintf("%s-%d-%d.jsonl", h.host, p.pid, h.now.UnixNano())
	features := []string{"ops", "proc"}
	if ppid == agentPID {
		features = append(features, "stacks") // hook processes are profiled
	}
	p.rec("proc_start", map[string]any{
		"pid": p.pid, "ppid": ppid, "uid": 0, "py": "3.12.3", "exe": argv[0], "argv": argv,
		"cwd": h.charmDir(), "features": features, "env": env,
	})
	return p
}

// tapProc is one process the tap recorded.
type tapProc struct {
	h           *tapHost
	pid         int
	file        string
	start       time.Time
	dispatch    time.Time
	lines       int
	exc, excMsg string

	// The stack sampler's view of the process (opsprof.go).
	prof     *tapProfile
	spans    []int  // the spans open, by tap number, outermost first
	nextSp   int    // the last span number given
	initOpen bool   // charm init is the innermost span
	obs      string // the observer running
	obsStack []string
	leaf     []synthFrame // what the code is in, under the charm's frames
	growth   int64        // how much RSS each sample in frDashload adds
}

// tapEvent is one ops event: its kind, class and handle path.
type tapEvent struct {
	kind, class, cls, path string
}

func (e tapEvent) handle() string { return e.path[strings.LastIndexByte(e.path, '['):] }

func (p *tapProc) advance(d time.Duration) {
	p.h.now = p.h.now.Add(d)
	p.sample(p.h.now)
}

// rec writes one tap record at the current time.
func (p *tapProc) rec(kind string, fields map[string]any) {
	h := p.h
	fields = p.spanRec(kind, fields)
	r := map[string]any{"t": h.now.UnixNano(), "m": h.now.UnixNano() - h.mono0, "k": kind}
	for k, v := range fields {
		r[k] = v
	}
	b, _ := json.Marshal(r)
	p.lines++
	h.out = append(h.out, h.record(unit.Record{Kind: unit.KindTap, Tap: &unit.Tap{
		Host: h.host, Unit: h.unit, PID: p.pid, File: p.file, Record: b,
	}}))
}

// call records a call the tap writes once, as it returns after d.
func (p *tapProc) call(kind string, d time.Duration, fields map[string]any) {
	p.leaf = frTool
	if kind == "pebble" {
		p.leaf = frPebble
	}
	p.advance(d)
	p.leaf = nil
	fields["dur_ns"] = d.Nanoseconds()
	p.rec(kind, fields)
	p.advance(300 * time.Microsecond)
}

func (p *tapProc) tool(name string, args []string, d time.Duration, rc int) {
	p.call("hook_tool", d, map[string]any{
		"tool": name, "args": args, "via": "ops.hookcmds._utils", "rc": rc, "out_bytes": 3, "err_bytes": 0,
	})
}

func (p *tapProc) logs(n int) {
	for i := range n {
		p.tool("juju-log", []string{"--log-level", "DEBUG", "--", fmt.Sprintf("<%d bytes>", 24+7*i%90)}, 2700*time.Microsecond, 0)
	}
}

func (p *tapProc) pebble(method, path string, d time.Duration, status int, extra map[string]any) {
	f := map[string]any{"method": method, "path": path, "status": status, "socket": "/charm/containers/otelcol/pebble.socket"}
	for k, v := range extra {
		f[k] = v
	}
	if status >= 400 {
		f["exc"], f["exc_msg"] = "APIError", "stat "+fmt.Sprint(extra["query"].(map[string]any)["path"])+": no such file or directory"
	}
	h := p.h
	switch {
	case path == "/v1/services" && extra["action"] == "replan" && h.dirty:
		h.dirty = false
		h.changes = append(h.changes, synthChange{h.now, 1200 * time.Millisecond, "replan", `Replan service "otelcol"`})
	case path == "/v1/services" && extra["action"] == "restart":
		h.changes = append(h.changes, synthChange{h.now, 900 * time.Millisecond, "restart", `Restart service "otelcol"`})
	}
	p.call("pebble", d, f)
}

func (p *tapProc) statusSet(status, msg string) {
	p.tool("status-set", []string{"--application=False", status, "--", msg}, 6*time.Millisecond, 0)
	p.rec("status_set", map[string]any{"status": status, "message": msg, "app": false, "rc": 0})
}

// reconcile is the charm's holistic reconcile in CharmBase.__init__, which
// takes most of each hook: it reads config and relations, checks the
// workload's files, pushes the config and replans, then waits for Pebble.
func (p *tapProc) reconcile(total time.Duration) {
	t0 := p.h.now
	p.logs(1)
	p.pebble("GET", "/v1/system-info", 700*time.Microsecond, 200, nil)
	p.tool("config-get", []string{"--format=json"}, 5*time.Millisecond, 0)
	p.logs(12)
	for _, ep := range []string{"receive-loki-logs", "send-loki-logs", "receive-traces", "send-traces", "metrics-endpoint", "send-remote-write", "grafana-dashboards-provider"} {
		p.tool("relation-ids", []string{ep, "--format=json"}, 2500*time.Microsecond, 0)
	}
	for _, f := range []string{"/etc/otelcol/otelcol-server-cert.crt", "/etc/otelcol/otelcol-private-key.key"} {
		p.pebble("GET", "/v1/files", 600*time.Microsecond, 404, map[string]any{"query": map[string]any{"path": f, "itself": "true", "action": "list"}})
	}
	p.pebble("POST", "/v1/exec", 15*time.Millisecond, 202, map[string]any{"command": "update-ca-certificates", "req_bytes": 218})
	p.pebble("GET", "/v1/changes/24/wait", 480*time.Millisecond, 200, map[string]any{"query": map[string]any{"timeout": "4.000s"}})
	p.logs(18)
	p.tool("is-leader", []string{"--format=json"}, 2500*time.Microsecond, 0)
	p.pebble("POST", "/v1/files", 2*time.Millisecond, 200, map[string]any{"req_stream": true})
	p.pebble("POST", "/v1/layers", 700*time.Microsecond, 200, map[string]any{"action": "add", "req_bytes": 974})
	p.pebble("POST", "/v1/services", 15*time.Millisecond, 202, map[string]any{"action": "replan", "services": []string{}, "req_bytes": 36})
	// Python building the collector config, then compressing the
	// dashboards to forward, which takes 17MiB: time the tap sees no call
	// in.
	if rest := total - p.h.now.Sub(t0) - 70*time.Millisecond; rest > 0 {
		p.leaf = frConfig
		p.advance(rest * 6 / 10)
		p.leaf, p.growth = frDashload, 17*mib/max(1, int64(rest*4/10/sampleEvery))
		p.advance(rest - rest*6/10)
		p.leaf = nil
	}
	p.pebble("GET", "/v1/changes/25/wait", 60*time.Millisecond, 200, map[string]any{"query": map[string]any{"timeout": "4.000s"}})
	p.statusSet("active", "")
	p.tool("application-version-set", []string{"--", "0.130.1"}, 5*time.Millisecond, 0)
}

// reconcileUntilKilled is a reconcile cut short by SIGKILL.
func (p *tapProc) reconcileUntilKilled() {
	p.logs(1)
	p.pebble("GET", "/v1/system-info", 700*time.Microsecond, 200, nil)
	p.tool("config-get", []string{"--format=json"}, 5*time.Millisecond, 0)
	p.logs(9)
	p.pebble("POST", "/v1/exec", 15*time.Millisecond, 202, map[string]any{"command": "update-ca-certificates", "req_bytes": 218})
	p.advance(2 * time.Second) // waiting on the change when the kernel kills it
}

// backlog records ops re-emitting deferred events: reemit runs the
// observers of each.
func (p *tapProc) backlog(reemit func()) {
	p.rec("reemit_start", nil)
	t0, count := p.h.now, 0
	if reemit != nil {
		reemit()
		count = 1
	}
	p.rec("reemit_backlog", map[string]any{"count": count, "dur_ns": p.h.now.Sub(t0).Nanoseconds()})
	p.advance(200 * time.Microsecond)
}

func (p *tapProc) event(kind, class, cls string) tapEvent {
	p.h.handles++
	path := fmt.Sprintf("OpenTelemetryCollectorK8sCharm/on/%s[%d]", kind, p.h.handles)
	if cls == "lifecycle" && (kind == "pre_commit" || kind == "commit") {
		path = fmt.Sprintf("on/%s[%d]", kind, p.h.handles)
	}
	return tapEvent{kind: kind, class: class, cls: cls, path: path}
}

func (p *tapProc) emit(ev tapEvent, depth int, observers []string, body func()) {
	p.emitErr(ev, depth, observers, func() (string, string) { body(); return "", "" })
}

// emitErr records Framework._emit of ev; body runs its observers and
// returns the exception that escaped them, if any.
func (p *tapProc) emitErr(ev tapEvent, depth int, observers []string, body func() (string, string)) {
	paths := make([]string, len(observers))
	for i, o := range observers {
		paths[i] = "OpenTelemetryCollectorK8sCharm/" + obsPath(o)
	}
	p.rec("emit", map[string]any{
		"ev": ev.path, "ev_kind": ev.kind, "ev_class": ev.class, "cls": ev.cls,
		"observers": paths, "depth": depth, "in_observer": nil,
	})
	t0 := p.h.now
	exc, msg := body()
	p.advance(100 * time.Microsecond)
	f := map[string]any{"ev": ev.path, "dur_ns": p.h.now.Sub(t0).Nanoseconds()}
	if exc != "" {
		f["exc"], f["exc_msg"] = exc, msg
	}
	p.rec("emit_end", f)
}

// observe records one observer of ev; body returns whether it deferred the
// event and the exception it raised, if any.
func (p *tapProc) observe(ev tapEvent, obs string, reemit bool, depth int, body func() (bool, string, string)) {
	method := obs[strings.LastIndexByte(obs, '.')+1:]
	p.rec("observer_start", map[string]any{
		"obs": obs, "obs_path": "OpenTelemetryCollectorK8sCharm", "method": method,
		"ev": ev.path, "ev_kind": ev.kind, "ev_class": ev.class, "cls": ev.cls, "reemit": reemit, "depth": depth,
	})
	t0 := p.h.now
	p.advance(400 * time.Microsecond)
	deferred, exc, msg := body()
	if deferred {
		p.rec("defer", map[string]any{"ev": ev.path, "ev_class": ev.class, "obs": obs})
	}
	p.advance(200 * time.Microsecond)
	f := map[string]any{"obs": obs, "ev": ev.path, "dur_ns": p.h.now.Sub(t0).Nanoseconds(), "deferred": deferred}
	if exc != "" {
		f["exc"], f["exc_msg"] = exc, msg
	}
	p.rec("observer_end", f)
}

// lifecycle records the events ops emits at the end of every dispatch.
func (p *tapProc) lifecycle() {
	for _, k := range [][2]string{{"collect_app_status", "CollectStatusEvent"}, {"collect_unit_status", "CollectStatusEvent"}, {"pre_commit", "PreCommitEvent"}} {
		p.emit(p.event(k[0], k[1], "lifecycle"), 0, nil, func() { p.advance(30 * time.Microsecond) })
	}
	commit := p.event("commit", "CommitEvent", "lifecycle")
	p.emit(commit, 0, []string{obsCommit}, func() {
		p.observe(commit, obsCommit, false, 0, func() (bool, string, string) { return false, "", "" })
	})
}

// subprocess records a tapped Python process the current observer starts
// and waits for.
func (p *tapProc) subprocess(argv []string, d time.Duration) {
	env := map[string]string{
		"unit_name": p.h.unit, "model_name": p.h.model, "model_uuid": "00000000-0000-4000-8000-0000000000aa",
		"dispatch_path": "hooks/update-status", "hook_name": "update-status", "charm_dir": p.h.charmDir(),
	}
	c := p.h.proc(p.pid, argv, env)
	c.advance(d)
	c.rec("proc_end", map[string]any{"pid": c.pid, "rc": 0})
	c.spoolEnd()
	p.advance(time.Millisecond)
}

// finish records dispatch and the process ending with exit code rc.
func (p *tapProc) finish(rc int) {
	f := map[string]any{"dur_ns": p.h.now.Sub(p.dispatch).Nanoseconds(), "rc": rc}
	if p.exc != "" {
		f["exc"], f["exc_msg"] = p.exc, p.excMsg
	}
	p.rec("dispatch_end", f)
	p.advance(400 * time.Microsecond)
	p.endProfile()
	end := map[string]any{"pid": p.pid, "rc": rc}
	if p.exc != "" {
		end["exc"], end["exc_msg"] = p.exc, p.excMsg
	}
	p.rec("proc_end", end)
	p.spoolEnd()
}

func (p *tapProc) spoolEnd() {
	h := p.h
	h.out = append(h.out, h.record(unit.Record{Kind: unit.KindSpoolEnd, SpoolEnd: &unit.SpoolEnd{
		Host: h.host, Unit: h.unit, PID: p.pid, File: p.file, Lines: int64(p.lines),
	}}))
}

// obsPath is how ops names an observer in an emit record: the object path
// and the method.
func obsPath(obs string) string {
	parts := strings.Split(obs, ".")
	if len(parts) < 2 {
		return obs
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
