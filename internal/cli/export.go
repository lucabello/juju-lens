package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

type exportFlags struct {
	output string
	models []string
	units  []string
	since  string
	until  string
	noVals bool
}

func newExportCmd() *cobra.Command {
	f := &exportFlags{}
	cmd := &cobra.Command{
		Use:   "export <recording>",
		Short: "Pack a recording, or part of it, into one file to share or attach",
		Long: `export writes a recording as one gzipped tar file: its manifest, everything
under raw/, and recorder.log. It leaves out index.db, which is as big as raw/
and is rebuilt from it the first time anything reads the recording.

To read the file, extract it and point juju-lens at the directory it makes:

  tar -xzf my-capture.tar.gz
  juju-lens inspect summary my-capture

Pack part of a recording with:

  --model   only these models' files (repeatable)
  --unit    only these units, or applications: their unit probe and workload
            logs, and the lines of the debug log and the RPC capture that
            belong to them (repeatable)
  --since, --until
            only lines in this time window (RFC 3339), read from each
            line's own timestamp
  --without-values
            leave out databags.json and config.json, the snapshots that hold
            the values of relation data and configuration

A hook that straddles the window is cut: inspect shows one that starts inside
the window as still running, and doesn't list one that starts before it.
Scoping chooses what to include; it doesn't redact. Status snapshots of a kept
model are kept whole, and databags.json and config.json hold the values of
relation data and configuration (--without-values leaves them out); logs hold
whatever the charms wrote. Review the file before you post it publicly. The archive's manifest says it is partial.

The recording must not be being written: stop a running recorder first.`,
		Example: `  juju-lens export ./recordings/2026-11-11T17-54-31--expt
  juju-lens export ./rec --model testing --unit grafana -o grafana.tar.gz
  juju-lens export ./rec --since 2026-11-11T17:55:00Z --until 2026-11-11T17:57:00Z`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(args[0], *f, cmd)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.output, "output", "o", "", "file to write (default: <recording directory name>.tar.gz in the current directory)")
	fl.StringArrayVar(&f.models, "model", nil, "only this model (repeatable)")
	fl.StringArrayVar(&f.units, "unit", nil, "only this unit or application (repeatable)")
	fl.StringVar(&f.since, "since", "", "only lines at or after this time (RFC 3339)")
	fl.StringVar(&f.until, "until", "", "only lines at or before this time (RFC 3339)")
	fl.BoolVar(&f.noVals, "without-values", false, "leave out the snapshots of relation data and configuration values (databags.json and config.json)")
	return cmd
}

func runExport(dir string, f exportFlags, cmd *cobra.Command) error {
	opts := recording.PackOptions{Models: f.models, Units: f.units, WithoutValues: f.noVals}
	var err error
	if f.since != "" {
		if opts.Since, err = time.Parse(time.RFC3339, f.since); err != nil {
			return fmt.Errorf("--since: %w", err)
		}
	}
	if f.until != "" {
		if opts.Until, err = time.Parse(time.RFC3339, f.until); err != nil {
			return fmt.Errorf("--until: %w", err)
		}
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Until.Before(opts.Since) {
		return fmt.Errorf("--until is before --since")
	}
	output := f.output
	if output == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		output = filepath.Base(abs) + ".tar.gz"
	}
	// Check the recording before creating the file, so that a wrong path
	// leaves nothing behind.
	if ok, err := recording.NewLayout(dir).Exists(); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
	}
	for _, m := range f.models {
		if !recording.HasModel(dir, m) {
			return fmt.Errorf("no model %q in this recording (it has %s)", m, strings.Join(recording.Models(dir), ", "))
		}
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	res, err := recording.Pack(dir, file, opts)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil && res.RawFiles == 0 && (len(f.models) > 0 || len(f.units) > 0 || !opts.Since.IsZero() || !opts.Until.IsZero()) {
		err = fmt.Errorf("nothing in the recording matches --model, --unit, --since and --until")
	}
	if err != nil {
		_ = os.Remove(output)
		return err
	}
	info, err := os.Stat(output)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %d files, %d bytes of raw data in %d bytes\n", output, res.Files, res.Bytes, info.Size())
	return nil
}
