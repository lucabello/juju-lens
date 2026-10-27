package recording

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures under testdata/ops are unit probe files cut from lab
// recordings (`record --attach unit`, 2026-10-22):
//   - k8s: opentelemetry-collector-k8s on Kubernetes: config-changed
//     succeeding, failing in charm init (ParserError) and being retried, killed
//     with SIGKILL mid-hook, and the reconcile action.
//   - machine: the opentelemetry-collector subordinate and its ubuntu
//     principal on an LXD machine.
//   - fake: the opsmatrix fake charm (ops 3.9.0 and 1.5.5), whose sequence
//     defers config-changed, re-emits it from db-relation-changed, runs an
//     action and raises in a start observer.
//   - stacks: one opentelemetry-collector-k8s config-changed with the stack
//     sampler on (2026-10-22, model lens-t12).
func loadFixture(t *testing.T, name string) []SpanRow {
	t.Helper()
	spans, _, err := LoadOps(filepath.Join("testdata", "ops", name))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) == 0 {
		t.Fatalf("no spans in fixture %s", name)
	}
	return spans
}

// roots returns the root spans in start order.
func roots(spans []SpanRow) []SpanRow {
	var out []SpanRow
	for _, s := range spans {
		if s.Attrs["kind"] == OpsRoot {
			out = append(out, s)
		}
	}
	return out
}

// tree returns the spans of one trace, and their children by parent id.
func tree(spans []SpanRow, traceID string) (map[string]SpanRow, map[string][]SpanRow) {
	byID := map[string]SpanRow{}
	kids := map[string][]SpanRow{}
	for _, s := range spans {
		if s.TraceID != traceID {
			continue
		}
		byID[s.SpanID] = s
		kids[s.ParentSpanID] = append(kids[s.ParentSpanID], s)
	}
	return byID, kids
}

func childrenOfKind(kids map[string][]SpanRow, parent, kind string) []SpanRow {
	var out []SpanRow
	for _, s := range kids[parent] {
		if s.Attrs["kind"] == kind {
			out = append(out, s)
		}
	}
	return out
}

func rootNamed(t *testing.T, rs []SpanRow, name string, nth int) SpanRow {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			if nth == 0 {
				return r
			}
			nth--
		}
	}
	t.Fatalf("no root %q (#%d)", name, nth)
	return SpanRow{}
}

// TestOpsTreesNest checks the invariants every tree must keep, on every
// fixture: one root per trace, every parent in the same trace, each span
// inside its parent's time, and every span labelled with the root's hook.
func TestOpsTreesNest(t *testing.T) {
	for _, fx := range []string{"k8s", "machine", "fake"} {
		spans := loadFixture(t, fx)
		ids := map[string]bool{}
		for _, s := range spans {
			if ids[s.SpanID] {
				t.Fatalf("%s: duplicate span id %s", fx, s.SpanID)
			}
			ids[s.SpanID] = true
		}
		for _, r := range roots(spans) {
			byID, _ := tree(spans, r.TraceID)
			for _, s := range byID {
				if s.Service != ServiceOps || s.Unit != r.Unit || s.Hook != r.Hook {
					t.Errorf("%s: span %s %q: service %q unit %q hook %q, root has unit %q hook %q",
						fx, s.SpanID, s.Name, s.Service, s.Unit, s.Hook, r.Unit, r.Hook)
				}
				if s.SpanID == r.SpanID {
					continue
				}
				p, ok := byID[s.ParentSpanID]
				if !ok {
					t.Fatalf("%s: span %q has no parent in its trace", fx, s.Name)
				}
				if s.Start.Before(p.Start) || s.End.After(p.End) || s.End.Before(s.Start) {
					t.Errorf("%s: %q [%s, %s] is outside its parent %q [%s, %s]", fx,
						s.Name, s.Start, s.End, p.Name, p.Start, p.End)
				}
			}
		}
	}
}

