package viewer

import "testing"

func TestDeriveAppStatuses(t *testing.T) {
	units := map[string]statusValue{
		"grafana/0": {Value: "active", Known: true},
		"grafana/1": {Value: "blocked", Message: "waiting for db", Known: true},
		"loki/0":    {Value: "active", Known: true},
		"loki/1":    {Value: "active", Known: true},
		"mimir/0":   {Value: "maintenance", Known: true},
		"mimir/1":   {Value: "waiting", Known: true},
		"tempo/0":   {Known: false}, // no snapshot yet, ignored
	}
	got := deriveAppStatuses(units)

	if got["grafana"].Value != "blocked" {
		t.Errorf("grafana: want blocked, got %q", got["grafana"].Value)
	}
	if got["grafana"].Message != "waiting for db" {
		t.Errorf("grafana message not carried from winning unit: %q", got["grafana"].Message)
	}
	if got["loki"].Value != "active" {
		t.Errorf("loki: want active, got %q", got["loki"].Value)
	}
	if got["mimir"].Value != "maintenance" {
		t.Errorf("mimir: want maintenance (higher than waiting), got %q", got["mimir"].Value)
	}
	if _, ok := got["tempo"]; ok {
		t.Errorf("tempo has no known units, should be omitted")
	}
}

func TestDeriveAppStatusesEmpty(t *testing.T) {
	if len(deriveAppStatuses(map[string]statusValue{})) != 0 {
		t.Errorf("empty input should derive no app statuses")
	}
}
