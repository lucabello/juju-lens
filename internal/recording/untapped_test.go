package recording

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// The untapped-k8s fixture is a lab recording (`record --attach unit`, model
// lens-t9 on a microk8s controller, 2026-10-22) of opentelemetry-collector-k8s
// and bashy, a charm whose dispatch is a bash script. In order: bashy's
// config-changed; juju exec on both units; the reconcile action; an otelcol
// config-changed; another one whose process was killed with SIGKILL right
// after it started, and Juju's retry; then `juju refresh`, which recreated
// the otelcol-0 pod: the old pod's stop, then upgrade-charm, config-changed
// and start before the new session installed the tap, and pebble-ready
// after; and a last juju exec.
const untappedK8s = "untapped-k8s"

func fixtureDir(name string) string { return filepath.Join("testdata", "ops", name) }

func fixtureStart(t *testing.T, name string) time.Time {
	t.Helper()
	m, err := Load(fixtureDir(name))
	if err != nil {
		t.Fatal(err)
	}
	return m.Started
}

func TestStatusLogSightings(t *testing.T) {
	got, err := LoadStatusLog(fixtureDir(untappedK8s), fixtureStart(t, untappedK8s))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range got {
		lines = append(lines, s.Start.Format("15:04:05")+" "+s.Unit+" "+s.Exec+" "+s.Name+" "+s.Kind)
	}
	// The deploy's hooks are in the log too, but before the recording.
	want := []string{
		"22:20:06 bashy/0 hook config-changed config-changed",
		"22:20:21 otelcol/0 action juju-exec juju-exec",
		"22:20:22 bashy/0 action juju-exec juju-exec",
		"22:20:22 otelcol/0 action reconcile reconcile",
		"22:20:43 otelcol/0 hook config-changed config-changed",
		"22:21:12 otelcol/0 hook config-changed config-changed",
		"22:22:03 otelcol/0 hook stop stop",
		"22:22:13 otelcol/0 hook upgrade-charm upgrade-charm",
		"22:22:17 otelcol/0 hook config-changed config-changed",
		"22:22:20 otelcol/0 hook start start",
		"22:22:22 otelcol/0 hook otelcol-pebble-ready pebble-ready",
		"22:23:52 otelcol/0 action juju-exec juju-exec",
	}
	if g, w := strings.Join(lines, "\n"), strings.Join(want, "\n"); g != w {
		t.Fatalf("sightings:\n%s\nwant:\n%s", g, w)
	}
	// stop runs until the agent goes away: the next entry is the new pod's.
	if s := got[6]; s.End.Sub(s.Start) < 9*time.Second {
		t.Errorf("stop ends at %s, want the next entry, upgrade-charm's", s.End)
	}
}

func TestTapCoverageReasons(t *testing.T) {
	cov, err := LoadTapCoverage(fixtureDir(untappedK8s), fixtureStart(t, untappedK8s))
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, "2026-10-22T"+s+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	for _, c := range []struct {
		unit, at, want string
	}{
		{"otelcol/0", "22:19:39.5", ReasonBeforeProbe}, // the recording started, the probe not yet
		{"otelcol/0", "22:20:43.358", ""},              // the tap in place
		{"otelcol/0", "22:22:10", ReasonNoSession},     // old pod gone, new one not started
		{"otelcol/0", "22:22:13.369", ReasonPodStart},  // upgrade-charm in the new pod
		{"otelcol/0", "22:22:20.003", ReasonPodStart},  // start, just before the new session
		{"otelcol/0", "22:22:21", ""},                  // after the new install
		{"otelcol/0", "22:24:00", ReasonAfterProbe},    // after the last record
		{"bashy/0", "22:20:06.329", unit.ReasonNoVenv}, // not built on ops
		{"grafana/0", "22:20:06", ""},                  // never covered
	} {
		if got := cov.Reason("lens-t9", c.unit, at(c.at)); got != c.want {
			t.Errorf("%s at %s: reason %q, want %q", c.unit, c.at, got, c.want)
		}
	}
	if !cov.Covers("lens-t9", "bashy/0") || cov.Covers("lens-t9", "grafana/0") {
		t.Errorf("covers: bashy %v grafana %v", cov.Covers("lens-t9", "bashy/0"), cov.Covers("lens-t9", "grafana/0"))
	}
	exec := HookSighting{Model: "lens-t9", Unit: "otelcol/0", Exec: ExecAction, Name: "juju-exec", Start: at("22:20:21.841")}
	if got := cov.UntappedReason(exec); got != ReasonJujuExec {
		t.Errorf("juju exec: reason %q", got)
	}
}

func TestOpsKilledProcessIsTruncated(t *testing.T) {
	spans, _, err := LoadOps(fixtureDir(untappedK8s))
	if err != nil {
		t.Fatal(err)
	}
	var truncated []string
	for _, r := range roots(spans) {
		if r.Attrs["tap"] != "" {
			truncated = append(truncated, r.Start.Format("15:04:05")+" "+r.Name+" "+r.Attrs["tap"]+" "+r.Attrs["tap_reason"]+" "+r.StatusCode)
		}
	}
	want := "22:21:12 config-changed truncated " + ReasonKilled + " ERROR"
	if len(truncated) != 1 || truncated[0] != want {
		t.Fatalf("truncated roots = %q, want [%q]", truncated, want)
	}
}
