package cli

import (
	"reflect"
	"testing"
)

const scopeStatus = `{"applications":{
 "zookeeper":{"units":{"zookeeper/0":{"machine":"1","subordinates":{"otelcol/0":{}}}}},
 "jammy":{"units":{"jammy/0":{"machine":"2","subordinates":{"otelcol/1":{}}}}},
 "resolute":{"units":{"resolute/0":{"machine":"0"}}},
 "otelcol":{"subordinate-to":["zookeeper","jammy"]}
}}`

// TestUnitScopeSelectsMachinesOfUnitsAndSubordinates: on a machine model the
// probes run per machine, so a selected subordinate must pull in its
// principal's machine, and an application selects all its units' machines.
func TestUnitScopeSelectsMachinesOfUnitsAndSubordinates(t *testing.T) {
	cases := []struct {
		apps, units []string
		want        []string
	}{
		{nil, []string{"resolute/0"}, []string{"0"}},
		{[]string{"zookeeper"}, nil, []string{"1"}},
		{[]string{"otelcol"}, nil, []string{"1", "2"}},
		{nil, []string{"otelcol/1"}, []string{"2"}},
		{[]string{"jammy"}, []string{"resolute/0"}, []string{"0", "2"}},
		{nil, []string{"zookeeper/5"}, []string{}},
	}
	for _, c := range cases {
		s, err := newUnitScope(c.apps, c.units)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseStatusMachines([]byte(scopeStatus), s)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("apps %v units %v: machines %v, want %v", c.apps, c.units, got, c.want)
		}
	}
}

// TestUnitScopeNames: names that mix up --app and --unit are refused rather
// than matching nothing, and the debug-log filter names what was asked for.
func TestUnitScopeNames(t *testing.T) {
	for _, bad := range [][2][]string{{{"grafana/0"}, nil}, {nil, {"grafana"}}, {nil, {"grafana/x"}}, {{""}, nil}} {
		if _, err := newUnitScope(bad[0], bad[1]); err == nil {
			t.Errorf("apps %v units %v: accepted", bad[0], bad[1])
		}
	}
	s, _ := newUnitScope([]string{"loki"}, []string{"grafana/0"})
	want := []string{"--include", "loki", "--include", "unit-grafana-0"}
	if got := s.debugLogInclude(); !reflect.DeepEqual(got, want) {
		t.Errorf("debug-log args %v, want %v", got, want)
	}
	if !s.has("loki/3") || !s.has("grafana/0") || s.has("grafana/1") {
		t.Error("has: want every loki unit and only grafana/0")
	}
	if !(unitScope{}).has("any/0") {
		t.Error("an empty scope must select everything")
	}
}
