package synth

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type memWriter struct{ buf bytes.Buffer }

func (m *memWriter) WriteLine(p []byte) (int, error) {
	n, err := m.buf.Write(p)
	if err != nil {
		return n, err
	}
	nn, err := m.buf.Write([]byte{'\n'})
	return n + nn, err
}

func TestGenerateTrivialIsDeterministic(t *testing.T) {
	a, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Two calls with the default (zero-value) Options must produce the
	// exact same tree, because Options.Start is filled in with a fixed
	// instant.
	if len(a.ResourceSpans) != len(b.ResourceSpans) {
		t.Fatalf("resource spans differ in count: %d vs %d", len(a.ResourceSpans), len(b.ResourceSpans))
	}
	for i := range a.ResourceSpans {
		if len(a.ResourceSpans[i].ScopeSpans) != len(b.ResourceSpans[i].ScopeSpans) {
			t.Fatalf("scope spans differ at %d", i)
		}
		for j := range a.ResourceSpans[i].ScopeSpans {
			as := a.ResourceSpans[i].ScopeSpans[j].Spans
			bs := b.ResourceSpans[i].ScopeSpans[j].Spans
			if len(as) != len(bs) {
				t.Fatalf("span count differs at [%d][%d]: %d vs %d", i, j, len(as), len(bs))
			}
			for k := range as {
				if as[k].Name != bs[k].Name || as[k].StartTimeUnixNano != bs[k].StartTimeUnixNano {
					t.Fatalf("span [%d][%d][%d] not deterministic", i, j, k)
				}
			}
		}
	}
}

func TestGenerateTrivialShape(t *testing.T) {
	req, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ResourceSpans) != 2 {
		t.Fatalf("want 2 resource spans (grafana, prometheus), got %d", len(req.ResourceSpans))
	}
	// Each resource should carry a juju.unit attribute.
	seen := map[string]bool{}
	for _, rs := range req.ResourceSpans {
		for _, attr := range rs.Resource.Attributes {
			if attr.Key == "juju.unit" {
				seen[attr.Value.GetStringValue()] = true
			}
		}
	}
	if !seen["grafana/0"] || !seen["prometheus/0"] {
		t.Fatalf("expected juju.unit for grafana/0 and prometheus/0, got %v", seen)
	}

	// Every span must have a hook attribute and a non-empty trace id.
	total := 0
	joinedCount := 0
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				total++
				if len(sp.TraceId) != 16 || len(sp.SpanId) != 8 {
					t.Errorf("span %q has malformed ids", sp.Name)
				}
				if strings.Contains(sp.Name, "relation-joined") {
					joinedCount++
				}
			}
		}
	}
	if total < 10 {
		t.Fatalf("expected at least 10 spans, got %d", total)
	}
	if joinedCount != 2 {
		t.Fatalf("expected 2 relation-joined spans, got %d", joinedCount)
	}
}

func TestGenerateUnknownScenario(t *testing.T) {
	if _, err := Generate("does-not-exist", Options{}); err == nil {
		t.Fatal("expected error for unknown scenario")
	}
}

func TestEmitProducesValidJSONL(t *testing.T) {
	w := &memWriter{}
	if err := Emit(t.Context(), ScenarioTrivial, Options{Start: time.Unix(0, 0).UTC()}, w); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	lines := strings.Split(strings.TrimRight(w.buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &v); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if _, ok := v["resource_spans"]; !ok {
		t.Fatalf("expected resource_spans key, got: %v", v)
	}
}
