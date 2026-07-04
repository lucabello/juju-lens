package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// jujuTopology is the controller/model inventory juju-lens resolves from the
// juju client at record time. It maps the UUIDs the probe derives from each
// agent's agent.conf to the human names the operator thinks in, and it drives
// scope filtering (record only a chosen controller or model).
type jujuTopology struct {
	// controllerName is the controller the recording is scoped to.
	controllerName string
	controllerUUID string
	// modelName maps a model UUID to its short name (e.g. "cos-lite").
	modelName map[string]string
	// controllerNameByUUID maps a controller UUID to its name.
	controllerNameByUUID map[string]string
}

// resolve returns the controller/model names for the UUIDs on a captured frame.
// Unknown UUIDs yield "" so the caller can fall back to the UUID itself.
func (t *jujuTopology) resolve(controllerUUID, modelUUID string) (string, string) {
	if t == nil {
		return "", ""
	}
	return t.controllerNameByUUID[controllerUUID], t.modelName[modelUUID]
}

// controllersJSON mirrors the fields we read from `juju controllers --format
// json`.
type controllersJSON struct {
	Controllers map[string]struct {
		UUID string `json:"uuid"`
	} `json:"controllers"`
	CurrentController string `json:"current-controller"`
}

// modelsJSON mirrors the fields we read from `juju models --format json`.
type modelsJSON struct {
	Models []struct {
		ShortName      string `json:"short-name"`
		ModelUUID      string `json:"model-uuid"`
		ControllerUUID string `json:"controller-uuid"`
		ControllerName string `json:"controller-name"`
	} `json:"models"`
}

// resolveScope builds the juju topology inventory for a recording and the probe
// filter that scopes it. It shells out to the juju client (as the invoking user
// when running under sudo). controllerName/modelName come from the record flags:
// a model may be given as "controller:model" or bare "model" (resolved within
// the chosen/current controller). An empty scope records everything.
func resolveScope(controllerName, modelName string) (*jujuTopology, probeFilter, error) {
	// A "controller:model" model spec overrides the controller flag.
	if c, m, ok := strings.Cut(modelName, ":"); ok {
		controllerName, modelName = c, m
	}
	if controllerName == "" {
		cj, err := runJujuControllers()
		if err != nil {
			return nil, probeFilter{}, err
		}
		controllerName = cj.CurrentController
		if controllerName == "" && len(cj.Controllers) == 1 {
			for name := range cj.Controllers {
				controllerName = name
			}
		}
		if controllerName == "" {
			return nil, probeFilter{}, fmt.Errorf("no controller given and no current juju controller; pass --controller")
		}
	}

	mj, err := runJujuModels(controllerName)
	if err != nil {
		return nil, probeFilter{}, err
	}
	topo := &jujuTopology{
		controllerName:       controllerName,
		modelName:            map[string]string{},
		controllerNameByUUID: map[string]string{},
	}
	for _, m := range mj.Models {
		topo.modelName[m.ModelUUID] = m.ShortName
		if m.ControllerUUID != "" {
			topo.controllerUUID = m.ControllerUUID
			topo.controllerNameByUUID[m.ControllerUUID] = m.ControllerName
		}
	}

	var filter probeFilter
	if modelName != "" {
		uuid := ""
		for _, m := range mj.Models {
			if m.ShortName == modelName {
				uuid = m.ModelUUID
				break
			}
		}
		if uuid == "" {
			return nil, probeFilter{}, fmt.Errorf("model %q not found on controller %q", modelName, controllerName)
		}
		filter.modelUUID = uuid
	} else {
		filter.controllerUUID = topo.controllerUUID
	}
	return topo, filter, nil
}

// probeFilter is the recorder-side representation of the scope; it is passed to
// the probe as --controller-uuid / --model-uuid flags.
type probeFilter struct {
	controllerUUID string
	modelUUID      string
}

func (f probeFilter) args() []string {
	switch {
	case f.modelUUID != "":
		return []string{"--model-uuid", f.modelUUID}
	case f.controllerUUID != "":
		return []string{"--controller-uuid", f.controllerUUID}
	default:
		return nil
	}
}

func runJujuControllers() (*controllersJSON, error) {
	out, err := runJuju("controllers", "--format", "json")
	if err != nil {
		return nil, err
	}
	var cj controllersJSON
	if err := json.Unmarshal(out, &cj); err != nil {
		return nil, fmt.Errorf("parsing juju controllers output: %w", err)
	}
	return &cj, nil
}

func runJujuModels(controller string) (*modelsJSON, error) {
	out, err := runJuju("models", "-c", controller, "--format", "json")
	if err != nil {
		return nil, err
	}
	var mj modelsJSON
	if err := json.Unmarshal(out, &mj); err != nil {
		return nil, fmt.Errorf("parsing juju models output: %w", err)
	}
	return &mj, nil
}

// runJuju invokes the juju client. record needs root for eBPF, but the juju
// client config lives in the invoking user's home, so when we are running under
// sudo we drop back to $SUDO_USER for these read-only lookups.
func runJuju(args ...string) ([]byte, error) {
	name, full := jujuCommand(args)
	cmd := exec.Command(name, full...)
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("running `juju %s`: %w%s", strings.Join(args, " "), err, suffix(detail))
	}
	return out, nil
}

// jujuCommand returns the command to run, wrapping in `sudo -u $SUDO_USER` when
// we are root under sudo so juju reads the operator's config, not root's.
func jujuCommand(args []string) (string, []string) {
	if os.Geteuid() == 0 {
		if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
			return "sudo", append([]string{"-u", u, "juju"}, args...)
		}
	}
	return "juju", args
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
