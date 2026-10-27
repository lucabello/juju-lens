package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/unit"
)

// unitIngester runs `juju-lens-probe --unit` on every unit in scope for
// `record --attach unit`, and writes what it streams back under
// raw/unit/<model>/<host>/. It needs nothing but `juju ssh`: no host access,
// no kubectl, nothing installed beforehand.
//
// Each session streams the probe binary over stdin into /tmp, then runs it;
// the probe deletes the file as it starts. On Kubernetes there is one
// session per unit, into its charm container; on machines one per machine,
// with sudo, next to the machine agent that runs every unit there. It
// reconciles on a ticker: units that appear get a probe, and a session that
// ends (pod recreated, dropped connection) is started again.
type unitIngester struct {
	layout     recording.Layout
	controller string
	discover   bool
	lifetime   time.Duration // passed to the probe as a safety net; 0 = none
	profile    bool          // the tap samples stacks (record --profile)
	linger     time.Duration // workload burst sampling after a hook's last Pebble change (record --linger)
	// podLogs: on Kubernetes the probes also follow the workload containers'
	// logs through their Pebble (record --k8s-log), and the recorder writes
	// them where `kubectl logs` would have put them.
	podLogs bool
	runID   string // names this recorder's uploads on the units
	// onAllLive is called once, when every target found by the first
	// listing has a probe that said hello.
	onAllLive func(n int)

	ctx context.Context
	wg  *sync.WaitGroup

	// installs bounds concurrent binary uploads, so a large model doesn't
	// push hundreds of copies through the controller at once.
	installs chan struct{}

	mu        sync.Mutex
	models    map[string]string      // model short-name -> "caas" | "iaas"
	active    map[string]*unitStream // by target key
	initial   map[string]bool        // targets of the first listing, until each said hello
	listed    bool
	readyDone bool
	backoff   map[string]time.Time // target key -> no new session before
	lastErr   map[string]string    // target key -> last reported launch error
	sessions  []recording.SourceStatus
	lines     int64

	logMu    sync.Mutex
	logFiles map[string]*recording.RotatingWriter // by model, pod and container
	logLines int64
}

type unitStream struct {
	key    string
	label  string // "<model> <juju ssh target>", for stderr
	name   string // "unit <model>/<host>", the manifest source name
	writer *recording.RotatingWriter
	start  time.Time
	lines  int64       // records written; only the session's read goroutine touches it
	arch   chan string // what the host's `uname -m` printed
}

// unitTarget is where one probe runs: the `juju ssh` target, and the host
// segment of its raw/unit path.
type unitTarget struct {
	model string
	ssh   string // "grafana/0" or "3"
	host  string // "grafana-0" or "machine-3"
	sudo  bool
}

func (t unitTarget) key() string { return t.model + "\x00" + t.host }

const (
	// unitInstallConcurrency is how many probe uploads run at once.
	unitInstallConcurrency = 8
	// unitReconcileEvery is how often the ingester looks for units without
	// a probe.
	unitReconcileEvery = 10 * time.Second
	// unitBusyRetry is how long a unit owned by another recording is left
	// alone before trying again.
	unitBusyRetry = 2 * time.Minute
)

// newUnitIngester prepares unit probes for the given models (short-name ->
// "caas" | "iaas"). With discover, models created later are picked up too.
func newUnitIngester(layout recording.Layout, controller string, discover bool, models map[string]string,
	lifetime time.Duration, profile bool, linger time.Duration, podLogs bool, onAllLive func(int)) *unitIngester {
	ui := &unitIngester{
		layout: layout, controller: controller, discover: discover,
		lifetime: lifetime, profile: profile, linger: linger, podLogs: podLogs, onAllLive: onAllLive,
		logFiles: map[string]*recording.RotatingWriter{},
		runID:    newRunID(),
		installs: make(chan struct{}, unitInstallConcurrency),
		models:   map[string]string{},
		active:   map[string]*unitStream{},
		initial:  map[string]bool{},
		backoff:  map[string]time.Time{},
		lastErr:  map[string]string{},
	}
	for m, t := range models {
		ui.models[m] = t
	}
	return ui
}

