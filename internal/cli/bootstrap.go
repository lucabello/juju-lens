package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/lucabello/juju-lens/internal/recording"
)

// captureStatusBootstrap runs `juju status --format=json` once for each scoped
// model and writes it to raw/status/<model>/bootstrap.json, so the indexer can
// seed ground-truth app/unit status at recording start (M8) instead of showing
// every scope as "unknown" until an RPC happens to report it. It then captures
// the pre-existing relation databags with `juju show-unit` (see
// captureDatabagBootstrap) so relations that predate the recording are visible
// from t0 too.
//
// It is best-effort and read-only (neither command mutates the model): a model
// whose status can't be fetched is skipped with a warning and the recording
// proceeds. Runs before the probe attaches so the files are present by the time
// the live indexer's first tick reads raw/.
//
// A model whose bootstrap.json already records at least one application is
// skipped, so this is safe to call repeatedly: the controller-scope discovery
// loop re-invokes it as models appear mid-recording (e.g. an ephemeral test
// model created after t0), whose apps would otherwise stay "unknown" because
// nothing captured their `juju status`. A model captured while still empty (it
// existed but had no apps deployed yet) is re-captured on later ticks until its
// applications show up, so their status is not lost.
func captureStatusBootstrap(layout recording.Layout, controller string, models []string) {
	for _, model := range models {
		if bootstrapHasApps(layout.StatusBootstrapFile(model)) {
			continue // already captured with real content
		}
		out, err := runJuju("status", "-m", controller+":"+model, "--format", "json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: status bootstrap for %s failed: %v\n", model, err)
			continue
		}
		dir := layout.StatusDir(model)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: status bootstrap dir for %s: %v\n", model, err)
			continue
		}
		if err := os.WriteFile(layout.StatusBootstrapFile(model), out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: writing status bootstrap for %s: %v\n", model, err)
		}
		captureDatabagBootstrap(layout, controller, model, out)
		captureConfigBootstrap(layout, controller, model, out)
	}
}

// bootstrapHasApps reports whether a saved status bootstrap file exists and
// already records at least one application. A file that is missing, unreadable,
// or captured while the model was still empty returns false, so the caller
// re-captures until the model's applications appear.
func bootstrapHasApps(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return len(appsFromStatus(raw)) > 0
}

// captureConfigBootstrap runs `juju config <app>` for every application in a
// model and writes their combined JSON (a map from application name to that
// app's verbatim `juju config --format=json` output) to
// raw/status/<model>/config.json. The indexer turns each into a baseline config
// snapshot so a config-changed event has a "before" to diff from t0. statusJSON
// is the `juju status` output already fetched for the model; its application
// names drive the config calls. Best-effort: apps that fail are skipped.
func captureConfigBootstrap(layout recording.Layout, controller, model string, statusJSON []byte) {
	apps := appsFromStatus(statusJSON)
	if len(apps) == 0 {
		return
	}
	combined := make(map[string]json.RawMessage, len(apps))
	for _, app := range apps {
		out, err := runJuju("config", app, "-m", controller+":"+model, "--format", "json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: config bootstrap for %s/%s failed: %v\n", model, app, err)
			continue
		}
		combined[app] = json.RawMessage(out)
	}
	if len(combined) == 0 {
		return
	}
	data, err := json.Marshal(combined)
	if err != nil {
		return
	}
	if err := os.WriteFile(layout.StatusConfigFile(model), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: writing config bootstrap for %s: %v\n", model, err)
	}
}

// appsFromStatus pulls the application names out of a `juju status --format=json`
// document.
func appsFromStatus(statusJSON []byte) []string {
	var doc struct {
		Applications map[string]json.RawMessage `json:"applications"`
	}
	if err := json.Unmarshal(statusJSON, &doc); err != nil {
		return nil
	}
	apps := make([]string, 0, len(doc.Applications))
	for app := range doc.Applications {
		apps = append(apps, app)
	}
	return apps
}

// captureDatabagBootstrap runs `juju show-unit` for every unit in a model and
// writes its verbatim JSON to raw/status/<model>/databags.json. show-unit
// reports each relation's application-data and per-unit data, which the indexer
// turns into baseline databag snapshots. statusJSON is the `juju status` output
// already fetched for the model; its unit names drive the show-unit call.
//
// Best-effort: a model with no units, or whose show-unit fails, is skipped.
func captureDatabagBootstrap(layout recording.Layout, controller, model string, statusJSON []byte) {
	units := unitsFromStatus(statusJSON)
	if len(units) == 0 {
		return
	}
	args := append([]string{"show-unit"}, units...)
	args = append(args, "-m", controller+":"+model, "--format", "json")
	out, err := runJuju(args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: databag bootstrap for %s failed: %v\n", model, err)
		return
	}
	if err := os.WriteFile(layout.StatusDatabagsFile(model), out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: writing databag bootstrap for %s: %v\n", model, err)
	}
}

// unitsFromStatus pulls every unit name (principals and their subordinates) out
// of a `juju status --format=json` document, so we know which units to query
// with show-unit. Unknown fields are ignored, keeping it tolerant across juju
// 3.6 and 4.x.
func unitsFromStatus(statusJSON []byte) []string {
	var doc struct {
		Applications map[string]struct {
			Units map[string]struct {
				Subordinates map[string]json.RawMessage `json:"subordinates"`
			} `json:"units"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(statusJSON, &doc); err != nil {
		return nil
	}
	var units []string
	for _, app := range doc.Applications {
		for u, unit := range app.Units {
			units = append(units, u)
			for sub := range unit.Subordinates {
				units = append(units, sub)
			}
		}
	}
	return units
}
