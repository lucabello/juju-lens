package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// hookRunsWithUntapped indexes a lab fixture as rebuildIndex does and
// returns its hook runs, one line each: start, unit, name, how it failed,
// the tap state and reason, and who saw a hook the tap missed.
func hookRunsWithUntapped(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join("..", "recording", "testdata", "ops", name)
	spans, err := recording.LoadSpans(dir)
	if err != nil {
		t.Fatal(err)
	}
	index.LabelHooks(spans)
	ops, _, err := recording.LoadOps(dir)
	if err != nil {
		t.Fatal(err)
	}
	untapped, err := untappedRoots(dir, spans, ops)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append(spans, ops...), untapped...)
	seenBy := map[string]string{}
	for _, u := range untapped {
		seenBy[u.SpanID] = u.Attrs["seen_by"]
	}
	trees := narrative.NewOpsIndex(all)
	var lines []string
	for _, r := range narrative.BuildHookRuns(all) {
		// What the tap recorded, and nothing for a hook it missed.
		if n := len(trees.Trees(r.OpsSpans)); (r.Tap == recording.TapMissing) != (n == 0) {
			t.Errorf("%s %s %s: tap %q but %d charm-side trees", r.StartTs.Format("15:04:05"), r.Unit, r.Kind, r.Tap, n)
		}
		line := r.StartTs.Format("15:04:05") + " " + r.Unit + " " + r.Kind + " " + r.Fail.String()
		if r.Tap != "" {
			line += " " + r.Tap + " (" + r.TapReason + ")"
		}
		for _, id := range r.OpsSpans {
			if s := seenBy[id]; s != "" {
				line += " seen by " + s
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// TestUntappedHooksKubernetes: a unit-only recording, cross-checked against
// the status log (see internal/recording/untapped_test.go for the fixture).
func TestUntappedHooksKubernetes(t *testing.T) {
	got := hookRunsWithUntapped(t, "untapped-k8s")
	want := strings.Join([]string{
		"22:20:06 bashy/0 config-changed none missing (the charm has no venv: it isn't built on ops) seen by status-log",
		"22:20:21 otelcol/0 juju-exec action none missing (juju exec runs commands, not the charm) seen by status-log",
		"22:20:22 bashy/0 juju-exec action none missing (juju exec runs commands, not the charm) seen by status-log",
		"22:20:22 otelcol/0 reconcile action none",
		"22:20:43 otelcol/0 config-changed none",
		"22:21:12 otelcol/0 config-changed retried truncated (" + recording.ReasonKilled + ")",
		"22:22:03 otelcol/0 stop none",
		"22:22:13 otelcol/0 upgrade-charm none missing (the tap wasn't installed yet: the pod had just started) seen by status-log",
		"22:22:17 otelcol/0 config-changed none missing (the tap wasn't installed yet: the pod had just started) seen by status-log",
		"22:22:20 otelcol/0 start none missing (the tap wasn't installed yet: the pod had just started) seen by status-log",
		"22:22:22 otelcol/0 pebble-ready none",
		"22:23:52 otelcol/0 juju-exec action none missing (juju exec runs commands, not the charm) seen by status-log",
	}, "\n")
	if got != want {
		t.Fatalf("hook runs:\n%s\nwant:\n%s", got, want)
	}
}

// TestUntappedHooksFromRPC: a host,unit lab recording on an LXD machine
// (otelcol subordinate to ubuntu) with no status log, where the tap
// recorded every hook. The records of two processes, ubuntu's
// update-status and otelcol's last config-changed, were taken out of the
// fixture, as if the tap had missed them: their run-hook brackets are left.
func TestUntappedHooksFromRPC(t *testing.T) {
	got := hookRunsWithUntapped(t, "untapped-hostunit")
	var missing []string
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, " missing ") {
			missing = append(missing, l)
		}
	}
	want := []string{
		"20:12:58 ubuntu/0 update-status none missing () seen by rpc",
		"20:13:26 opentelemetry-collector/0 config-changed none missing () seen by rpc",
	}
	if strings.Join(missing, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hooks the tap missed:\n%s\nwant:\n%s\nall runs:\n%s", strings.Join(missing, "\n"), strings.Join(want, "\n"), got)
	}
}