// newRunID names one recorder's uploads, so two recorders, even on
// different hosts with the same PID, never write the same file on a unit.
func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
}

func (ui *unitIngester) start(ctx context.Context, wg *sync.WaitGroup) {
	ui.ctx = ctx
	ui.wg = wg
	wg.Add(1)
	go func() {
		defer wg.Done()
		ui.reconcile()
		t := time.NewTicker(unitReconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ui.reconcile()
			}
		}
	}()
}

// reconcile starts a probe on every target in scope that doesn't have one.
func (ui *unitIngester) reconcile() {
	if ui.discover {
		if mj, err := runJujuModels(ui.controller); err == nil {
			ui.mu.Lock()
			for _, m := range mj.Models {
				if m.ShortName != "" && (m.ModelType == "caas" || m.ModelType == "iaas") {
					ui.models[m.ShortName] = m.ModelType
				}
			}
			ui.mu.Unlock()
		}
	}
	ui.mu.Lock()
	models := make(map[string]string, len(ui.models))
	for m, t := range ui.models {
		models[m] = t
	}
	ui.mu.Unlock()

	var targets []unitTarget
	complete := true
	for model, typ := range models {
		ts, err := unitTargets(ui.controller, model, typ)
		if err != nil {
			complete = false // model gone / juju hiccup: retry next tick
			continue
		}
		targets = append(targets, ts...)
	}
	ui.mu.Lock()
	if complete {
		current := make(map[string]bool, len(targets))
		for _, t := range targets {
			current[t.key()] = true
		}
		if !ui.listed {
			ui.listed = true
			ui.initial = current
		}
		for k := range ui.initial {
			if !current[k] {
				delete(ui.initial, k) // removed since the first listing
			}
		}
	}
	ui.mu.Unlock()
	ui.checkReady()
	for _, t := range targets {
		if ui.ctx.Err() != nil {
			return
		}
		ui.ensure(t)
	}
}

// unitTargets lists where probes run for a model: its units on Kubernetes,
// its machines otherwise. A machine agent runs every unit on its machine, so
// one probe per machine covers them all, subordinates included.
func unitTargets(controller, model, modelType string) ([]unitTarget, error) {
	if modelType == "iaas" {
		ids, err := runJujuMachines(controller, model)
		if err != nil {
			return nil, err
		}
		out := make([]unitTarget, 0, len(ids))
		for _, id := range ids {
			out = append(out, unitTarget{model: model, ssh: id, host: "machine-" + id, sudo: true})
		}
		return out, nil
	}
	out, err := runJuju("status", "-m", controller+":"+model, "--format", "json")
	if err != nil {
		return nil, err
	}
	units, err := parseStatusUnits(out)
	if err != nil {
		return nil, err
	}
	ts := make([]unitTarget, 0, len(units))
	for _, u := range units {
		ts = append(ts, unitTarget{model: model, ssh: u, host: strings.ReplaceAll(u, "/", "-")})
	}
	return ts, nil
}

