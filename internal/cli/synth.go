package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/synth"

	"github.com/spf13/cobra"
)

type synthFlags struct {
	output    string
	startTime string
	model     string
}

func newSynthCmd() *cobra.Command {
	f := &synthFlags{}
	cmd := &cobra.Command{
		Use:   "synth <scenario>",
		Short: "Generate a synthetic recording for viewer development",
		Long: `synth writes a recording directory whose spans mimic what a real
Juju controller would emit for a given scenario. The recording is
byte-for-byte reproducible when --start is fixed.

Available scenarios:
  trivial   two applications (grafana, prometheus) forming one relation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSynth(cmd.Context(), synth.Scenario(args[0]), *f)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/synth-<scenario>)")
	cmd.Flags().StringVar(&f.startTime, "start", "", "RFC3339 start instant (empty = the scenario's deterministic default)")
	cmd.Flags().StringVar(&f.model, "model", "default", "juju.model resource attribute stamped on every span")
	return cmd
}

func runSynth(ctx context.Context, sc synth.Scenario, f synthFlags) error {
	out := f.output
	if out == "" {
		out = filepath.Join("recordings", "synth-"+string(sc))
	}
	layout := recording.NewLayout(out)
	if err := layout.Init(); err != nil {
		return err
	}

	opts := synth.Options{ModelName: f.model}
	if f.startTime != "" {
		t, err := time.Parse(time.RFC3339, f.startTime)
		if err != nil {
			return fmt.Errorf("--start: %w", err)
		}
		opts.Start = t
	}

	writer := recording.NewRotatingWriter(layout.OTLPTracesFile, func() time.Time {
		if !opts.Start.IsZero() {
			return opts.Start
		}
		// Match the default the synth package uses so both the file
		// name and the span timestamps agree.
		return time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	})
	if err := synth.Emit(ctx, sc, opts, writer); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	man := recording.New("juju-lens", "synth", "", "synth-"+string(sc))
	man.Models = []recording.ModelInfo{{Name: f.model}}
	man.AddSource(recording.SourceStatus{
		Name:    "synth",
		Kind:    "synth",
		Started: time.Now().UTC(),
		Stopped: time.Now().UTC(),
		Records: 1,
	})
	man.Finalize(recording.EndReasonSynth)
	if err := man.Save(layout.Root); err != nil {
		return err
	}
	// Synthetic recordings ship a ready-to-open index so `juju-lens synth
	// ... && juju-lens view ...` works without a separate `index` step.
	if err := runIndex(layout.Root); err != nil {
		return fmt.Errorf("indexing synthetic recording: %w", err)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: wrote synthetic recording to %s\n", layout.Root)
	return nil
}
