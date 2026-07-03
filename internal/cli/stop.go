package cli

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

type stopFlags struct {
	timeout time.Duration
	force   bool
}

func newStopCmd() *cobra.Command {
	f := &stopFlags{}
	cmd := &cobra.Command{
		Use:   "stop <recording>",
		Short: "Signal a running recorder to stop cleanly",
		Long: `stop reads recorder.pid from the recording directory and delivers
SIGTERM, which triggers the same clean shutdown as Ctrl-C: OTEL config is
restored, the manifest is finalised, and the SQLite index is built.

Use --force to escalate to SIGKILL after the timeout; the recorder cannot
restore controller config in that case.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(args[0], *f)
		},
	}
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second, "wait this long for the recorder to exit before giving up")
	cmd.Flags().BoolVar(&f.force, "force", false, "if the recorder is still alive after --timeout, send SIGKILL")
	return cmd
}

func runStop(dir string, f stopFlags) error {
	layout := recording.NewLayout(dir)
	ok, err := layout.Exists()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no manifest.json under %q; not a juju-lens recording", dir)
	}
	pid, err := recording.ReadPidFile(layout.PidFile())
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no recorder is running against %s (no pid file at %s)",
				dir, layout.PidFile())
		}
		return err
	}
	if !recording.PidAlive(pid) {
		// Stale pid file — remove it and tell the operator so they can
		// restart cleanly.
		_ = recording.RemovePidFile(layout.PidFile())
		return fmt.Errorf("recorder PID %d is not running; removed stale %s",
			pid, layout.PidFile())
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("locating recorder process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM to %d: %w", pid, err)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: sent SIGTERM to recorder (pid %d) at %s\n", pid, dir)

	// Poll until the process is gone or --timeout elapses. 200ms is a
	// good balance between responsiveness and not spamming syscalls.
	deadline := time.Now().Add(f.timeout)
	for time.Now().Before(deadline) {
		if !recording.PidAlive(pid) {
			fmt.Fprintf(os.Stderr, "juju-lens: recorder %d exited cleanly\n", pid)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !f.force {
		return fmt.Errorf("recorder %d still running after %s; re-run with --force to SIGKILL", pid, f.timeout)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("sending SIGKILL to %d: %w", pid, err)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: SIGKILL sent to %d (controller OTEL config may not have been restored)\n", pid)
	return nil
}
