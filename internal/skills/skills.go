// Package skills holds the instructions for AI agents that ship inside the
// juju-lens binary, so that they match the commands the binary has. Each skill
// is a SKILL.md file with a name and description in its frontmatter, the
// format agent runtimes read.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed files
var files embed.FS

// Skill is one embedded skill.
type Skill struct {
	Name        string
	Description string
	Body        string // the whole SKILL.md, frontmatter included
}

// All returns every skill, by name.
func All() []Skill {
	var out []Skill
	ents, _ := fs.ReadDir(files, "files")
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		data, err := files.ReadFile("files/" + e.Name() + "/SKILL.md")
		if err != nil {
			continue
		}
		s := Skill{Name: e.Name(), Body: string(data)}
		s.Description = frontmatter(s.Body)["description"]
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the skill with the name.
func Get(name string) (Skill, error) {
	for _, s := range All() {
		if s.Name == name {
			return s, nil
		}
	}
	var names []string
	for _, s := range All() {
		names = append(names, s.Name)
	}
	return Skill{}, fmt.Errorf("no skill %q (have: %s)", name, strings.Join(names, ", "))
}

// Install writes each named skill, or every skill when none is named, as
// <dir>/<name>/SKILL.md, and returns the paths it wrote.
func Install(dir string, names []string) ([]string, error) {
	var chosen []Skill
	if len(names) == 0 {
		chosen = All()
	}
	for _, n := range names {
		s, err := Get(n)
		if err != nil {
			return nil, err
		}
		chosen = append(chosen, s)
	}
	var written []string
	for _, s := range chosen {
		path := filepath.Join(dir, s.Name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(path, []byte(s.Body), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

// frontmatter reads the key: value lines between the leading --- lines.
func frontmatter(body string) map[string]string {
	out := map[string]string{}
	rest, ok := strings.CutPrefix(body, "---\n")
	if !ok {
		return out
	}
	head, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return out
	}
	for _, l := range strings.Split(head, "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
