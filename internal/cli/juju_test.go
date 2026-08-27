package cli

import "testing"

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
	full := &modelsJSON{Models: []struct {
		ShortName      string `json:"short-name"`
		ModelUUID      string `json:"model-uuid"`
		ModelType      string `json:"model-type"`
		ControllerUUID string `json:"controller-uuid"`
		ControllerName string `json:"controller-name"`
	}{
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
