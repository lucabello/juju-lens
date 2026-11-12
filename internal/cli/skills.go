package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lucabello/juju-lens/internal/skills"

	"github.com/spf13/cobra"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Instructions for AI agents that ship with this juju-lens",
		Long: `skills prints or installs the instructions for AI agents that are built into
this binary, as SKILL.md files, so that they describe the commands this version
has.

  juju-lens skills list
  juju-lens skills show investigate-a-recording
  juju-lens skills install --dir ~/.claude/skills`,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, s := range skills.All() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n  %s\n", s.Name, s.Description)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show <name>",
		Short: "Print a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := skills.Get(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), s.Body)
			return err
		},
	})
	var dir string
	install := &cobra.Command{
		Use:   "install [name...]",
		Short: "Write skills as <dir>/<name>/SKILL.md",
		Long: `install writes the named skills, or all of them, as <dir>/<name>/SKILL.md.
Which directory an agent reads skills from depends on the agent: ~/.claude/skills
for Claude Code, for example.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				return fmt.Errorf("--dir is required: the directory your agent reads skills from, such as ~/.claude/skills")
			}
			paths, err := skills.Install(expandHome(dir), args)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", strings.Join(paths, "\n      "))
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", "", "the directory to write <name>/SKILL.md under")
	cmd.AddCommand(install)
	return cmd
}

// expandHome turns a leading ~/ into the user's home directory.
func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}
