package index

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// statusSpan builds a synthesised span the way recording.LoadSpans would for a
// Uniter status-setter RPC: facade/method/params live in Attrs.
func statusSpan(id, method, unit string, start time.Time, params string) recording.SpanRow {
	return recording.SpanRow{
		SpanID: id, Model: "default", Unit: unit,
		Start: start, End: start,
		Name: "Uniter." + method,
		Attrs: map[string]string{
			"facade": "Uniter",
			"method": method,
			"params": params,
		},
	}
}

func TestExtractSnapshotsFromStatusRPCs(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		// A hook commit that sets no status. Must be ignored.
		{
			SpanID: "01", Model: "default", Unit: "grafana/0", Start: base, End: base,
			Name:  "Uniter.CommitHookChanges",
			Attrs: map[string]string{"facade": "Uniter", "method": "CommitHookChanges", "params": `{"changes":[]}`},
		},
		statusSpan("02", "SetStatus", "grafana/0", base.Add(200*time.Millisecond),
			`{"entities":[{"tag":"unit-grafana-0","status":"active","info":"Ready"}]}`),
		statusSpan("03", "SetApplicationStatus", "grafana/0", base.Add(210*time.Millisecond),
			`{"entities":[{"tag":"application-grafana","status":"active","info":"All units ready"}]}`),
		// prometheus workload status, no message.
		statusSpan("04", "SetStatus", "prometheus/0", base.Add(300*time.Millisecond),
			`{"entities":[{"tag":"unit-prometheus-0","status":"waiting"}]}`),
		// A non-Uniter facade must be ignored.
		{
			SpanID: "05", Model: "default", Start: base.Add(400 * time.Millisecond), End: base.Add(400 * time.Millisecond),
			Name:  "Client.FullStatus",
			Attrs: map[string]string{"facade": "Client", "method": "FullStatus"},
		},
	}

	got := ExtractSnapshots(spans)
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d: %+v", len(got), got)
	}

	want := []struct {
		kind  SnapshotKind
		scope string
		value string
	}{
		{KindUnitStatus, "unit-status:grafana/0", "active"},
		{KindAppStatus, "app-status:grafana", "active"},
		{KindUnitStatus, "unit-status:prometheus/0", "waiting"},
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Scope != w.scope {
			t.Errorf("snap[%d] kind/scope = %s/%s, want %s/%s", i, got[i].Kind, got[i].Scope, w.kind, w.scope)
		}
		var body statusBody
		if err := json.Unmarshal(got[i].Body, &body); err != nil {
			t.Fatalf("snap[%d] body not JSON: %v (%s)", i, err, got[i].Body)
		}
		if body.Value != w.value {
			t.Errorf("snap[%d] value = %s, want %s", i, body.Value, w.value)
		}
		if body.Since.IsZero() {
			t.Errorf("snap[%d] since must be set", i)
		}
		if got[i].ProducingSpanID == "" {
			t.Errorf("snap[%d] must carry producing span id", i)
		}
	}
}

// Some real controllers send SetApplicationStatus with the leader unit's own
// tag rather than an application tag. That must still land as the bare
// application's app-status, not a phantom "app" named after the unit (a real
// capture surfaced exactly this: a "mimir/2" entry in the Applications
// section, sitting next to "mimir").
func TestExtractAppStatusFromUnitTag(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		statusSpan("01", "SetApplicationStatus", "mimir/2", base,
			`{"entities":[{"tag":"unit-mimir-2","status":"active","info":"ready"}]}`),
	}
	got := ExtractSnapshots(spans)
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot, got %d: %+v", len(got), got)
	}
	if got[0].Kind != KindAppStatus || got[0].Scope != "app-status:mimir" {
		t.Errorf("kind/scope = %s/%s, want %s/app-status:mimir", got[0].Kind, got[0].Scope, KindAppStatus)
	}
}

// SetAgentStatus (executing/idle) must extract into its own agent-status scope,
// distinct from the workload unit-status scope, so the sidebar and Status pane
// draw from different axes. This guards against the recorder/extractor ever
// dropping agent status (it is not recoverable from a recording that omits it).
func TestExtractAgentStatusScope(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		statusSpan("01", "SetAgentStatus", "grafana/0", base,
			`{"entities":[{"tag":"unit-grafana-0","status":"executing","info":"running config-changed hook"}]}`),
		statusSpan("02", "SetUnitStatus", "grafana/0", base.Add(time.Second),
			`{"entities":[{"tag":"unit-grafana-0","status":"active","info":"Ready"}]}`),
		statusSpan("03", "SetAgentStatus", "grafana/0", base.Add(2*time.Second),
			`{"entities":[{"tag":"unit-grafana-0","status":"idle"}]}`),
	}
	got := ExtractSnapshots(spans)
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d: %+v", len(got), got)
	}
	want := []struct {
		kind  SnapshotKind
		scope string
		value string
	}{
		{KindAgentStatus, "agent-status:grafana/0", "executing"},
		{KindUnitStatus, "unit-status:grafana/0", "active"},
		{KindAgentStatus, "agent-status:grafana/0", "idle"},
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Scope != w.scope {
			t.Errorf("snap[%d] kind/scope = %s/%s, want %s/%s", i, got[i].Kind, got[i].Scope, w.kind, w.scope)
		}
		var body statusBody
		if err := json.Unmarshal(got[i].Body, &body); err != nil {
			t.Fatalf("snap[%d] body not JSON: %v", i, err)
		}
		if body.Value != w.value {
			t.Errorf("snap[%d] value = %s, want %s", i, body.Value, w.value)
		}
	}
}

