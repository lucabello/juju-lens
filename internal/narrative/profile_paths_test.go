package narrative

import (
	"testing"

	"github.com/lucabello/juju-lens/internal/recording"
)

// A hook run as ./src/charm.py has the charm's own files under relative
// paths. They are the charm's code, as the absolute ones are, or a charm
// profile would drop the charm's own functions.
func TestCharmFilesByPath(t *testing.T) {
	for _, tc := range []struct {
		path  string
		charm bool
		short string
	}{
		{"/var/lib/juju/agents/unit-grafana-0/charm/src/charm.py", true, "src/charm.py"},
		{"/var/lib/juju/agents/unit-grafana-0/charm/lib/charms/x/v0/y.py", true, "lib/charms/x/v0/y.py"},
		{"./src/charm.py", true, "src/charm.py"},
		{"src/secret_storage.py", true, "src/secret_storage.py"},
		{"lib/charms/grafana_k8s/v0/grafana_source.py", true, "lib/charms/grafana_k8s/v0/grafana_source.py"},
		{"/var/lib/juju/agents/unit-grafana-0/charm/venv/lib/python3.8/site-packages/ops/main.py", false, "ops/main.py"},
		{"/var/lib/juju/agents/unit-grafana-0/charm/venv/lib/python3.8/site-packages/cosl/utils.py", false, "cosl/utils.py"},
		{"/usr/lib/python3.8/lzma.py", false, "lzma.py"},
		{"<frozen importlib._bootstrap>", false, "<frozen importlib._bootstrap>"},
		{"venv/lib/python3.8/site-packages/ops/main.py", false, "ops/main.py"},
	} {
		if got := IsCharmFile(tc.path); got != tc.charm {
			t.Errorf("IsCharmFile(%q) = %v, want %v", tc.path, got, tc.charm)
		}
		if got := ShortPath(tc.path); got != tc.short {
			t.Errorf("ShortPath(%q) = %q, want %q", tc.path, got, tc.short)
		}
	}
}

// The same function of the same charm in two units is one function of a
// profile over both, though the units' directories differ.
func TestProfileMergesTheSameCharmFileOfDifferentUnits(t *testing.T) {
	mk := func(unit string) recording.OpsProfile {
		return recording.OpsProfile{
			RootSpanID: unit,
			Frames:     []recording.ProfileFrame{{File: "/var/lib/juju/agents/unit-" + unit + "/charm/src/charm.py", Func: "_configure", Line: 10}},
			Stacks:     []recording.ProfileStack{{SpanID: unit, Frames: []int{0}, Samples: 1, WallNs: 5}},
		}
	}
	p := NewProfile([]recording.OpsProfile{mk("grafana-0"), mk("grafana-1")}, nil)
	if len(p.Frames) != 1 || len(p.Stacks) != 2 {
		t.Fatalf("%d frames for the same function in two units, want 1: %+v", len(p.Frames), p.Frames)
	}
	if !IsCharmFile(p.Frames[0].File) {
		t.Errorf("%q should still be the charm's code", p.Frames[0].File)
	}
}
