package narrative

import (
	"strings"
	"testing"
)

func TestPrettyJSONExpandsNestedJSONAndYAML(t *testing.T) {
	// A databag whose values are (1) a JSON string and (2) a multi-line YAML
	// blob should both be expanded into structure.
	body := `{"uids":"{\"prometheus/0\":\"uid-1\"}","state":"id: 7\napplication-members:\n  prometheus: 0\n","addr":"10.0.0.1"}`
	got := PrettyJSON(body)
	for _, want := range []string{
		`"prometheus/0": "uid-1"`, // nested JSON expanded
		`"application-members"`,   // nested YAML expanded
		`"prometheus": 0`,         // nested YAML leaf
		`"addr": "10.0.0.1"`,      // plain scalar left intact (not YAML-mangled)
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PrettyJSON missing %q\n---\n%s", want, got)
		}
	}
	// The escaped-JSON string must not survive verbatim.
	if strings.Contains(got, `\"prometheus/0\"`) {
		t.Errorf("nested JSON was not expanded:\n%s", got)
	}
}

func TestLineDiff(t *testing.T) {
	a := []string{"{", `  "x": 1`, "}"}
	b := []string{"{", `  "x": 2`, `  "y": 3`, "}"}
	got := LineDiff(a, b)
	var sb strings.Builder
	for _, d := range got {
		sb.WriteByte(d.Op)
	}
	// context, remove x:1, add x:2, add y:3, context  -> " -++ "
	if ops := sb.String(); ops != " -++ " {
		t.Errorf("LineDiff ops = %q, want %q", ops, " -++ ")
	}
}

func TestRelationLabelLocalSideFirst(t *testing.T) {
	cases := []struct {
		key, local, want string
	}{
		{"grafana.grafana-source#prometheus.grafana-source", "grafana",
			"grafana:grafana-source → prometheus:grafana-source"},
		{"ca.certificates#loki.certificates", "loki",
			"loki:certificates → ca:certificates"},
		{"mimir.mimir-peers", "mimir", "mimir:mimir-peers (peer)"},
	}
	for _, c := range cases {
		if got := RelationLabel(c.key, c.local); got != c.want {
			t.Errorf("RelationLabel(%q, %q) = %q, want %q", c.key, c.local, got, c.want)
		}
	}
}
