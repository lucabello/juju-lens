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

	"github.com/lucabello/juju-lens/internal/otlpsink"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

type recordFlags struct {
	output      string
	otlpAddr    string
	maxDuration time.Duration
	maxSize     int64
	dontSetOtel bool
}

func newRecordCmd() *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record <controller>",
		Short: "Record OTLP traces and logs from a Juju controller",
		Long: `record starts an OTLP gRPC server, writes every payload to a
recording directory, and stops cleanly on Ctrl-C or when a cap is reached.

Milestone 1 only implements the OTLP receiver and the on-disk layout. It
does not yet configure the controller for you or ingest juju/k8s/journal
logs; see the VISION.md for the full scope.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecord(cmd.Context(), args[0], *f)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/<ts>--<controller>)")
	cmd.Flags().StringVar(&f.otlpAddr, "otlp-addr", "127.0.0.1:4317", "address to listen on for OTLP gRPC")
	cmd.Flags().DurationVar(&f.maxDuration, "max-duration", 0, "stop recording after this duration (0 = no limit)")
	cmd.Flags().Int64Var(&f.maxSize, "max-size", 0, "stop recording after this many bytes are written to raw/ (0 = no limit)")
	cmd.Flags().BoolVar(&f.dontSetOtel, "no-set-otel", true, "do not touch controller open-telemetry-* config keys (default true; wiring lands in a later milestone)")
	return cmd
}

func runRecord(ctx context.Context, controller string, f recordFlags) error {
	if f.output == "" {
		f.output = filepath.Join("recordings", recording.SuggestedDirName(controller, time.Now()))
	}
	layout := recording.NewLayout(f.output)
	if err := layout.Init(); err != nil {
		return err
	}

	// The tool version/commit stamp isn't reachable from here directly;
	// synth-side commands use "-" and record uses whatever build info was
	// baked into the top-level command. Keep it simple for M1.
	man := recording.New("juju-lens", "record", "", controller)
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
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("updating manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "juju-lens: recording %q; OTLP gRPC on %s\n", controller, server.Addr())
	if f.dontSetOtel {
		fmt.Fprintf(os.Stderr, "juju-lens: point the controller at this endpoint manually, e.g.\n"+
			"  juju controller-config \\\n"+
			"    open-telemetry-enabled=true \\\n"+
			"    open-telemetry-endpoint=%s \\\n"+
			"    open-telemetry-insecure=true \\\n"+
			"    open-telemetry-sample-ratio=1.0\n", server.Addr())
	}

	// Signal handling and optional caps live off the main goroutine so
	// Serve() can block cleanly. Any of these firing calls GracefulStop.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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