// parseStatusUnits lists the unit names in `juju status --format json`
// output, sorted.
func parseStatusUnits(out []byte) ([]string, error) {
	var st struct {
		Applications map[string]struct {
			Units map[string]json.RawMessage `json:"units"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parsing juju status output: %w", err)
	}
	var units []string
	for _, app := range st.Applications {
		for u := range app.Units {
			units = append(units, u)
		}
	}
	sort.Strings(units)
	return units, nil
}

func (ui *unitIngester) ensure(t unitTarget) {
	key := t.key()
	ui.mu.Lock()
	if _, ok := ui.active[key]; ok || time.Now().Before(ui.backoff[key]) {
		ui.mu.Unlock()
		return
	}
	st := &unitStream{key: key, label: t.model + " " + t.ssh, name: "unit " + t.model + "/" + t.host, start: time.Now().UTC()}
	ui.active[key] = st
	ui.mu.Unlock()

	if err := ui.launch(st, t); err != nil {
		ui.mu.Lock()
		delete(ui.active, key)
		repeat := ui.lastErr[key] == err.Error()
		ui.lastErr[key] = err.Error()
		ui.mu.Unlock()
		// A reconcile still in flight at shutdown fails here with the
		// context's error; that is not worth reporting.
		if ui.ctx.Err() == nil && !repeat {
			fmt.Fprintf(os.Stderr, "juju-lens: unit probe for %s: %v\n", st.label, err)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "juju-lens: starting unit probe on %s\n", st.label)
}

// probeScript is the remote command: see uploadScript. The file name is
// unique to this recorder.
func (ui *unitIngester) probeScript(t unitTarget) string {
	path := uploadPath(ui.runID)
	args := []string{path, "--unit", "--unlink-self"}
	if ui.lifetime > 0 {
		args = append(args, "--lifetime", ui.lifetime.String())
	}
	if !ui.profile {
		args = append(args, "--no-stacks")
	}
	args = append(args, "--linger", ui.linger.String())
	if ui.podLogs && ui.models[t.model] == "caas" {
		args = append(args, "--pebble-logs")
	}
	run := strings.Join(args, " ")
	if t.sudo {
		run = "sudo " + run
	}
	return uploadScript(path, run, true)
}

func (ui *unitIngester) launch(st *unitStream, t unitTarget) error {
	name, full := jujuCommand([]string{
		"ssh", "-m", ui.controller + ":" + t.model, "--pty=false", t.ssh, ui.probeScript(t),
	})
	cmd := exec.CommandContext(ui.ctx, name, full...)
	// On machines `juju ssh` runs ssh as a child, which keeps the session
	// and its stdout open if only juju is killed. Ending the recording
	// (cancelling ctx) kills the whole group, so the session closes, the
	// probe's next heartbeat fails and it cleans up on the unit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	st.arch = make(chan string, 1)
	st.writer = recording.NewRotatingWriter(ui.layout.UnitFileFor(t.model, t.host), func() time.Time { return time.Now() })

	// The session says what architecture the host is, and gets the probe for
	// it; closing stdin then ends `cat`, and the shell starts the probe.
	ui.wg.Add(1)
	go func() {
		defer ui.wg.Done()
		defer stdin.Close()
		var uname string
		select {
		case uname = <-st.arch:
		case <-ui.ctx.Done():
			return
		case <-time.After(2 * time.Minute):
			fmt.Fprintf(os.Stderr, "juju-lens: unit probe %s: the host didn't say what architecture it is\n", st.label)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return
		}
		bin, err := ui.openFor(uname)
		if err != nil {
			if ui.ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "juju-lens: unit probe %s: %v\n", st.label, err)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return
		}
		defer bin.Close()
		select {
		case ui.installs <- struct{}{}:
		case <-ui.ctx.Done():
			return
		}
		_, err = io.Copy(stdin, bin)
		<-ui.installs
		if err != nil && ui.ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "juju-lens: unit probe %s: uploading: %v\n", st.label, err)
		}
	}()
	var lastStderr atomic.Value
	lastStderr.Store("")
	stderrDone := make(chan struct{})
	ui.wg.Add(1)
	go func() {
		defer ui.wg.Done()
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lastStderr.Store(sc.Text())
			fmt.Fprintf(os.Stderr, "unit %s: %s\n", st.label, sc.Text())
		}
	}()
	ui.wg.Add(1)
	go func() {
		defer ui.wg.Done()
		said, busy := ui.read(st, stdout)
		<-stderrDone
		_ = cmd.Wait()
		_ = st.writer.Close()
		ui.finish(st, said, busy, lastStderr.Load().(string))
	}()
	return nil
}

// openFor opens the probe for a host whose `uname -m` printed uname.
func (ui *unitIngester) openFor(uname string) (io.ReadCloser, error) {
	arch, err := goArchOf(uname)
	if err != nil {
		return nil, err
	}
	return openProbe(arch)
}

// read copies the probe's records into raw/unit/ until the session ends,
// and reports whether the probe said hello, or found the unit busy.
func (ui *unitIngester) read(st *unitStream, stdout io.Reader) (said, busy bool) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if bytes.HasPrefix(line, []byte(archPrefix)) {
			select {
			case st.arch <- string(line[len(archPrefix):]):
			default:
			}
			continue
		}
		if len(line) == 0 || line[0] != '{' {
			continue // not a probe record (e.g. a login banner)
		}
		if bytes.HasPrefix(line, pebbleLogPrefix) {
			// A workload log line goes where `kubectl logs` would have put it,
			// not in the probe's record.
			ui.writePodLog(st, line)
			continue
		}
		if _, err := st.writer.WriteLine(line); err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: unit probe %s: writing: %v\n", st.label, err)
			return said, busy
		}
		atomic.AddInt64(&ui.lines, 1)
		st.lines++
		var head struct {
			Kind     string         `json:"kind"`
			Busy     *unit.Busy     `json:"busy"`
			Loader   *unit.Loader   `json:"loader"`
			SpoolEnd *unit.SpoolEnd `json:"spool_end"`
		}
		switch {
		case !said && !busy:
			if json.Unmarshal(line, &head) != nil {
				continue
			}
			switch head.Kind {
			case unit.KindHello:
				said = true
				ui.helloFrom(st)
			case unit.KindBusy:
				busy = true
				pid, age := 0, time.Duration(0)
				if head.Busy != nil {
					pid, age = head.Busy.PID, time.Duration(head.Busy.HeartbeatAgeMs)*time.Millisecond
				}
				fmt.Fprintf(os.Stderr, "juju-lens: %s is owned by another juju-lens recording (probe pid %d, heartbeat %s ago); not recording it, retrying in %s\n",
					st.label, pid, age.Round(time.Second), unitBusyRetry)
			}
		// The probe encodes kind first; only loader and spool_end records
		// need decoding.
		case bytes.HasPrefix(line, loaderPrefix):
			if json.Unmarshal(line, &head) == nil && head.Loader != nil {
				reportLoader(st.label, head.Loader)
			}
		case bytes.HasPrefix(line, spoolEndPrefix):
			if json.Unmarshal(line, &head) == nil && head.SpoolEnd != nil && head.SpoolEnd.Truncated {
				fmt.Fprintf(os.Stderr, "juju-lens: %s: hook process %d of %s ended without finishing (killed, or out of memory); its record stops after %d lines\n",
					st.label, head.SpoolEnd.PID, head.SpoolEnd.Unit, head.SpoolEnd.Lines)
			}
		}
	}
	return said, busy
}

var (
	pebbleLogPrefix = []byte(`{"kind":"` + unit.KindPebbleLog + `"`)
	loaderPrefix    = []byte(`{"kind":"` + unit.KindLoader + `"`)
	spoolEndPrefix  = []byte(`{"kind":"` + unit.KindSpoolEnd + `"`)
)

// reportLoader tells the operator about the tap in each charm: where it
// couldn't go, and where it was put back after a refresh.
func reportLoader(label string, l *unit.Loader) {
	switch l.Event {
	case unit.LoaderInstalled:
		fmt.Fprintf(os.Stderr, "juju-lens: %s: tap installed for %s\n", label, l.Unit)
	case unit.LoaderReinstalled:
		fmt.Fprintf(os.Stderr, "juju-lens: %s: tap reinstalled for %s after its venv changed at %s; hooks in between ran without it\n",
			label, l.Unit, l.DirChanged.Local().Format(time.TimeOnly))
	case unit.LoaderSkipped:
		fmt.Fprintf(os.Stderr, "juju-lens: %s: no tap for %s: %s\n", label, l.Unit, l.Reason)
	case unit.LoaderUnavailable:
		fmt.Fprintf(os.Stderr, "juju-lens: %s: no tap on this host: %s\n", label, l.Reason)
	}
}

func (ui *unitIngester) helloFrom(st *unitStream) {
	ui.mu.Lock()
	delete(ui.lastErr, st.key)
	delete(ui.initial, st.key)
	ui.mu.Unlock()
	fmt.Fprintf(os.Stderr, "juju-lens: unit probe running on %s\n", st.label)
	ui.checkReady()
}

// checkReady fires onAllLive once every target of the first listing has a
// probe that said hello. Targets that went away count as done.
func (ui *unitIngester) checkReady() {
	ui.mu.Lock()
	if ui.readyDone || !ui.listed || len(ui.initial) > 0 {
		ui.mu.Unlock()
		return
	}
	ui.readyDone = true
	n := len(ui.active)
	ui.mu.Unlock()
	if ui.onAllLive != nil {
		ui.onAllLive(n)
	}
}

// finish records a session that ended, and lets reconcile start a new one.
func (ui *unitIngester) finish(st *unitStream, said, busy bool, lastStderr string) {
	stopping := ui.ctx.Err() != nil
	src := recording.SourceStatus{
		Name: st.name, Kind: "unit-probe",
		Started: st.start, Stopped: time.Now().UTC(), Records: st.lines,
	}
	switch {
	case stopping:
	case busy:
		src.Error = "unit owned by another juju-lens recording"
	case lastStderr != "":
		src.Error = "session ended: " + lastStderr
	default:
		src.Error = "session ended"
	}
	ui.mu.Lock()
	if ui.active[st.key] == st {
		delete(ui.active, st.key)
	}
	if busy {
		ui.backoff[st.key] = time.Now().Add(unitBusyRetry)
	}
	if said || busy || stopping {
		ui.sessions = append(ui.sessions, src)
	}
	ui.mu.Unlock()
	if said && !stopping {
		fmt.Fprintf(os.Stderr, "juju-lens: unit probe on %s ended (%s); restarting it\n", st.label, src.Error)
	}
}

// writePodLog writes one workload log line of a Kubernetes unit as
// "<time> <time> [<service>] <message>", the shape of `kubectl logs
// --timestamps` for a container that runs under Pebble, so it is indexed like
// those lines, under raw/k8s/<model>/<pod>/<container>/.
func (ui *unitIngester) writePodLog(st *unitStream, line []byte) {
	var rec struct {
		PebbleLog *unit.PebbleLog `json:"pebble_log"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.PebbleLog == nil {
		return
	}
	l := rec.PebbleLog
	model, pod, _ := strings.Cut(st.name[len("unit "):], "/")
	key := model + "\x00" + pod + "\x00" + l.Container
	ui.logMu.Lock()
	w := ui.logFiles[key]
	if w == nil {
		w = recording.NewRotatingWriter(ui.layout.K8sLogFileFor(model, pod, l.Container), func() time.Time { return time.Now() })
		ui.logFiles[key] = w
	}
	ts := l.Time.Format(time.RFC3339Nano)
	msg := strings.NewReplacer("\r", "", "\n", " ").Replace(l.Message)
	_, err := w.WriteLine([]byte(fmt.Sprintf("%s %s [%s] %s", ts, ts, l.Service, msg)))
	ui.logMu.Unlock()
	if err == nil {
		atomic.AddInt64(&ui.logLines, 1)
	}
}

// closeLogs flushes the workload log files. Call it after the sessions ended.
func (ui *unitIngester) closeLogs() {
	ui.logMu.Lock()
	defer ui.logMu.Unlock()
	for _, w := range ui.logFiles {
		_ = w.Close()
	}
}

// podLogCount is the number of workload log lines written so far.
func (ui *unitIngester) podLogCount() int64 { return atomic.LoadInt64(&ui.logLines) }

// count is the number of probe records written so far.
func (ui *unitIngester) count() int64 { return atomic.LoadInt64(&ui.lines) }

// sessionSources lists every probe session that ran, for the manifest. Call
// it after the sessions have ended.
func (ui *unitIngester) sessionSources() []recording.SourceStatus {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	out := append([]recording.SourceStatus(nil), ui.sessions...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}
