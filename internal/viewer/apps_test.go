package viewer

import (
	"testing"

	"github.com/lucabello/juju-lens/internal/recording"
)

func TestBuildAppTreeIgnoresUnitlessSpans(t *testing.T) {
	spans := []recording.SpanRow{
		{Unit: "grafana/0"},
		{Unit: "grafana/0"}, // duplicate must not double up
		{Unit: "grafana/1"},
		{Unit: "prometheus/0"},
		{Unit: ""}, // controller-side facade span
	}
	tree := buildAppTree(spans)
	if len(tree.apps) != 2 || tree.apps[0] != "grafana" || tree.apps[1] != "prometheus" {
		t.Fatalf("apps in unexpected order: %v", tree.apps)
	}
	if len(tree.units["grafana"]) != 2 {
		t.Fatalf("grafana should have 2 unique units, got %v", tree.units["grafana"])
	}
	if tree.units["grafana"][0] != "grafana/0" {
		t.Fatalf("units must be sorted, got %v", tree.units["grafana"])
	}
	if len(tree.units["prometheus"]) != 1 {
		t.Fatalf("prometheus should have 1 unit, got %v", tree.units["prometheus"])
	}
}

func TestBuildAppTreeEmpty(t *testing.T) {
	tree := buildAppTree(nil)
	if len(tree.apps) != 0 {
		t.Fatalf("nil spans should yield empty tree, got %v", tree.apps)
	}
}

func TestAppOfHandlesEdgeCases(t *testing.T) {
	cases := map[string]string{
		"grafana/0":            "grafana",
		"grafana-agent-k8s/12": "grafana-agent-k8s",
		"":                     "",
		"controller":           "controller", // no slash → the whole thing
	}
	for in, want := range cases {
		if got := appOf(in); got != want {
			t.Errorf("appOf(%q) = %q, want %q", in, got, want)
		}
	}
}
