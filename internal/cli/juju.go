package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// jujuTopology is the controller/model inventory juju-lens resolves from the
// juju client at record time. It maps the UUIDs the probe derives from each
// agent's agent.conf to the human names the operator thinks in, and it drives
// scope filtering (record only a chosen controller or model).
type jujuTopology struct {
	// controllerName is the controller the recording is scoped to.
	controllerName string
	controllerUUID string

	// mu guards the maps below: they are read by the probe ingest goroutine
	// (via resolve) while the controller-scope discovery ticker refreshes them
	// (via refresh) as models are created during recording.
	mu sync.RWMutex
	// modelName maps a model UUID to its short name (e.g. "cos-lite").
	modelName map[string]string
	// modelType maps a model short name to "caas" or "iaas"; it routes each
	// model to the right log ingester (k8s pod logs vs machine journald).
	modelType map[string]string
	// controllerNameByUUID maps a controller UUID to its name.
	controllerNameByUUID map[string]string
	// lastRefresh rate-limits refresh-on-miss so a burst of RPCs from an
	// unknown model triggers at most one juju query per refreshMinInterval.
	lastRefresh time.Time
}

// refreshMinInterval bounds how often a resolve miss may trigger a juju query.
const refreshMinInterval = 2 * time.Second

// resolve returns the controller/model names for the UUIDs on a captured frame.
// Unknown UUIDs yield "" so the caller can fall back to the UUID itself. When a
// model UUID is unknown (a model created after the last topology refresh, e.g.
// mid-recording), it re-queries juju once (rate-limited) so the model resolves
// to its name promptly instead of surfacing as a duplicate UUID-keyed model.
func (t *jujuTopology) resolve(controllerUUID, modelUUID string) (string, string) {
	if t == nil {
		return "", ""
	}
	t.mu.RLock()
	cn := t.controllerNameByUUID[controllerUUID]
	mn := t.modelName[modelUUID]
	t.mu.RUnlock()
	if mn == "" && modelUUID != "" {
		t.refreshOnMiss()
		t.mu.RLock()
		cn = t.controllerNameByUUID[controllerUUID]
		mn = t.modelName[modelUUID]
		t.mu.RUnlock()
	}
	return cn, mn
}

// refreshOnMiss calls refresh at most once per refreshMinInterval, so a burst of
// RPCs from a not-yet-known model does not spawn a juju query per envelope.
func (t *jujuTopology) refreshOnMiss() {
	t.mu.Lock()
	if time.Since(t.lastRefresh) < refreshMinInterval {
		t.mu.Unlock()
		return
	}
	t.lastRefresh = time.Now()
	t.mu.Unlock()
	t.refresh()
}

// refresh re-queries the controller's models and rebuilds the topology from
// that result, so a model created mid-recording resolves its UUID to a name
// instead of falling back to the raw UUID (which would surface as a separate,
// duplicate model in the viewer). Best-effort: a failed query leaves the
// existing topology untouched.
//
// The rebuild is wholesale, not a merge: a model that no longer appears (e.g.
// a short-lived test model destroyed mid-recording) must drop out of
// modelName here. Otherwise modelsToStream keeps returning it forever, and
// callers driven by it — captureStatusBootstrap, the debug-log/k8s/machine
// discovery tickers — retry and fail against it on every tick for the rest of
// the recording instead of just once.
func (t *jujuTopology) refresh() {
	if t == nil {
		return
	}
	mj, err := runJujuModels(t.controllerName)
	if err != nil {
		return
	}
	modelName, modelType, controllerNameByUUID, _ := buildTopologyMaps(mj)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastRefresh = time.Now()
	t.modelName = modelName
	t.modelType = modelType
	t.controllerNameByUUID = controllerNameByUUID
}

