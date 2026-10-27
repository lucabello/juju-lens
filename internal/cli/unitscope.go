package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// unitScope narrows a recording of one model to some of its applications and
// units (record --app, --unit). The zero value selects everything.
type unitScope struct {
	apps  map[string]bool
	units map[string]bool
}

// newUnitScope checks the names given to --app and --unit: an application is
// "grafana", a unit "grafana/0".
func newUnitScope(apps, units []string) (unitScope, error) {
	var s unitScope
	for _, a := range apps {
		if a == "" || strings.Contains(a, "/") {
			return s, fmt.Errorf("--app wants an application name such as \"grafana\", got %q (use --unit for a unit)", a)
		}
		if s.apps == nil {
			s.apps = map[string]bool{}
		}
		s.apps[a] = true
	}
	for _, u := range units {
		app, n, ok := strings.Cut(u, "/")
		if _, err := strconv.Atoi(n); !ok || app == "" || err != nil {
			return s, fmt.Errorf("--unit wants a unit name such as \"grafana/0\", got %q (use --app for an application)", u)
		}
		if s.units == nil {
			s.units = map[string]bool{}
		}
		s.units[u] = true
	}
	return s, nil
}

func (s unitScope) active() bool { return len(s.apps) > 0 || len(s.units) > 0 }

// has reports whether a unit ("grafana/0") is selected. An empty scope selects
// every unit.
func (s unitScope) has(unit string) bool {
	if !s.active() {
		return true
	}
	app, _, _ := strings.Cut(unit, "/")
	return s.units[unit] || s.apps[app]
}

// debugLogInclude returns the `juju debug-log` arguments that keep only the
// selected applications' and units' messages, in a stable order.
func (s unitScope) debugLogInclude() []string {
	var args []string
	for _, a := range sortedKeys(s.apps) {
		args = append(args, "--include", a)
	}
	for _, u := range sortedKeys(s.units) {
		args = append(args, "--include", "unit-"+strings.ReplaceAll(u, "/", "-"))
	}
	return args
}

func (s unitScope) String() string {
	return strings.Join(append(sortedKeys(s.apps), sortedKeys(s.units)...), ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseStatusMachines lists the machines that host a selected unit in `juju
// status --format json` output, sorted by id. A subordinate runs on its
// principal's machine, so selecting one selects that machine.
func parseStatusMachines(out []byte, scope unitScope) ([]string, error) {
	var st struct {
		Applications map[string]struct {
			Units map[string]struct {
				Machine      string                     `json:"machine"`
				Subordinates map[string]json.RawMessage `json:"subordinates"`
			} `json:"units"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parsing juju status output: %w", err)
	}
	seen := map[string]bool{}
	for _, app := range st.Applications {
		for name, u := range app.Units {
			if u.Machine == "" {
				continue
			}
			hit := scope.has(name)
			for sub := range u.Subordinates {
				hit = hit || scope.has(sub)
			}
			if hit {
				seen[u.Machine] = true
			}
		}
	}
	return sortedKeys(seen), nil
}