func TestOpsDeferredAndReemitted(t *testing.T) {
	spans := loadFixture(t, "fake")
	rs := roots(spans)
	for nth := 0; nth < 2; nth++ { // ops 1.5.5, then 3.9.0
		cc := rootNamed(t, rs, "config-changed", nth)
		_, kids := tree(spans, cc.TraceID)
		evs := childrenOfKind(kids, cc.SpanID, OpsEvent)
		if len(evs) == 0 || evs[0].Name != "config_changed" || evs[0].Attrs["deferred"] != "true" {
			t.Fatalf("config-changed: want its first event deferred, got %+v", evs)
		}

		rel := rootNamed(t, rs, "db-relation-changed", nth)
		if rel.Attrs["exec"] != ExecHook || rel.Attrs["hook_kind"] != "relation-changed" {
			t.Errorf("db-relation-changed: exec %q hook_kind %q", rel.Attrs["exec"], rel.Attrs["hook_kind"])
		}
		_, kids = tree(spans, rel.TraceID)
		init := childrenOfKind(kids, rel.SpanID, OpsInit)
		backlog := childrenOfKind(kids, rel.SpanID, OpsBacklog)
		if len(init) != 1 || len(backlog) != 1 || backlog[0].Attrs["count"] != "1" {
			t.Fatalf("db-relation-changed: want one init and a backlog of 1, got %d init, %+v", len(init), backlog)
		}
		if init[0].End.After(backlog[0].Start) {
			t.Errorf("charm init ends at %s, after the backlog starts at %s", init[0].End, backlog[0].Start)
		}
		re := childrenOfKind(kids, backlog[0].SpanID, OpsEvent)
		if len(re) != 1 || re[0].Name != "config_changed" || re[0].Attrs["reemitted"] != "true" {
			t.Fatalf("backlog: want config_changed re-emitted, got %+v", re)
		}
		obs := childrenOfKind(kids, re[0].SpanID, OpsObserver)
		if len(obs) != 1 || !strings.HasSuffix(obs[0].Name, "._on_config_changed") {
			t.Fatalf("re-emitted config_changed: want its observer, got %+v", obs)
		}
		// The observer emits a custom event, observed in turn.
		custom := childrenOfKind(kids, obs[0].SpanID, OpsEvent)
		if len(custom) != 1 || custom[0].Name != "ready" {
			t.Fatalf("want the custom event under the observer that emitted it, got %+v", custom)
		}
		// status-set is one tool span with what it set.
		var set []SpanRow
		for _, s := range kids[childrenOfKind(kids, custom[0].SpanID, OpsObserver)[0].SpanID] {
			if s.Name == "status-set" {
				set = append(set, s)
			}
		}
		if len(set) != 1 || set[0].Attrs["status"] != "active" || set[0].Attrs["kind"] != OpsTool {
			t.Errorf("want one status-set tool span setting active, got %+v", set)
		}
		// The relation hook's own event comes after the backlog.
		evs = childrenOfKind(kids, rel.SpanID, OpsEvent)
		if len(evs) == 0 || evs[0].Name != "db_relation_changed" || evs[0].Attrs["reemitted"] != "" {
			t.Errorf("want db_relation_changed emitted after the backlog, got %+v", evs)
		}
	}
}

func TestOpsObserverException(t *testing.T) {
	spans := loadFixture(t, "fake")
	start := rootNamed(t, roots(spans), "start", 1)
	if start.StatusCode != "ERROR" || start.Attrs["rc"] != "1" || start.Attrs["exc"] != "RuntimeError" ||
		!strings.HasPrefix(start.StatusMsg, "RuntimeError: boom") {
		t.Fatalf("root: status %q %q rc %q exc %q", start.StatusCode, start.StatusMsg, start.Attrs["rc"], start.Attrs["exc"])
	}
	_, kids := tree(spans, start.TraceID)
	ev := childrenOfKind(kids, start.SpanID, OpsEvent)[0]
	obs := childrenOfKind(kids, ev.SpanID, OpsObserver)
	if len(obs) != 1 || obs[0].StatusCode != "ERROR" || obs[0].Attrs["exc"] != "RuntimeError" {
		t.Fatalf("want the exception on the observer that raised it, got %+v", obs)
	}
	if init := childrenOfKind(kids, start.SpanID, OpsInit)[0]; init.StatusCode != "OK" {
		t.Errorf("charm init didn't raise, but has status %q", init.StatusCode)
	}
}

