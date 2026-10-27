package recording

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// loadProfiles returns the spans and profiles under root.
func loadProfiles(t *testing.T, root string) ([]SpanRow, []OpsProfile) {
	t.Helper()
	spans, profiles, err := LoadOps(root)
	if err != nil {
		t.Fatal(err)
	}
	return spans, profiles
}

// checkProfileLinks checks what holds for any profile: every sample is on
// the timeline, inside its process, in time order; every stack belongs to
// a span of the process's tree. It returns that tree's spans by id.
func checkProfileLinks(t *testing.T, p OpsProfile, spans []SpanRow) map[string]SpanRow {
	t.Helper()
	var root SpanRow
	for _, s := range spans {
		if s.SpanID == p.RootSpanID {
			root = s
		}
	}
	if root.Attrs["kind"] != OpsRoot {
		t.Fatalf("profile root %s is not a root span", p.RootSpanID)
	}
	byID, _ := tree(spans, root.TraceID)
	kept := 0
	for i, st := range p.Stacks {
		if _, ok := byID[st.SpanID]; !ok {
			t.Errorf("stack %d is in span %s, not in its process's tree", i, st.SpanID)
		}
		for _, f := range st.Frames {
			if f < 0 || f >= len(p.Frames) {
				t.Fatalf("stack %d names frame %d of %d", i, f, len(p.Frames))
			}
		}
		kept += st.Samples
	}
	if len(p.Timeline) != p.Samples || kept != p.Samples-p.Dropped {
		t.Errorf("%d samples, %d dropped; %d on the timeline, %d in stacks", p.Samples, p.Dropped, len(p.Timeline), kept)
	}
	for i, s := range p.Timeline {
		if s.Time.Before(root.Start) || s.Time.After(root.End) {
			t.Fatalf("sample %d at %s, outside its process [%s, %s]", i, s.Time, root.Start, root.End)
		}
		if i > 0 && s.Time.Before(p.Timeline[i-1].Time) {
			t.Fatalf("sample %d before sample %d", i, i-1)
		}
		if s.Stack >= len(p.Stacks) {
			t.Fatalf("sample %d names stack %d of %d", i, s.Stack, len(p.Stacks))
		}
	}
	return byID
}

// TestOpsProfileSpans takes a recorded otelcol config-changed, whose charm
// does most of its work in its constructor, and checks each stack sits in
// the span that was running: the charm's __init__ in charm init, an
// observer's method in that observer's span.
func TestOpsProfileSpans(t *testing.T) {
	spans, profiles := loadProfiles(t, "testdata/ops/stacks")
	if len(profiles) != 1 {
		t.Fatalf("want 1 profile, got %d", len(profiles))
	}
	p := profiles[0]
	if !p.Complete || p.IntervalNs != 10_000_000 || p.RSSStart == 0 || p.RSSPeak < p.RSSStart {
		t.Fatalf("profile: complete=%v interval=%d rss %d -> %d", p.Complete, p.IntervalNs, p.RSSStart, p.RSSPeak)
	}
	byID := checkProfileLinks(t, p, spans)

	wall := map[string]int64{} // by span kind
	inInit := 0
	for _, st := range p.Stacks {
		sp := byID[st.SpanID]
		wall[sp.Attrs["kind"]] += st.WallNs
		if slices.ContainsFunc(st.Frames, func(f int) bool { return p.Frames[f].Func == "OpenTelemetryCollectorK8sCharm.__init__" }) {
			if sp.Attrs["kind"] != OpsInit {
				t.Errorf("the charm's constructor sampled in %s span %q", sp.Attrs["kind"], sp.Name)
			}
			inInit += st.Samples
		}
		if sp.Attrs["kind"] == OpsObserver &&
			!slices.ContainsFunc(st.Frames, func(f int) bool { return strings.HasSuffix(sp.Name, "."+p.Frames[f].Func) }) {
			t.Errorf("a stack in observer %q has no frame of it", sp.Name)
		}
	}
	if inInit == 0 {
		t.Fatal("no sample of the charm's constructor")
	}
	// The samples in charm init cover its duration, to within two samples.
	for _, s := range byID {
		if s.Attrs["kind"] == OpsInit {
			if d := s.End.Sub(s.Start).Nanoseconds() - wall[OpsInit]; d > 2*p.IntervalNs || d < -2*p.IntervalNs {
				t.Errorf("charm init lasted %s; its samples add up to %dns", s.End.Sub(s.Start), wall[OpsInit])
			}
		}
	}
	if wall[OpsObserver] == 0 || wall[OpsRoot] == 0 {
		t.Errorf("wall time by span kind: %v", wall)
	}
}

// TestOpsProfileKilled cuts the hook off after its second stacks record, as
// a SIGKILL would: the profile keeps what was written, marked incomplete.
func TestOpsProfileKilled(t *testing.T) {
	name, lines := fixtureLines(t, "stacks", "lens-t12", "otelcol-0")
	if n := bytes.Count(bytes.Join(lines, nil), []byte(`"k":"stacks"`)); n < 3 {
		t.Fatalf("fixture has %d stacks records, want 3 or more", n)
	}
	var kept [][]byte
	seen := 0
	for _, l := range lines {
		if bytes.Contains(l, []byte(`"kind":"spool_end"`)) {
			kept = append(kept, bytes.Replace(l, []byte(`"lines":`), []byte(`"truncated":true,"lines":`), 1))
			break
		}
		if seen < 2 {
			kept = append(kept, l)
		}
		if bytes.Contains(l, []byte(`"k":"stacks"`)) {
			seen++
		}
	}
	root := t.TempDir()
	writeUnitFile(t, root, "lens-t12", "otelcol-0", name, kept)
	spans, profiles := loadProfiles(t, root)
	if len(profiles) != 1 {
		t.Fatalf("want 1 profile, got %d", len(profiles))
	}
	p := profiles[0]
	if p.Complete || p.Samples == 0 || p.IntervalNs != 0 {
		t.Fatalf("killed hook's profile: complete=%v samples=%d interval=%d", p.Complete, p.Samples, p.IntervalNs)
	}
	checkProfileLinks(t, p, spans)
	_, full := loadProfiles(t, "testdata/ops/stacks")
	if p.Samples >= full[0].Samples {
		t.Errorf("cut profile has %d samples, the whole one %d", p.Samples, full[0].Samples)
	}
}