// buildTopologyMaps converts a `juju models` result into the lookup maps
// jujuTopology needs, plus the controller UUID shared by those models. It is
// pure (no I/O) so both resolveScope's initial build and refresh's rebuild go
// through the same logic, and it's unit-testable without shelling out to juju.
func buildTopologyMaps(mj *modelsJSON) (modelName, modelType, controllerNameByUUID map[string]string, controllerUUID string) {
	modelName = make(map[string]string, len(mj.Models))
	modelType = make(map[string]string, len(mj.Models))
	controllerNameByUUID = make(map[string]string, len(mj.Models))
	for _, m := range mj.Models {
		modelName[m.ModelUUID] = m.ShortName
		modelType[m.ShortName] = m.ModelType
		if m.ControllerUUID != "" {
			controllerUUID = m.ControllerUUID
			controllerNameByUUID[m.ControllerUUID] = m.ControllerName
		}
	}
	return modelName, modelType, controllerNameByUUID, controllerUUID
}

// modelNames returns the known model short-names, sorted. It is concurrency-safe
// against refresh.
func (t *jujuTopology) modelNames() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.modelName))
	for _, name := range t.modelName {
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
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
		ModelType      string `json:"model-type"` // "caas" | "iaas"
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
	byUUID, typeByName, controllerByUUID, controllerUUID := buildTopologyMaps(mj)
	topo := &jujuTopology{
		controllerName:       controllerName,
		controllerUUID:       controllerUUID,
		modelName:            byUUID,
		modelType:            typeByName,
		controllerNameByUUID: controllerByUUID,
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

// modelsToStream returns the model short-names debug-log should tail for the
// resolved scope: the single scoped model, or every model on the scoped
// controller. Nil when no juju topology was resolved.
func modelsToStream(topo *jujuTopology, filter probeFilter) []string {
	if topo == nil {
		return nil
	}
	if filter.modelUUID != "" {
		topo.mu.RLock()
		name := topo.modelName[filter.modelUUID]
		topo.mu.RUnlock()
		if name != "" {
			return []string{name}
		}
		return nil
	}
	return topo.modelNames()
}

// splitByModelType partitions model short-names into CAAS and IAAS buckets
// using the topology's model-type map, so each model is routed to the right log
// ingester. Models of unknown type are dropped from both.
func splitByModelType(topo *jujuTopology, models []string) (caas, iaas []string) {
	topo.mu.RLock()
	defer topo.mu.RUnlock()
	for _, m := range models {
		switch topo.modelType[m] {
		case "caas":
			caas = append(caas, m)
		case "iaas":
			iaas = append(iaas, m)
		}
	}
	return caas, iaas
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

// runJujuMachines returns the machine ids of a model, sorted. It is used by the
// journald ingester to discover which hosts to tail.
func runJujuMachines(controller, model string) ([]string, error) {
	out, err := runJuju("machines", "-m", controller+":"+model, "--format", "json")
	if err != nil {
		return nil, err
	}
	var mj struct {
		Machines map[string]json.RawMessage `json:"machines"`
	}
	if err := json.Unmarshal(out, &mj); err != nil {
		return nil, fmt.Errorf("parsing juju machines output: %w", err)
	}
	ids := make([]string, 0, len(mj.Machines))
	for id := range mj.Machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
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
	return userCommand("juju", args)
}

// kubectlCommand is jujuCommand's kubectl analogue: the k8s log ingester needs
// the operator's kubeconfig, which lives in their home, so under sudo we run
// kubectl as $SUDO_USER too.
func kubectlCommand(args []string) (string, []string) {
	return userCommand("kubectl", args)
}

// userCommand wraps a client invocation in `sudo -u $SUDO_USER` when we are root
// under sudo, so the client reads the operator's config (juju/kube) rather than
// root's. Otherwise it runs the binary directly.
func userCommand(bin string, args []string) (string, []string) {
	if os.Geteuid() == 0 {
		if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
			return "sudo", append([]string{"-u", u, bin}, args...)
		}
	}
	return bin, args
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
