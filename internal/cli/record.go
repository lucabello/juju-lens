package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/juju"
	"github.com/lucabello/juju-lens/internal/otlpsink"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

type recordFlags struct {
	output            string
	otlpAddr          string
	advertiseEndpoint string
	controller        string
	sampleRatio       string
	maxDuration       time.Duration
	maxSize           int64
	dontSetOtel       bool
}

func newRecordCmd() *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record <name>",
		Short: "Record OTLP traces and logs from a Juju controller",
		Long: `record starts an OTLP gRPC server, writes every payload to a
recording directory, and stops cleanly on Ctrl-C, on SIGTERM (delivered
by 'juju-lens stop'), or when a --max-* cap is reached.

The <name> argument is a label for the recording; it becomes part of the
directory name and is stored in manifest.json. It is not passed to the
juju CLI — use --controller for that.

Unless --no-set-otel is passed, record also configures the controller's
open-telemetry-* keys to point at itself and restores the previous values
on shutdown. When --controller is empty the juju CLI's currently active
controller is used.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecord(cmd.Context(), args[0], *f)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/<ts>--<name>)")
	cmd.Flags().StringVar(&f.otlpAddr, "otlp-addr", "127.0.0.1:4317", "address to listen on for OTLP gRPC")
	cmd.Flags().StringVar(&f.advertiseEndpoint, "advertise-endpoint", "", "endpoint the controller should send to (defaults to --otlp-addr)")
	cmd.Flags().StringVar(&f.controller, "controller", "", "juju controller to configure (default: whatever the juju CLI has active)")
	cmd.Flags().StringVar(&f.sampleRatio, "sample-ratio", "1.0", "value passed to open-telemetry-sample-ratio")
	cmd.Flags().DurationVar(&f.maxDuration, "max-duration", 0, "stop recording after this duration (0 = no limit)")
	cmd.Flags().Int64Var(&f.maxSize, "max-size", 0, "stop recording after this many bytes are written to raw/ (0 = no limit)")
	cmd.Flags().BoolVar(&f.dontSetOtel, "no-set-otel", false, "do not touch controller open-telemetry-* config keys")
	return cmd
}

func runRecord(ctx context.Context, name string, f recordFlags) error {
	if f.output == "" {
		f.output = filepath.Join("recordings", recording.SuggestedDirName(name, time.Now()))
	}
	layout := recording.NewLayout(f.output)
	if err := layout.Init(); err != nil {
		return err
	}

	// Refuse to overwrite an in-flight recording. Two recorders writing
	// the same raw/ would produce corrupt JSONL.
	if pid, err := recording.ReadPidFile(layout.PidFile()); err == nil && recording.PidAlive(pid) {
		return fmt.Errorf("another recorder (PID %d) is already writing to %s; stop it with `juju-lens stop %s`",
			pid, layout.Root, layout.Root)
	}
	if err := recording.WritePidFile(layout.PidFile(), os.Getpid()); err != nil {
		return fmt.Errorf("writing pid file: %w", err)
	}
	defer recording.RemovePidFile(layout.PidFile())

	man := recording.New("juju-lens", "record", "", name)
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("writing initial manifest: %w", err)
	}

	tracesWriter := recording.NewRotatingWriter(layout.OTLPTracesFile, nil)
	logsWriter := recording.NewRotatingWriter(layout.OTLPLogsFile, nil)
	metricsWriter := recording.NewRotatingWriter(layout.OTLPMetricsFile, nil)
	defer tracesWriter.Close()
	defer logsWriter.Close()
	defer metricsWriter.Close()

	sinks := &otlpsink.Sinks{
		Traces:  tracesWriter,
		Logs:    logsWriter,
		Metrics: metricsWriter,
	}

	server, err := otlpsink.NewServer(f.otlpAddr, sinks)
	if err != nil {
		return err
	}
	man.OTLPListenAddresses = []string{server.Addr()}
	man.AddSource(recording.SourceStatus{Name: "otlp-grpc", Kind: "otlp-grpc", Started: time.Now().UTC()})

	// Auto-configure the controller unless the user opted out. Anything
	// non-fatal (missing juju CLI, unreadable config) degrades to a
	// warning and manual instructions, so `record` works even off-grid.
	restore := configureOTEL(f, server.Addr(), man)
	defer restore(man, layout)

	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("updating manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "juju-lens: recording %q; OTLP gRPC on %s (pid %d)\n",
		name, server.Addr(), os.Getpid())
	fmt.Fprintf(os.Stderr, "juju-lens: stop this recording with `juju-lens stop %s` (or SIGTERM/SIGINT)\n",
		layout.Root)

	// Signal handling and optional caps live off the main goroutine so
	// Serve() can block cleanly. Any of these firing calls GracefulStop.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	endReason := recording.EndReasonUnknown
	go func() {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "juju-lens: got %s, stopping\n", sig)
			endReason = recording.EndReasonSignal
			server.GracefulStop()
		}
	}()
	if f.maxDuration > 0 {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(f.maxDuration):
				fmt.Fprintf(os.Stderr, "juju-lens: hit --max-duration %s, stopping\n", f.maxDuration)
				endReason = recording.EndReasonMaxDuration
				server.GracefulStop()
			}
		}()
	}
	if f.maxSize > 0 {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					total := tracesWriter.Total() + logsWriter.Total() + metricsWriter.Total()
					if total >= f.maxSize {
						fmt.Fprintf(os.Stderr, "juju-lens: hit --max-size %d bytes (wrote %d), stopping\n", f.maxSize, total)
						endReason = recording.EndReasonMaxSize
						server.GracefulStop()
						return
					}
				}
			}
		}()
	}

	serveErr := server.Serve()
	cancel()

	if endReason == recording.EndReasonUnknown && serveErr != nil {
		endReason = recording.EndReasonError
	}
	if endReason == recording.EndReasonUnknown {
		endReason = recording.EndReasonUserRequested
	}
	man.FinishSource("otlp-grpc", serveErr)
	man.Finalize(endReason)
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("finalising manifest: %w", err)
	}
	// Build the SQLite index once the raw files are closed. Recording is
	// the authoritative source; the index is derived and re-buildable via
	// `juju-lens index`. Failure here is logged but not fatal, so a
	// corrupt raw file never loses the on-disk recording.
	if err := runIndex(layout.Root); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: indexing recording failed: %v (run `juju-lens index %s` to retry)\n",
			err, layout.Root)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: recording saved to %s (traces=%d logs=%d metrics=%d)\n",
		layout.Root, sinks.TraceCount(), sinks.LogCount(), sinks.MetricCount())
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return serveErr
	}
	return nil
}

// configureOTEL applies open-telemetry-* controller config (unless the
// user opted out or the juju CLI is unavailable) and returns a function
// that restores the previous values. The returned closure is safe to call
// multiple times; a no-op restoration still updates the manifest so the
// operator can tell restoration ran.
func configureOTEL(f recordFlags, boundAddr string, man *recording.Manifest) func(*recording.Manifest, recording.Layout) {
	if f.dontSetOtel {
		printManualOTEL(boundAddr, f)
		return func(*recording.Manifest, recording.Layout) {}
	}
	// An empty target means "whatever the juju CLI has active"; the
	// wrapper omits --controller in that case.
	jc := juju.New(f.controller)
	if err := jc.Available(); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: juju CLI not available, skipping auto-configure (%v)\n", err)
		printManualOTEL(boundAddr, f)
		return func(*recording.Manifest, recording.Layout) {}
	}
	prev, err := jc.ReadOTELConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: could not read current OTEL config, skipping auto-configure (%v)\n", err)
		printManualOTEL(boundAddr, f)
		return func(*recording.Manifest, recording.Layout) {}
	}
	man.PreviousOTELConfig = prev
	endpoint := f.advertiseEndpoint
	if endpoint == "" {
		endpoint = boundAddr
	}
	toSet := map[string]string{
		"open-telemetry-enabled":      "true",
		"open-telemetry-endpoint":     endpoint,
		"open-telemetry-insecure":     "true",
		"open-telemetry-sample-ratio": f.sampleRatio,
	}
	if err := jc.SetOTELConfig(toSet); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: auto-configure failed (%v)\n", err)
		printManualOTEL(boundAddr, f)
		return func(*recording.Manifest, recording.Layout) {}
	}
	label := f.controller
	if label == "" {
		label = "(active)"
	}
	fmt.Fprintf(os.Stderr, "juju-lens: configured controller %s to send OTLP to %s\n", label, endpoint)
	return func(finalMan *recording.Manifest, layout recording.Layout) {
		if err := jc.RestoreOTELConfig(prev); err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: could not restore OTEL config (%v); saved previous values in %s\n",
				err, layout.Manifest())
			return
		}
		finalMan.OTELRestored = true
		if err := finalMan.Save(layout.Root); err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: could not update manifest after restore: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "juju-lens: restored previous OTEL config on controller %s\n", label)
	}
}

// printManualOTEL is the fallback instruction block for when the recorder
// cannot (or was told not to) drive the juju CLI itself.
func printManualOTEL(boundAddr string, f recordFlags) {
	endpoint := f.advertiseEndpoint
	if endpoint == "" {
		endpoint = boundAddr
	}
	fmt.Fprintf(os.Stderr, "juju-lens: point the controller at this endpoint manually, e.g.\n"+
		"  juju controller-config \\\n"+
		"    open-telemetry-enabled=true \\\n"+
		"    open-telemetry-endpoint=%s \\\n"+
		"    open-telemetry-insecure=true \\\n"+
		"    open-telemetry-sample-ratio=%s\n", endpoint, f.sampleRatio)
}
