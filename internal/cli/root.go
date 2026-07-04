// Package cli wires the juju-lens subcommands. It is intentionally thin:
// each subcommand delegates to a dedicated internal package so cobra
// details never leak into business logic.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// BuildInfo is stamped by main at link time.
type BuildInfo struct {
	Version string
	Commit  string
}

// Execute parses args and runs the appropriate subcommand.
func Execute(bi BuildInfo) error {
	root := newRootCmd(bi)
	return root.Execute()
}

func newRootCmd(bi BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:          "juju-lens",
		Short:        "A time machine for Juju controllers",
		Long:         "juju-lens records everything a Juju controller emits and lets you replay it offline.",
		SilenceUsage: true,
	}
	root.AddCommand(
		newVersionCmd(bi),
		newRecordCmd(),
		newWatchCmd(),
		newStopCmd(),
		newSynthCmd(),
		newIndexCmd(),
		newViewCmd(),
	)
	return root
}

func newVersionCmd(bi BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the juju-lens version and exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "juju-lens %s (%s)\n", bi.Version, bi.Commit)
			return err
		},
	}
}
