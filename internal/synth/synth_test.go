package synth

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/wire"
)

func TestGenerateTrivialIsDeterministic(t *testing.T) {
	a, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Two calls with the default (zero-value) Options must produce the exact
	// same stream, because Options.Start is filled with a fixed instant.
	if !reflect.DeepEqual(a, b) {
		t.Fatal("generate is not deterministic for default options")
	}
}

func TestGenerateTrivialShape(t *testing.T) {
	msgs, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Every request must be immediately followed by a matching response with
	// the same request-id/pid/conn; requests carry a facade+method.
	if len(msgs)%2 != 0 {
		t.Fatalf("expected paired request/response messages, got odd count %d", len(msgs))
	}
	units := map[string]bool{}
	statusSets, appStatusSets, joined := 0, 0, 0
	for i := 0; i < len(msgs); i += 2 {
		req, resp := msgs[i], msgs[i+1]
		if req.Dir != wire.DirWrite {
			t.Fatalf("msg %d: expected request (write), got %s", i, req.Dir)
		}
		if resp.Dir != wire.DirRead {
			t.Fatalf("msg %d: expected response (read), got %s", i+1, resp.Dir)
		}
		if req.Msg.RequestID != resp.Msg.RequestID || req.PID != resp.PID || req.Conn != resp.Conn {
			t.Fatalf("msg %d/%d not paired: %+v / %+v", i, i+1, req.Msg, resp.Msg)
		}
		if !req.Msg.IsRequest() {
			t.Fatalf("msg %d request has no facade/method", i)
		}
		if resp.Ts.Before(req.Ts) {
			t.Fatalf("response %d precedes its request", i+1)
		}
		units[req.Unit] = true
		switch req.Msg.Request {
		case "SetStatus":
			statusSets++
		case "SetApplicationStatus":
			appStatusSets++
		case "CommitHookChanges":
			if strings.Contains(string(req.Msg.Params), "relation-joined") {
				joined++
			}
		}
	}
	if !units["grafana/0"] || !units["prometheus/0"] {
		t.Fatalf("expected grafana/0 and prometheus/0, got %v", units)
	}
	if statusSets != 2 {
		t.Fatalf("expected 2 unit SetStatus RPCs, got %d", statusSets)
	}
	if appStatusSets != 2 {
		t.Fatalf("expected 2 SetApplicationStatus RPCs, got %d", appStatusSets)
	}
	if joined != 2 {
		t.Fatalf("expected 2 relation-joined commits, got %d", joined)
	}
}

func TestGenerateModelAttribution(t *testing.T) {
	msgs, err := Generate(ScenarioTrivial, Options{ModelName: "cos"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Model != "cos" {
			t.Fatalf("message not attributed to model cos: %+v", m)
		}
	}
}

func TestGenerateStartHonoured(t *testing.T) {
	start := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	msgs, err := Generate(ScenarioTrivial, Options{Start: start})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Ts.Before(start) {
		t.Fatalf("first message %s precedes start %s", msgs[0].Ts, start)
	}
}

func TestGenerateUnknownScenario(t *testing.T) {
	if _, err := Generate("does-not-exist", Options{}); err == nil {
		t.Fatal("expected error for unknown scenario")
	}
}

// TestGenerateEmitsHookMarkers verifies each hook's commit is bracketed by the
// uniter SetState run-hook/continue markers index.LabelHooks reads. Without a
// balanced run-hook before and continue after, the hook would not surface as a
// timeline event, so this guards the Events navigator's content.
func TestGenerateEmitsHookMarkers(t *testing.T) {
	msgs, err := Generate(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	runHooks, continues, commits := 0, 0, 0
	depth := 0
	for i := 0; i < len(msgs); i += 2 {
		req := msgs[i]
		switch req.Msg.Request {
		case "SetState":
			p := string(req.Msg.Params)
			switch {
			case strings.Contains(p, "run-hook"):
				runHooks++
				depth++
			case strings.Contains(p, "continue"):
				continues++
				depth--
			}
		case "CommitHookChanges":
			commits++
			if depth != 1 {
				t.Errorf("commit at msg %d ran outside a hook bracket (depth %d)", i, depth)
			}
		}
	}
	if runHooks == 0 || runHooks != continues {
		t.Fatalf("unbalanced hook markers: %d run-hook vs %d continue", runHooks, continues)
	}
	if depth != 0 {
		t.Fatalf("hook brackets not closed: final depth %d", depth)
	}
	if commits < runHooks {
		t.Fatalf("expected at least one commit per hook: %d commits, %d hooks", commits, runHooks)
	}
}

// TestBootstrapAndDebugLogPresent guards the M8 status seed and the synthetic
// debug-log stream (§7): both must exist for the trivial scenario, and the log
// stream must include traefik — a unit with no RPCs — so the merged Logs pane
// shows a source no event owns.
func TestBootstrapAndDebugLogPresent(t *testing.T) {
	boot, err := BootstrapStatus(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(boot), "traefik") {
		t.Fatal("bootstrap status missing traefik (the RPC-less app)")
	}
	logs, err := DebugLog(ScenarioTrivial, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("expected synthetic debug-log lines")
	}
	var sawTraefik bool
	for i := 1; i < len(logs); i++ {
		if logs[i].Ts.Before(logs[i-1].Ts) {
			t.Fatalf("debug-log lines not chronological at %d", i)
		}
	}
	for _, l := range logs {
		if strings.Contains(l.Text, "unit-traefik-0") {
			sawTraefik = true
		}
	}
	if !sawTraefik {
		t.Fatal("debug-log missing traefik lines (log-only source)")
	}
}
