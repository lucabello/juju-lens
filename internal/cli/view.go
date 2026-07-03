package cli

import (
	"fmt"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/viewer"

	"github.com/spf13/cobra"
)

func newViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view <recording>",
		Short: "Open a recording in the TUI viewer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			// Refuse random directories: the presence of a manifest
			// file is our signal that this is really a recording.
			ok, err := recording.NewLayout(dir).Exists()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
			}
			return viewer.Run(dir)
		},
	}
}