func TestOpsLabProcesses(t *testing.T) {
	spans := loadFixture(t, "k8s")
	rs := roots(spans)
	var names []string
	for _, r := range rs {
		names = append(names, r.Unit+" "+r.Name+" "+r.StatusCode)
		if r.Model != "lens-smoke" || r.App != "otelcol" {
			t.Errorf("root %q: model %q app %q", r.Name, r.Model, r.App)
		}
	}
	want := []string{
		"otelcol/1 config-changed OK",
		"otelcol/1 config-changed ERROR",
		"otelcol/1 config-changed ERROR",
		"otelcol/1 config-changed OK",
		"otelcol/0 reconcile action OK",
		"otelcol/1 config-changed ERROR",
		"otelcol/1 config-changed OK",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("roots:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}

	// The charm raised while being constructed: init carries it too.
	failed := rs[1]
	_, kids := tree(spans, failed.TraceID)
	init := childrenOfKind(kids, failed.SpanID, OpsInit)
	if len(init) != 1 || init[0].Attrs["exc"] != "ParserError" || failed.Attrs["exc"] != "ParserError" {
		t.Fatalf("failed config-changed: init %+v, root exc %q", init, failed.Attrs["exc"])
	}

	// Killed mid-hook: no exit status, and nothing pretends to have finished.
	killed := rs[5]
	if killed.Attrs["truncated"] != "true" || killed.Attrs["rc"] != "" ||
		killed.StatusMsg != "killed before it exited (no proc_end)" {
		t.Fatalf("killed hook root: %+v", killed)
	}
	byID, _ := tree(spans, killed.TraceID)
	unfinished := 0
	for _, s := range byID {
		if s.Attrs["unfinished"] == "true" {
			unfinished++
			if s.StatusCode != "" {
				t.Errorf("unfinished span %q has status %q", s.Name, s.StatusCode)
			}
		}
	}
	if unfinished == 0 {
		t.Error("killed hook: want its open spans marked unfinished")
	}

	action := rs[4]
	if action.Attrs["exec"] != ExecAction || action.Hook != "reconcile action" || action.Attrs["hook_kind"] != "" {
		t.Errorf("action root: exec %q hook %q hook_kind %q", action.Attrs["exec"], action.Hook, action.Attrs["hook_kind"])
	}
}

func TestOpsMachineHostWithTwoUnits(t *testing.T) {
	spans := loadFixture(t, "machine")
	got := map[string]int{}
	for _, r := range roots(spans) {
		got[r.Unit+" "+r.Name]++
		if r.Attrs["exec"] != ExecHook {
			t.Errorf("%s %s: exec %q, want hook (its parent is the machine agent)", r.Unit, r.Name, r.Attrs["exec"])
		}
	}
	want := map[string]int{"opentelemetry-collector/0 config-changed": 2, "ubuntu/0 update-status": 1}
	if len(got) != len(want) || got["opentelemetry-collector/0 config-changed"] != 2 || got["ubuntu/0 update-status"] != 1 {
		t.Fatalf("roots %v, want %v", got, want)
	}
}

// fixtureLines returns the lines of the one unit file under testdata/ops/<name>/raw/unit/<model>/<host>/.
func fixtureLines(t *testing.T, name, model, host string) (string, [][]byte) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join("testdata", "ops", name, "raw", "unit", model, host, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want one unit file for %s/%s, got %v", model, host, matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(matches[0]), bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

func writeUnitFile(t *testing.T, root, model, host, name string, lines [][]byte) {
	t.Helper()
	dir := filepath.Join(root, "raw", "unit", model, host)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := append(bytes.Join(lines, []byte("\n")), '\n')
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOpsStillRunningAtEnd cuts a recording off in the middle of a hook: its
// spool_end never came, so the hook is open, not killed.
func TestOpsStillRunningAtEnd(t *testing.T) {
	name, lines := fixtureLines(t, "fake", "lab", "ops-3.9.0-py3.14")
	cut := 0
	for i, l := range lines {
		if bytes.Contains(l, []byte(`"k":"emit","ev":"FakeCharm/on/db_relation_changed`)) {
			cut = i + 1
			break
		}
	}
	if cut == 0 {
		t.Fatal("fixture has no db_relation_changed emit")
	}
	root := t.TempDir()
	writeUnitFile(t, root, "lab", "ops-3.9.0-py3.14", name, lines[:cut])
	spans, _, err := LoadOps(root)
	if err != nil {
		t.Fatal(err)
	}
	rs := roots(spans)
	last := rs[len(rs)-1]
	if last.Name != "db-relation-changed" || last.Attrs["open"] != "true" || last.Attrs["truncated"] != "" ||
		last.StatusCode != "" {
		t.Fatalf("cut-off hook root: %+v", last)
	}
	if first := rs[0]; first.Attrs["open"] != "" || first.StatusCode != "OK" {
		t.Errorf("the finished hook before it: %+v", first)
	}
}

// TestOpsCutWithItsSession ends a probe session in the middle of a hook, as
// when its pod is deleted, and starts a new one: the hook's records stop
// where the old session did, so it is truncated, not running.
func TestOpsCutWithItsSession(t *testing.T) {
	name, lines := fixtureLines(t, "fake", "lab", "ops-3.9.0-py3.14")
	cut := 0
	for i, l := range lines {
		if bytes.Contains(l, []byte(`"k":"emit","ev":"FakeCharm/on/db_relation_changed`)) {
			cut = i + 1
			break
		}
	}
	if cut == 0 || !bytes.Contains(lines[0], []byte(`"kind":"hello"`)) {
		t.Fatal("fixture has no hello, or no db_relation_changed emit")
	}
	root := t.TempDir()
	cutLines := append(append([][]byte{}, lines[:cut]...), lines[0])
	writeUnitFile(t, root, "lab", "ops-3.9.0-py3.14", name, cutLines)
	spans, _, err := LoadOps(root)
	if err != nil {
		t.Fatal(err)
	}
	rs := roots(spans)
	last := rs[len(rs)-1]
	if last.Name != "db-relation-changed" || last.Attrs["open"] != "" || last.Attrs["tap"] != TapTruncated ||
		last.Attrs["tap_reason"] != ReasonSessionCut {
		t.Fatalf("hook cut with its session: %+v", last)
	}
}

// TestOpsSubprocess runs a recorded process again as if a hook had started
// it: same JUJU_* environment, but its parent is the hook, not the agent.
func TestOpsSubprocess(t *testing.T) {
	name, lines := fixtureLines(t, "fake", "lab", "ops-3.9.0-py3.14")
	type tapLine struct {
		Kind string `json:"kind"`
		Tap  struct {
			PID  int             `json:"pid"`
			File string          `json:"file"`
			Rec  json.RawMessage `json:"rec"`
		} `json:"tap"`
		SpoolEnd struct {
			File string `json:"file"`
		} `json:"spool_end"`
	}
	fileOf := func(l []byte) (string, int) {
		var tl tapLine
		_ = json.Unmarshal(l, &tl)
		if tl.Kind == "spool_end" {
			return tl.SpoolEnd.File, -1
		}
		return tl.Tap.File, tl.Tap.PID
	}
	// The parent is the config-changed hook (the first process); the child
	// is a copy of the leader-elected process under another pid.
	parentFile, parentPID := fileOf(lines[1])
	var child [][]byte
	var childFile string
	for _, l := range lines {
		if bytes.Contains(l, []byte(`"dispatch_path":"hooks/leader-elected"`)) {
			childFile, _ = fileOf(l)
			break
		}
	}
	for _, l := range lines {
		if f, _ := fileOf(l); f == childFile {
			c := bytes.ReplaceAll(l, []byte(childFile), []byte("child.jsonl"))
			c = bytes.ReplaceAll(c, []byte(`"ppid":`), []byte(`"ppid":`+itoa(parentPID)+`,"was_ppid":`))
			c = bytes.ReplaceAll(c, []byte(`"pid":`), []byte(`"pid":99`))
			child = append(child, c)
		}
	}
	// Insert the child while the parent runs: after its first record.
	var out [][]byte
	for _, l := range lines {
		if f, _ := fileOf(l); f == childFile {
			continue
		}
		out = append(out, l)
		if f, _ := fileOf(l); f == parentFile && len(child) > 0 {
			out = append(out, child...)
			child = nil
		}
	}
	root := t.TempDir()
	writeUnitFile(t, root, "lab", "ops-3.9.0-py3.14", name, out)
	spans, _, err := LoadOps(root)
	if err != nil {
		t.Fatal(err)
	}
	var parent, sub SpanRow
	for _, r := range roots(spans) {
		switch {
		case r.Name == "config-changed":
			parent = r
		case r.Attrs["exec"] == ExecProcess:
			sub = r
		}
	}
	if sub.SpanID == "" {
		t.Fatal("the copied process wasn't recognised as a subprocess")
	}
	if sub.TraceID != parent.TraceID || sub.ParentSpanID != parent.SpanID || sub.Hook != "config-changed" {
		t.Fatalf("subprocess: trace %s parent %s hook %q; want trace %s parent %s hook config-changed",
			sub.TraceID, sub.ParentSpanID, sub.Hook, parent.TraceID, parent.SpanID)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
