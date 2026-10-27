package recording

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// The res-* fixtures are unit probe files cut from lab recordings with
// resource measurement (2026-10-22): each keeps the hello, the first
// resources records, the Pebble changes, and a few executions with their
// tap records.
//   - res-k8s: opentelemetry-collector-k8s (config-changed with a replan,
//     update-status) and loki-k8s (update-status, `juju exec -- sleep 2`).
//   - res-machine: the opentelemetry-collector subordinate and its ubuntu
//     principal on an LXD container (update-status each, `juju exec --
//     sleep 2` on ubuntu/0).

func TestOpsRootsCarryResources(t *testing.T) {
	for _, c := range []struct {
		fixture, cgroup string
		workloads       bool
	}{
		{"res-k8s", "charm", true},
		{"res-machine", "agent", false},
	} {
		spans := loadFixture(t, c.fixture)
		var tapped, polled int
		for _, r := range roots(spans) {
			a := r.Attrs
			if a["res_cgroup"] != c.cgroup {
				t.Errorf("%s %s %s: res_cgroup %q, want %q", c.fixture, r.Unit, r.Name, a["res_cgroup"], c.cgroup)
			}
			if (a["res_workloads"] != "") != c.workloads {
				t.Errorf("%s %s %s: res_workloads %q", c.fixture, r.Unit, r.Name, a["res_workloads"])
			}
			switch a[AttrResSource] {
			case unit.SourceTap:
				tapped++
				// The tap's own boundaries, and the process tree's CPU.
				if a["exec"] != ExecHook || a["rc"] != "0" || a["res_cpu_ms"] == "0" || a["res_end_slack_ms"] != "" {
					t.Errorf("%s %s %s: %v", c.fixture, r.Unit, r.Name, a)
				}
				if ps, _ := time.Parse(time.RFC3339Nano, a["res_proc_start"]); ps.After(r.Start) {
					t.Errorf("%s %s %s: process start %v after the tap's %v", c.fixture, r.Unit, r.Name, ps, r.Start)
				}
			case unit.SourceProc:
				polled++
				// `juju exec -- sleep 2`: found by the poll alone, a hook
				// the tap missed.
				_, kids := tree(spans, r.TraceID)
				if r.Name != "juju-exec action" || a["exec"] != ExecAction || a["tap"] != TapMissing || a["seen_by"] != SeenByProc ||
					a["res_end_slack_ms"] == "" || len(kids[r.SpanID]) != 0 {
					t.Errorf("%s: polled root %s %v", c.fixture, r.Name, a)
				}
				if d := r.End.Sub(r.Start); d < 2*time.Second || d > 2200*time.Millisecond {
					t.Errorf("%s: juju-exec lasted %v, want sleep 2's 2 s", c.fixture, d)
				}
			default:
				t.Errorf("%s %s %s: no resource attributes", c.fixture, r.Unit, r.Name)
			}
		}
		if tapped < 2 || polled != 1 {
			t.Errorf("%s: %d tapped and %d polled roots, want 2+ and 1", c.fixture, tapped, polled)
		}
	}
}

func TestOpsResourcesWorkloads(t *testing.T) {
	spans := loadFixture(t, "res-k8s")
	cc := rootNamed(t, roots(spans), "config-changed", 0)
	var wl []unit.ContainerUsage
	if err := json.Unmarshal([]byte(cc.Attrs["res_workloads"]), &wl); err != nil || len(wl) != 1 {
		t.Fatalf("res_workloads %q: %v", cc.Attrs["res_workloads"], err)
	}
	// The charm replanned otelcol, which restarted: the workload's CPU
	// during the hook is the collector starting up.
	if w := wl[0]; w.Container != "otelcol" || w.CPUMs < 100 || w.MemSamples == 0 || w.Error != "" {
		t.Errorf("otelcol during config-changed: %+v", w)
	}

	res, err := LoadUnitResources(filepath.Join("testdata", "ops", "res-k8s"))
	if err != nil {
		t.Fatal(err)
	}
	containers := map[string]bool{}
	for _, s := range res.Samples {
		containers[s.Host+"/"+s.Container] = true
		if s.Unit == "" {
			t.Errorf("%s/%s: no unit", s.Host, s.Container)
		}
	}
	for _, want := range []string{"otelcol-0/charm", "otelcol-0/otelcol", "otelcol-0/" + ProbeContainer, "loki-0/loki", "loki-0/node-exporter"} {
		if !containers[want] {
			t.Errorf("no samples of %s; have %v", want, containers)
		}
	}
	replans := 0
	for _, ch := range res.Changes {
		if ch.Host == "otelcol-0" && ch.Kind == "replan" {
			replans++
		}
	}
	if replans == 0 || len(res.Disks) == 0 {
		t.Errorf("%d otelcol replans, %d disk samples", replans, len(res.Disks))
	}
}

// mem-k8s is cut from a lab recording (2026-10-22) of
// opentelemetry-collector-k8s whose config change (debug_exporter_for_metrics)
// made the charm restart otelcol: one config-changed, the workload's burst
// readings until 30 s after the replan was ready, and its Pebble changes.
func TestOpsMemoryPerSpan(t *testing.T) {
	spans := loadFixture(t, "mem-k8s")
	cc := rootNamed(t, roots(spans), "config-changed", 0)
	_, kids := tree(spans, cc.TraceID)
	n := 0
	for _, sp := range append([]SpanRow{cc}, spans...) {
		if sp.TraceID != cc.TraceID {
			continue
		}
		switch sp.Attrs["kind"] {
		case OpsRoot, OpsInit, OpsEvent, OpsObserver:
		default:
			continue
		}
		n++
		start, _ := strconv.ParseInt(sp.Attrs[AttrRSSStart], 10, 64)
		end, _ := strconv.ParseInt(sp.Attrs[AttrRSSEnd], 10, 64)
		peak, _ := strconv.ParseInt(sp.Attrs[AttrRSSPeak], 10, 64)
		if start == 0 || end == 0 || peak < max(start, end) {
			t.Errorf("%s %s: rss start %d end %d peak %d", sp.Attrs["kind"], sp.Name, start, end, peak)
		}
	}
	if n < 5 || len(kids[cc.SpanID]) == 0 {
		t.Fatalf("%d spans with memory", n)
	}
	// Charm init builds the whole config: the process's high-water mark
	// rose there, so its peak is exact, and above where it ended.
	init := childrenOfKind(kids, cc.SpanID, OpsInit)[0]
	if init.Attrs[AttrRSSHWMExact] != "true" || init.Attrs[AttrRSSPeak] <= init.Attrs[AttrRSSEnd] {
		t.Errorf("init memory: %v", init.Attrs)
	}
	if cc.Attrs["res_cgroup_mem_peak_exact"] == "" || cc.Attrs["res_cgroup_mem_peak_source"] == "" {
		t.Errorf("root lacks the cgroup peak's source: %v", cc.Attrs)
	}

	res, err := LoadUnitResources(filepath.Join("testdata", "ops", "mem-k8s"))
	if err != nil {
		t.Fatal(err)
	}
	var burst, probe int
	for _, s := range res.Samples {
		switch {
		case s.Container == ProbeContainer:
			probe++
		case s.MemStat == nil || s.MemEvents == nil:
			t.Errorf("%s at %v: no memory.stat or memory.events", s.Container, s.TS)
		case s.Container == "otelcol":
			burst++
		}
	}
	// Bursts: about 10 a second for the hook's 5 s and the 30 s after.
	if burst < 300 || probe > 20 {
		t.Errorf("%d otelcol samples, %d probe samples", burst, probe)
	}
}
