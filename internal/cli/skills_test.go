package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/skills"

	"github.com/spf13/cobra"
)

var codeSpan = regexp.MustCompile("`([^`\n]+)`")

// Every juju-lens command a skill shows exists in this binary, and so does
// every flag it names, so that a skill can't drift from the commands.
func TestSkillsNameOnlyCommandsAndFlagsThatExist(t *testing.T) {
	root := newRootCmd(BuildInfo{Version: "test"})
	all := skills.All()
	if len(all) == 0 {
		t.Fatal("no skills embedded")
	}
	for _, s := range all {
		spans := codeSpan.FindAllStringSubmatch(s.Body, -1)
		var snippets []string
		for _, m := range spans {
			snippets = append(snippets, m[1])
		}
		if len(snippets) == 0 {
			t.Errorf("skill %s shows no commands", s.Name)
		}
		for _, sn := range snippets {
			fields := strings.Fields(sn)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "juju-lens" {
				cmd, _, err := root.Find(fields[1:])
				if err != nil || cmd == root {
					t.Errorf("skill %s: `%s` is not a juju-lens command", s.Name, sn)
					continue
				}
				for _, f := range fields[1:] {
					if name, ok := strings.CutPrefix(f, "--"); ok {
						if name != "help" && cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil {
							t.Errorf("skill %s: `%s`: %s has no --%s", s.Name, sn, cmd.CommandPath(), name)
						}
					}
				}
				continue
			}
			// A flag named on its own, such as `--fail problem`: it must
			// exist on some command.
			for _, f := range fields {
				name, ok := strings.CutPrefix(f, "--")
				if !ok {
					continue
				}
				if name != "help" && !flagExistsUnder(root, name) {
					t.Errorf("skill %s: `%s` names --%s, which no command has", s.Name, sn, name)
				}
			}
		}
	}
}

func flagExistsUnder(root *cobra.Command, flag string) bool {
	found := false
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Flags().Lookup(flag) != nil {
			found = true
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return found
}

// A skill says nothing a recording may not hold.
func TestSkillsDoNotPromiseWhatARecordingMayLack(t *testing.T) {
	for _, s := range skills.All() {
		low := strings.ToLower(s.Body)
		for _, banned := range []string{"relation data", "databag", "agent status", "run-hook", "settle"} {
			if strings.Contains(low, banned) {
				t.Errorf("skill %s mentions %q", s.Name, banned)
			}
		}
		if !strings.HasPrefix(s.Body, "---\nname: "+s.Name+"\ndescription: ") || s.Description == "" {
			t.Errorf("skill %s: frontmatter lacks its name or description", s.Name)
		}
	}
}

func TestSkillsCommand(t *testing.T) {
	run := func(args ...string) (string, error) {
		root := newRootCmd(BuildInfo{Version: "test"})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"skills"}, args...))
		err := root.Execute()
		return out.String(), err
	}
	if out, err := run("list"); err != nil || !strings.Contains(out, "investigate-a-recording") {
		t.Errorf("list: %v %q", err, out)
	}
	if out, err := run("show", "investigate-a-recording"); err != nil || !strings.Contains(out, "juju-lens inspect summary") {
		t.Errorf("show: %v", err)
	}
	if _, err := run("show", "nope"); err == nil {
		t.Error("an unknown skill should fail")
	}
	dir := t.TempDir()
	if _, err := run("install", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	want, _ := skills.Get("investigate-a-recording")
	got, err := os.ReadFile(filepath.Join(dir, "investigate-a-recording", "SKILL.md"))
	if err != nil || string(got) != want.Body {
		t.Errorf("installed file: %v", err)
	}
	if _, err := run("install"); err == nil {
		t.Error("install without --dir should fail")
	}
}