func TestDatabagSnapshotsFromCommitHookChanges(t *testing.T) {
	base := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	params := `{"args":[{"tag":"unit-loki-0","update-network-info":false,` +
		`"relation-unit-settings":[` +
		`{"relation":"relation-loki.certificates#ca.certificates","unit":"unit-loki-0",` +
		`"settings":{"csr":"PEM"},"application-settings":null},` +
		`{"relation":"relation-prometheus.metrics-endpoint#loki.metrics-endpoint","unit":"unit-loki-0",` +
		`"settings":{"addr":"10.0.0.1"},"application-settings":{"scrape":"cfg"}},` +
		`{"relation":"relation-x#y","unit":"unit-loki-0","settings":{},"application-settings":null}]}]}`
	sp := statusSpan("s1", "CommitHookChanges", "loki/0", base, params)
	got := databagSnapshots(sp, params)

	// Expect: loki/0 unit databag on both non-empty relations, plus the loki
	// application databag on the second (application-settings present). The
	// empty-settings third relation yields nothing.
	// Scopes carry the canonical (sorted-segment) relation key.
	want := map[string]string{
		"databag:ca.certificates#loki.certificates:loki/0":                 `{"csr":"PEM"}`,
		"databag:loki.metrics-endpoint#prometheus.metrics-endpoint:loki/0": `{"addr":"10.0.0.1"}`,
		"databag:loki.metrics-endpoint#prometheus.metrics-endpoint:loki":   `{"scrape":"cfg"}`,
	}
	// Writing the application databag also reveals leadership: loki/0 leads loki.
	var leader []Snapshot
	var databags []Snapshot
	for _, s := range got {
		if s.Kind == KindLeadership {
			leader = append(leader, s)
			continue
		}
		databags = append(databags, s)
	}
	if len(leader) != 1 || leader[0].Scope != "leadership:loki" || string(leader[0].Body) != `{"value":"loki/0","since":"2026-07-04T14:00:00Z"}` {
		t.Errorf("expected one leadership:loki snapshot for loki/0, got %+v", leader)
	}
	got = databags
	if len(got) != len(want) {
		t.Fatalf("expected %d databag snapshots, got %d: %+v", len(want), len(got), got)
	}
	for _, s := range got {
		if s.Kind != KindDatabag {
			t.Errorf("scope %s kind = %s, want databag", s.Scope, s.Kind)
		}
		wb, ok := want[s.Scope]
		if !ok {
			t.Errorf("unexpected scope %s", s.Scope)
			continue
		}
		if string(s.Body) != wb {
			t.Errorf("scope %s body = %s, want %s", s.Scope, s.Body, wb)
		}
		if s.ProducingSpanID != "s1" {
			t.Errorf("scope %s missing producing span id", s.Scope)
		}
	}
}

// configSpan builds a span for a Uniter.ConfigSettings RPC: the config value
// lives in the response envelope, not the params.
func configSpan(id, unit, response string) recording.SpanRow {
	return recording.SpanRow{
		SpanID: id, Model: "default", Unit: unit,
		Name: "Uniter.ConfigSettings",
		Attrs: map[string]string{
			"facade":   "Uniter",
			"method":   "ConfigSettings",
			"params":   `{"entities":[{"tag":"unit-` + unit + `-0"}]}`,
			"response": response,
		},
	}
}

func TestConfigSnapshotsFromConfigSettings(t *testing.T) {
	// A populated config becomes one app-scoped config snapshot from the
	// response's results[0].settings.
	sp := configSpan("c1", "grafana",
		`{"results":[{"settings":{"log_level":"info","reporting_enabled":true}}]}`)
	got := SnapshotsForSpan(sp)
	if len(got) != 1 {
		t.Fatalf("expected 1 config snapshot, got %d: %+v", len(got), got)
	}
	s := got[0]
	if s.Kind != KindConfig || s.Scope != "config:grafana" {
		t.Errorf("kind/scope = %s/%s, want config/config:grafana", s.Kind, s.Scope)
	}
	if s.ProducingSpanID != "c1" {
		t.Errorf("missing producing span id: %+v", s)
	}
	var body map[string]any
	if err := json.Unmarshal(s.Body, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, s.Body)
	}
	if body["log_level"] != "info" {
		t.Errorf("body = %s, want log_level=info", s.Body)
	}

	// Empty settings (config-less/subordinate charm) yield nothing.
	if got := SnapshotsForSpan(configSpan("c2", "empty", `{"results":[{"settings":{}}]}`)); len(got) != 0 {
		t.Errorf("empty settings should yield no snapshot, got %+v", got)
	}
}

func TestRelationForSpan(t *testing.T) {
	cases := map[string]string{
		`{"relation":"relation-a.b#c.d","unit":"unit-a-0"}`:                             "a.b#c.d",
		`{"relation-unit-pairs":[{"relation":"relation-e.f#g.h","local-unit":"unit"}]}`: "e.f#g.h",
		`{"args":[{"relation-unit-settings":[{"relation":"relation-i.j#k.l"}]}]}`:       "i.j#k.l",
		`{"entities":[{"tag":"unit-a-0"}]}`:                                             "",
	}
	for params, want := range cases {
		sp := recording.SpanRow{Attrs: map[string]string{"params": params}}
		if got := RelationForSpan(sp); got != want {
			t.Errorf("RelationForSpan(%s) = %q, want %q", params, got, want)
		}
	}
}

func TestEntityName(t *testing.T) {
	cases := map[string]string{
		"unit-grafana-0":         "grafana/0",
		"unit-nova-compute-3":    "nova-compute/3",
		"application-prometheus": "prometheus",
		"machine-0":              "",
	}
	for tag, want := range cases {
		if got := entityName(tag); got != want {
			t.Errorf("entityName(%q) = %q, want %q", tag, got, want)
		}
	}
}
