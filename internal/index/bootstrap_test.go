package index

import (
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// A show-unit document for two related apps (grafana ↔ prometheus over
// grafana-source), a multi-unit prometheus peer relation, and a single-unit
// grafana peer relation (no related-units — the case that used to be dropped).
const showUnitFixture = `{
  "grafana/0": {
    "relation-info": [
      {
        "relation-id": 7,
        "endpoint": "grafana-source",
        "related-endpoint": "grafana-source",
        "application-data": {"grafana_uid": "abc"},
        "local-unit": {"in-scope": true, "data": {"private-address": "10.0.0.1"}},
        "related-units": {
          "prometheus/0": {"in-scope": true, "data": {"grafana_source_host": "http://p"}}
        }
      },
      {
        "relation-id": 5,
        "endpoint": "grafana-peers",
        "related-endpoint": "grafana-peers",
        "application-data": {"leader": "grafana/0"},
        "local-unit": {"in-scope": true, "data": {"ready": "true"}},
        "related-units": {}
      }
    ]
  },
  "prometheus/0": {
    "relation-info": [
      {
        "relation-id": 7,
        "endpoint": "grafana-source",
        "related-endpoint": "grafana-source",
        "application-data": {"scrape": "cfg"},
        "local-unit": {"data": {"grafana_source_host": "http://p"}},
        "related-units": {"grafana/0": {"data": {"private-address": "10.0.0.1"}}}
      },
      {
        "relation-id": 3,
        "endpoint": "prometheus-peers",
        "related-endpoint": "prometheus-peers",
        "application-data": {},
        "local-unit": {"data": {"role": "leader"}},
        "related-units": {"prometheus/1": {"data": {"role": "follower"}}}
      }
    ]
  }
}`

func TestDatabagBootstrapSnapshots(t *testing.T) {
	ts := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	got := DatabagBootstrapSnapshots("cos", []byte(showUnitFixture), ts)

	gotScopes := make([]string, 0, len(got))
	for _, s := range got {
		if s.Kind != KindDatabag {
			t.Errorf("scope %s kind = %s, want databag", s.Scope, s.Kind)
		}
		if s.Model != "cos" || !s.Ts.Equal(ts) {
			t.Errorf("scope %s wrong model/ts: %+v", s.Scope, s)
		}
		gotScopes = append(gotScopes, s.Scope)
	}
	sort.Strings(gotScopes)

	// The grafana-source relation collapses to one canonical key regardless of
	// which unit reported it; the peer relation is a single segment.
	const rel = "grafana.grafana-source#prometheus.grafana-source"
	want := []string{
		"databag:" + rel + ":grafana",                      // grafana app data
		"databag:" + rel + ":grafana/0",                    // grafana/0 unit data
		"databag:" + rel + ":prometheus",                   // prometheus app data
		"databag:" + rel + ":prometheus/0",                 // prometheus/0 unit data
		"databag:grafana.grafana-peers:grafana",            // single-unit peer app data
		"databag:grafana.grafana-peers:grafana/0",          // single-unit peer unit data
		"databag:prometheus.prometheus-peers:prometheus/0", // peer local
		"databag:prometheus.prometheus-peers:prometheus/1", // peer related
	}
	sort.Strings(want)
	if len(gotScopes) != len(want) {
		t.Fatalf("scopes = %v\nwant   %v", gotScopes, want)
	}
	for i := range want {
		if gotScopes[i] != want[i] {
			t.Errorf("scope[%d] = %s, want %s", i, gotScopes[i], want[i])
		}
	}
}

// A combined `juju config` document for one app: log_level has a value,
// cpu is unset (no value field, only a default-less definition).
const configFixture = `{
  "grafana": {
    "application": "grafana",
    "charm": "grafana-k8s",
    "settings": {
      "log_level": {"type": "string", "value": "info", "source": "default"},
      "reporting_enabled": {"type": "boolean", "value": true, "source": "user"},
      "cpu": {"type": "string", "source": "unset"}
    }
  }
}`

func TestConfigBootstrapSnapshots(t *testing.T) {
	ts := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	got := ConfigBootstrapSnapshots("cos", []byte(configFixture), ts)
	if len(got) != 1 {
		t.Fatalf("want 1 config snapshot, got %d: %+v", len(got), got)
	}
	s := got[0]
	if s.Kind != KindConfig || s.Scope != "config:grafana" {
		t.Errorf("kind/scope = %s/%s", s.Kind, s.Scope)
	}
	// Body is the flat {key: value} map the RPC extractor also produces — an
	// unset option becomes null, matching ConfigSettings' effective view.
	var body map[string]any
	if err := json.Unmarshal(s.Body, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, s.Body)
	}
	if body["log_level"] != "info" || body["reporting_enabled"] != true {
		t.Errorf("body = %s", s.Body)
	}
	if v, ok := body["cpu"]; !ok || v != nil {
		t.Errorf("unset option should be null, got %v (present=%v)", v, ok)
	}
}

// TestBootstrapMatchesRPCScope locks the invariant the whole feature rests on:
// a databag captured at bootstrap lands on the exact same scope the RPC
// extractor produces for the same relation, so they dedup instead of doubling.
func TestBootstrapMatchesRPCScope(t *testing.T) {
	ts := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	boots := DatabagBootstrapSnapshots("cos", []byte(showUnitFixture), ts)
	bootScope := ""
	for _, s := range boots {
		if s.Scope == "databag:grafana.grafana-source#prometheus.grafana-source:grafana/0" {
			bootScope = s.Scope
		}
	}
	if bootScope == "" {
		t.Fatal("bootstrap did not produce the grafana/0 databag scope")
	}
	// The RPC path names the relation the other way round in its tag; it must
	// still canonicalise to the same scope.
	rpc := statusSpan("s1", "CommitHookChanges", "grafana/0", ts,
		`{"args":[{"tag":"unit-grafana-0","relation-unit-settings":[`+
			`{"relation":"relation-prometheus.grafana-source#grafana.grafana-source","unit":"unit-grafana-0",`+
			`"settings":{"private-address":"10.0.0.1"}}]}]}`)
	rpcSnaps := databagSnapshots(rpc, rpc.Attrs["params"])
	if len(rpcSnaps) != 1 || rpcSnaps[0].Scope != bootScope {
		t.Fatalf("RPC scope %v does not match bootstrap scope %q", rpcSnaps, bootScope)
	}
}
