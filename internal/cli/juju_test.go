package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTopologyMapsDropsModelsNotInResult exercises the pure map-building
// logic that both resolveScope (initial build) and jujuTopology.refresh
// (rebuild) go through. refresh calls this with the *current* `juju models`
// result and assigns the maps wholesale rather than merging, so a model that
// no longer appears — e.g. a short-lived test model destroyed mid-recording —
// must be absent from the returned maps. Otherwise modelsToStream keeps
// surfacing it forever, and captureStatusBootstrap retries and fails against
// it on every discovery tick for the rest of the recording instead of just
// once (the bug this test guards against).
func TestBuildTopologyMapsDropsModelsNotInResult(t *testing.T) {
	full := &modelsJSON{Models: []jujuModel{
		{ShortName: "cos-lite", ModelUUID: "uuid-cos", ModelType: "caas", ControllerUUID: "ctrl-1", ControllerName: "k8s"},
		{ShortName: "test-charm-9uyg", ModelUUID: "uuid-test", ModelType: "caas", ControllerUUID: "ctrl-1", ControllerName: "k8s"},
	}}

	modelName, modelType, controllerNameByUUID, controllerUUID := buildTopologyMaps(full)
	if modelName["uuid-test"] != "test-charm-9uyg" {
		t.Fatalf("expected the ephemeral model to be present after the first query, got %q", modelName["uuid-test"])
	}
	if controllerUUID != "ctrl-1" {
		t.Fatalf("expected controller uuid ctrl-1, got %q", controllerUUID)
	}
	if controllerNameByUUID["ctrl-1"] != "k8s" {
		t.Fatalf("expected controller name k8s for ctrl-1, got %q", controllerNameByUUID["ctrl-1"])
	}
	if modelType["test-charm-9uyg"] != "caas" {
		t.Fatalf("expected model type caas for test-charm-9uyg, got %q", modelType["test-charm-9uyg"])
	}

	// The test model was destroyed; a subsequent `juju models` no longer lists
	// it. A rebuild from this result must drop it, not keep it around from the
	// previous call.
	afterDeletion := &modelsJSON{Models: full.Models[:1]}
	modelName, _, _, _ = buildTopologyMaps(afterDeletion)
	if _, ok := modelName["uuid-test"]; ok {
		t.Fatal("deleted model should not appear in a fresh build; a leftover entry causes it to be retried forever by modelsToStream-driven callers")
	}
	if modelName["uuid-cos"] != "cos-lite" {
		t.Fatal("the still-present model should remain")
	}
}

// fakeJuju puts a `juju` on PATH that answers `controllers` and `models` with
// the given models JSON, and records the arguments of every other call in a
// file it returns the path of.
func fakeJuju(t *testing.T, modelsJSON string) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"controllers) echo '{\"current-controller\":\"jaas\",\"controllers\":{\"jaas\":{\"uuid\":\"c-uuid\"}}}' ;;\n" +
		"models) cat <<'EOF'\n" + modelsJSON + "\nEOF\n;;\n" +
		"*) echo \"$@\" >> " + calls + "; echo '{}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "juju"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return calls
}

const jaasModels = `{"models":[
 {"name":"alice@example.com/dev","short-name":"dev","model-uuid":"u-alice","model-type":"iaas","controller-uuid":"c-uuid","controller-name":"jaas","owner":"alice@example.com"},
 {"name":"ci@serviceaccount/dev","short-name":"dev","model-uuid":"u-sa","model-type":"caas","controller-uuid":"c-uuid","controller-name":"jaas","owner":"ci@serviceaccount"},
 {"name":"ci@serviceaccount/prod","short-name":"prod","model-uuid":"u-prod","model-type":"caas","controller-uuid":"c-uuid","controller-name":"jaas","owner":"ci@serviceaccount"}
]}`

// TestResolveScopeJAASModels: a model of a JAAS controller is found by
// "owner/model", and the juju client is then asked about it by that name, which
// is the only one it finds a model owned by someone else under. A short name
// that two owners share is ambiguous; one only one owner has still works.
func TestResolveScopeJAASModels(t *testing.T) {
	calls := fakeJuju(t, jaasModels)

	topo, filter, err := resolveScope("", "jaas:ci@serviceaccount/dev")
	if err != nil {
		t.Fatalf("owner/model: %v", err)
	}
	if filter.modelUUID != "u-sa" || topo.controllerName != "jaas" {
		t.Errorf("resolved %q on %q, want u-sa on jaas", filter.modelUUID, topo.controllerName)
	}
	if got := modelArg("jaas", "dev"); got != "jaas:ci@serviceaccount/dev" {
		t.Errorf("-m for the model chosen: %q", got)
	}
	if _, err := runJuju("status", "-m", modelArg("jaas", "dev"), "--format", "json"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(calls); !strings.Contains(string(data), "status -m jaas:ci@serviceaccount/dev") {
		t.Errorf("juju was called with %q", data)
	}

	if _, _, err := resolveScope("jaas", "dev"); err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "alice@example.com/dev") || !strings.Contains(err.Error(), "ci@serviceaccount/dev") {
		t.Errorf("a short name two owners share: %v, want an ambiguity error naming both", err)
	}

	_, filter, err = resolveScope("jaas", "prod")
	if err != nil || filter.modelUUID != "u-prod" {
		t.Errorf("short name of one model: %q, %v", filter.modelUUID, err)
	}
	if got := modelArg("jaas", "prod"); got != "jaas:ci@serviceaccount/prod" {
		t.Errorf("-m for a model of another owner: %q, want it qualified", got)
	}
	if _, _, err := resolveScope("jaas", "nobody/dev"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("an owner that has no such model: %v", err)
	}
}
