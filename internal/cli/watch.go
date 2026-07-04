package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/viewer"

	"github.com/spf13/cobra"
)

// newWatchCmd wires `juju-lens watch`: record a live model/controller and open
// the viewer following it, in one command. It is the live-debugging entry point
// (as opposed to headless `record`, which captures for later, and `view`, which
// opens a saved recording).
//
// Under the hood it runs `record` as a child process — so the recorder's
// progress output goes to a log file instead of fighting the TUI's alt-screen,
// and the child gets a clean SIGTERM (finalise manifest + authoritative index)
// when the viewer quits. It needs root for eBPF, so the whole thing runs under
// sudo and the TUI renders as root (the recorder's juju calls still drop to
// $SUDO_USER internally).
func newWatchCmd() *cobra.Command {
	f := &recordFlags{debugLog: true}
	cmd := &cobra.Command{
		Use:   "watch [model]",
		Short: "Record a live model/controller and follow it in the viewer",
		Long: `watch records a live model (or whole controller) and immediately opens
the viewer following the capture, so you can watch hooks, status and relation
databags update in real time. Quitting the viewer (q) stops the recording; the
recording directory is kept so you can 'juju-lens view' it again later.

Examples:
  sudo juju-lens watch cos-lite            # follow one model on the current controller
  sudo juju-lens watch --controller kub    # follow every model on a controller`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				f.model = args[0]
			}
			return runWatch(cmd.Context(), *f)
		},
	}
	cmd.Flags().StringVar(&f.model, "model", "", "scope to a single model (also accepted as a positional arg)")
	cmd.Flags().StringVar(&f.controller, "controller", "", "scope to a controller (all its models)")
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/<ts>--watch-<scope>)")
	cmd.Flags().StringVar(&f.probePath, "probe-path", "", "path to the juju-lens-probe binary")
	cmd.Flags().BoolVar(&f.debugLog, "debug-log", true, "also stream 'juju debug-log' for the scoped models")
	return cmd
}

func runWatch(ctx context.Context, f recordFlags) error {
	scope := f.model
	if scope == "" {
		scope = f.controller
	}
	name := "watch"
	if scope != "" {
		name = "watch-" + scope
	}
	dir := f.output
	if dir == "" {
		dir = filepath.Join("recordings", recording.SuggestedDirName(name, time.Now()))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating juju-lens binary: %w", err)
	}
	recArgs := []string{"record", name, "-o", dir}
	if f.model != "" {
		recArgs = append(recArgs, "--model", f.model)
	}
	if f.controller != "" {
		recArgs = append(recArgs, "--controller", f.controller)
	}
	if f.probePath != "" {
		recArgs = append(recArgs, "--probe-path", f.probePath)
	}
	if !f.debugLog {
		recArgs = append(recArgs, "--debug-log=false")
	}

	// The recorder's progress goes to record.log so it doesn't corrupt the TUI.
	logPath := filepath.Join(dir, "record.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	rec := exec.Command(self, recArgs...)
	rec.Stdout = logFile
	rec.Stderr = logFile
	if err := rec.Start(); err != nil {
		return fmt.Errorf("starting recorder: %w", err)
	}
	recDone := make(chan error, 1)
	go func() { recDone <- rec.Wait() }()

	fmt.Fprintf(os.Stderr, "juju-lens: starting live capture into %s …\n", dir)

	// Wait until the recording is ready to view, or the recorder gives up.
	manifest := filepath.Join(dir, recording.ManifestFilename)
	idxDB := recording.NewLayout(dir).IndexDB()
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(30 * time.Second)
ready:
	for {
		select {
		case <-ctx.Done():
			_ = rec.Process.Signal(syscall.SIGTERM)
			<-recDone
			return ctx.Err()
		case err := <-recDone:
			detail := "exited"
			if err != nil {
				detail = "failed: " + err.Error()
			}
			return fmt.Errorf("recorder %s before capture started; see %s:\n%s",
				detail, logPath, tailFile(logPath, 25))
		case <-deadline:
			_ = rec.Process.Signal(syscall.SIGTERM)
			<-recDone
			return fmt.Errorf("recorder did not become ready within 30s; see %s:\n%s", logPath, tailFile(logPath, 25))
		case <-poll.C:
			if fileExists(manifest) && fileExists(idxDB) {
				break ready
			}
		}
	}

	// Foreground the viewer in follow mode. It blocks until the user quits.
	verr := viewer.Run(dir, true)

	// Stop the recorder cleanly so it finalises the manifest and rebuilds the
	// authoritative index, then wait for it.
	_ = rec.Process.Signal(syscall.SIGTERM)
	<-recDone

	fmt.Fprintf(os.Stderr, "juju-lens: recording saved to %s (view it again with `juju-lens view %s`)\n", dir, dir)
	return verr
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// tailFile returns the last n lines of a file, for surfacing recorder errors.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
