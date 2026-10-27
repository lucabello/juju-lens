package cli

import (
	"fmt"
	"strings"
)

// attachValues lists what --attach accepts, for help and error text.
const attachValues = "host, unit, none"

// attachSides is the parsed --attach set: which capture sides a recording
// runs. The zero value is `none`: logs and the status snapshot only.
type attachSides struct {
	// host runs juju-lens-probe where the Juju agents run (this host, or
	// --ssh-target over `juju ssh`) and captures their API RPCs with eBPF.
	host bool
	// unit runs juju-lens-probe inside every unit in scope over `juju ssh`.
	unit bool
}

// parseAttach reads a comma-separated --attach value. Empty means `host`.
// Order and repeats don't matter; `none` can't be combined with a side.
func parseAttach(s string) (attachSides, error) {
	if strings.TrimSpace(s) == "" {
		return attachSides{host: true}, nil
	}
	var a attachSides
	none := false
	for _, v := range strings.Split(s, ",") {
		switch v = strings.TrimSpace(v); v {
		case "host":
			a.host = true
		case "unit":
			a.unit = true
		case "none":
			none = true
		case "local", "ssh":
			return attachSides{}, fmt.Errorf("--attach %s was replaced by --attach host, which runs the probe over 'juju ssh' when --ssh-target is set and locally otherwise (want %s)", v, attachValues)
		default:
			return attachSides{}, fmt.Errorf("unknown --attach value %q (want %s)", v, attachValues)
		}
	}
	if none && a != (attachSides{}) {
		return attachSides{}, fmt.Errorf("--attach none can't be combined with another value (got %q)", s)
	}
	return a, nil
}

// String is the normalised form stored in the manifest's attach_mode.
func (a attachSides) String() string {
	var parts []string
	if a.host {
		parts = append(parts, "host")
	}
	if a.unit {
		parts = append(parts, "unit")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}
