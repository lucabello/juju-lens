package cli

import (
	"fmt"
	"os"

	"github.com/lucabello/juju-lens/internal/recording"
)

// captureStatusBootstrap runs `juju status --format=json` once for each scoped
// model and writes it to raw/status/<model>/bootstrap.json, so the indexer can
// seed ground-truth app/unit status at recording start (M8) instead of showing
// every scope as "unknown" until an RPC happens to report it.
//
// It is best-effort and read-only (juju status never mutates the model): a model
// whose status can't be fetched is skipped with a warning and the recording
// proceeds. Runs before the probe attaches so the file is present by the time
// the live indexer's first tick reads raw/.
func captureStatusBootstrap(layout recording.Layout, controller string, models []string) {
	for _, model := range models {
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
	}
}
