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
	if ctx.Err() != nil {
		return ctx.Err()
	}

	msgs, err := synth.Generate(sc, opts)
	if err != nil {
		return err
	}

	// One rotating writer per model. A shared cursor holds the message being
	// written so each writer buckets it into the hour file matching its own
	// capture time (the file name and the timestamps inside it agree).
	var curTs time.Time
	writers := map[string]*recording.RotatingWriter{}
	defer func() {
		for _, w := range writers {
			_ = w.Close()
		}
	}()
	for _, cm := range msgs {
		curTs = cm.Ts
		w := writers[cm.Model]
		if w == nil {
			w = recording.NewRotatingWriter(layout.RPCFileFor(cm.Model), func() time.Time { return curTs })
			writers[cm.Model] = w
		}
		line, err := cm.MarshalLine()
		if err != nil {
			return fmt.Errorf("marshalling synth message: %w", err)
		}
		if _, err := w.WriteLine(line); err != nil {
			return fmt.Errorf("writing synth message: %w", err)
		}
	}
	for _, w := range writers {
		if err := w.Close(); err != nil {
			return err
		}
	}

	// Synthetic debug-log stream: interleaves with the RPCs so the viewer's
	// merged Logs pane has real content. One rotating writer per model, bucketed
	// by the line's own hour so file name and timestamps agree.
	if logs, lerr := synth.DebugLog(sc, opts); lerr == nil {
		var logTs time.Time
		lw := recording.NewRotatingWriter(layout.JujuLogFileFor(f.model), func() time.Time { return logTs })
		for _, ll := range logs {
			logTs = ll.Ts
			if _, err := lw.WriteLine([]byte(ll.Text)); err != nil {
				_ = lw.Close()
				return fmt.Errorf("writing synth debug-log: %w", err)
			}
		}
		if err := lw.Close(); err != nil {
			return err
		}
	}

	// Ground-truth status bootstrap (M8): write the scenario's `juju status`
	// document so the indexer seeds baseline app/unit status and the Status pane
	// is populated from t0. Best-effort: a scenario without a bootstrap just
	// falls back to RPC-derived status.
	if boot, berr := synth.BootstrapStatus(sc, opts); berr == nil {
		dir := layout.StatusDir(f.model)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating status dir: %w", err)
		}
		if err := os.WriteFile(layout.StatusBootstrapFile(f.model), boot, 0o644); err != nil {
			return fmt.Errorf("writing status bootstrap: %w", err)
		}
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
