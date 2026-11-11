package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export <recording>",
		Short: "Pack a recording into one file to share or attach",
		Long: `export writes a recording as one gzipped tar file: its manifest, everything
under raw/, and recorder.log. It leaves out index.db, which is as big as raw/
and is rebuilt from it the first time anything reads the recording.

To read the file, extract it and point juju-lens at the directory it makes:

  tar -xzf my-capture.tar.gz
  juju-lens inspect summary my-capture

The recording must not be being written: stop a running recorder first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(args[0], output, cmd)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write (default: <recording directory name>.tar.gz in the current directory)")
	return cmd
}

func runExport(dir, output string, cmd *cobra.Command) error {
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
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	res, err := recording.Pack(dir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
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
