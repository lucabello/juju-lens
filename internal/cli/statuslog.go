package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// statusLogParallel bounds the `juju show-status-log` calls running at once.
const statusLogParallel = 8

// captureStatusLog writes raw/status/<model>/status-log.json for each model
// the unit probes covered: every covered unit's `juju show-status-log --type
// juju-unit` output since the recording started (since). The indexer reads it
// to find the hooks the tap recorded nothing of. It runs once, as the
// recording stops, so the log covers the whole recording; it is best-effort
// and read-only, like the status bootstrap.
func captureStatusLog(layout recording.Layout, controller string, since time.Time) {
	cov, err := recording.LoadTapCoverage(layout.Root, since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: status log: %v\n", err)
		return
	}
	// --days counts back from now, in whole days.
	days := strconv.Itoa(int(time.Since(since)/(24*time.Hour)) + 1)
	sem := make(chan struct{}, statusLogParallel)
	for model, units := range cov.Units() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		byUnit := make(map[string]json.RawMessage, len(units))
		for _, u := range units {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				out, err := runJuju("show-status-log", "-m", modelArg(controller, model),
					"--type", "juju-unit", "--days", days, "--format", "json", u)
				if err != nil || !json.Valid(out) {
					fmt.Fprintf(os.Stderr, "juju-lens: status log of %s failed: %v\n", u, err)
					return
				}
				mu.Lock()
				byUnit[u] = out
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(byUnit) == 0 {
			continue
		}
		body, _ := json.Marshal(byUnit)
		err := os.MkdirAll(layout.StatusDir(model), 0o755)
		if err == nil {
			err = os.WriteFile(layout.StatusLogFile(model), body, 0o644)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: writing status log for %s: %v\n", model, err)
		}
	}
}
