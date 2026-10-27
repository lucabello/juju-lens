package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/probe"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/unit"
	"github.com/lucabello/juju-lens/internal/wire"

	"github.com/spf13/cobra"
)

type recordFlags struct {
	output      string
	attach      string
	controller  string
	model       string
	sshTarget   string
	pids        []int
	logs        bool
	debugLog    bool
	k8sLog      bool
	machineLog  bool
	maxDuration time.Duration
	maxSize     int64
	profile     bool
	linger      time.Duration
	apps        []string
	units       []string

	detach        bool
	detachTimeout time.Duration
	// detachedChild marks the re-executed daemon started by --detach. It
	// reports readiness on readyFD and ignores SIGHUP.
	detachedChild bool
}

func newRecordCmd() *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record <name>",
		Short: "Record a Juju controller and its models",
		Long: `record captures what a Juju controller and its models do into a recording
directory. It stops cleanly on Ctrl-C, on SIGTERM (delivered by 'juju-lens
stop'), or when a --max-* cap is reached.

The <name> argument labels the recording; it becomes part of the directory
name and is stored in manifest.json.

--attach chooses what is captured, as a comma-separated set:

  host   run the probe where the Juju agents run, and capture every Juju
         API RPC at their crypto/tls boundary with read-only eBPF uprobes. The
         probe runs on this host (juju-lens runs itself as 'juju-lens probe'),
         or on --ssh-target, to which juju-lens uploads it over 'juju ssh' and
         runs it with sudo. Needs root or CAP_BPF there. This is the default.
  unit   run the probe inside every unit in scope over 'juju ssh': in
         the charm container on Kubernetes, next to the machine agent (with
         sudo) on machine models. Needs nothing but 'juju ssh'. Records the
         charm's side of each hook: ops events, observers, hook tool and
         Pebble calls, CPU and memory, and Python stacks sampled every 10 ms
         (--profile=false turns the stacks off).
  none   capture nothing from the agents: record only logs and the status
         snapshot. Needs nothing but the juju client.

'host,unit' runs both in one recording. The probe is part of juju-lens: it
carries a static build for linux/amd64 and linux/arm64, and uploads the one
that matches each machine's architecture.

--app and --unit, with --model, narrow the recording to some of its
applications or units: only they get a unit probe (on a machine model, the
machines that host them, where the probe still covers every unit), and
debug-log is limited to them. The status snapshot and --attach host still
cover the whole model.

record never changes the controller: host attach only adds read-only uprobes.
Unit attach writes into each unit for as long as it runs:

  - the probe binary, uploaded to /tmp, which deletes itself as it starts;
  - a lock, /tmp/juju-lens-unit.lock, and a spool, /tmp/juju-lens-tap;
  - in every charm venv, a Python module, _juju_lens_tap.py, and a loader
    that imports it as a hook starts: _juju_lens_tap.pth in site-packages,
    or sitecustomize.py for charms that put the venv on PYTHONPATH;
  - in each Kubernetes workload container, one 'sh' started through Pebble
    exec, to read the container's cgroup about 10 times a second while a
    hook runs and for --linger after the last Pebble change it caused.

When the recording ends, the probes remove all of it, with the bytecode
Python cached for the module, and the shells exit; on Kubernetes this can
take up to about 30 seconds. If a probe dies first, the module and loader
stay in the venvs, and the spool and lock may stay in /tmp, but they do
nothing: the module is inert once the probe's heartbeat in the spool is 30
seconds old, and the next --attach unit recording on the unit removes them.
On Kubernetes 'juju ssh' itself leaves a small /tmp/<random>.pid per session.

Alongside, record ingests logs for the scoped models: 'juju debug-log',
workload-container logs for CAAS models (read through each unit probe's Pebble
connection with --attach unit, which needs no cluster access; else with
'kubectl logs'), and machine journald for IAAS models ('juju ssh … journalctl'). Disable any of them with
--debug-log=false / --k8s-log=false / --machine-log=false.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("detach-timeout") && !f.detach {
				return errors.New("--detach-timeout requires --detach")
			}
			sides, err := parseAttach(f.attach)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("profile") && !sides.unit {
				return errors.New("--profile needs --attach unit")
			}
			if cmd.Flags().Changed("linger") && !sides.unit {
				return errors.New("--linger needs --attach unit")
			}
			if f.linger < 0 {
				return errors.New("--linger can't be negative")
			}
			if _, err := f.scope(sides); err != nil {
				return err
			}
			f.applyLogs()
			if f.detach {
				return runDetached(cmd, args[0], *f)
			}
			return runRecord(cmd.Context(), args[0], *f, sides)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/<ts>--<name>)")
	cmd.Flags().StringVar(&f.attach, "attach", "host", "what to capture, comma-separated: "+attachValues+" (see above)")
	cmd.Flags().StringVar(&f.controller, "controller", "", "scope to this juju controller (all its models); default: current controller")
	cmd.Flags().StringVar(&f.model, "model", "", "scope to a single model, as \"model\", \"owner/model\" or \"controller:model\" (\"controller:owner/model\" for a model another user owns, such as on a JAAS controller)")
	cmd.Flags().StringVar(&f.sshTarget, "ssh-target", "", "with --attach host, run the probe on this machine over 'juju ssh' (e.g. 'controller/0')")
	cmd.Flags().IntSliceVar(&f.pids, "pid", nil, "restrict the probe to these PIDs (default: all jujud/containeragent)")
	cmd.Flags().BoolVar(&f.logs, "logs", true, "record logs at all; --logs=false turns off --debug-log, --k8s-log and --machine-log")
	cmd.Flags().BoolVar(&f.debugLog, "debug-log", true, "also stream 'juju debug-log' for the scoped models")
	cmd.Flags().BoolVar(&f.k8sLog, "k8s-log", true, "also record workload-container logs for CAAS models (through the unit probes with --attach unit, else 'kubectl logs')")
	cmd.Flags().BoolVar(&f.machineLog, "machine-log", true, "also stream machine journald for IAAS models ('juju ssh … journalctl')")
	cmd.Flags().DurationVar(&f.maxDuration, "max-duration", 0, "stop recording after this duration (0 = no limit)")
	cmd.Flags().Int64Var(&f.maxSize, "max-size", 0, "stop recording after this many bytes of raw/ (0 = no limit)")
	cmd.Flags().StringSliceVar(&f.apps, "app", nil, "with --model, record only this application's units, such as \"grafana\"; repeatable (see above)")
	cmd.Flags().StringSliceVar(&f.units, "unit", nil, "with --model, record only this unit, such as \"grafana/0\"; repeatable (see above)")
	cmd.Flags().BoolVar(&f.profile, "profile", true, "with --attach unit, sample the charms' Python stacks")
	cmd.Flags().DurationVar(&f.linger, "linger", unit.DefaultLinger, "with --attach unit, keep sampling workloads 10 times a second this long after a hook's last Pebble change is ready")
	cmd.Flags().BoolVar(&f.detach, "detach", false, "run the recorder in the background; return once it is capturing, printing the recording dir and pid")
	cmd.Flags().DurationVar(&f.detachTimeout, "detach-timeout", 30*time.Second, "with --detach, fail if the recorder isn't capturing within this long")
	cmd.Flags().BoolVar(&f.detachedChild, detachedChildFlag, false, "internal: set on the daemon started by --detach")
	_ = cmd.Flags().MarkHidden(detachedChildFlag)
	return cmd
}

// applyLogs turns every log source off for --logs=false.
func (f *recordFlags) applyLogs() {
	if !f.logs {
		f.debugLog, f.k8sLog, f.machineLog = false, false, false
	}
}

// scope checks --app and --unit, which name things inside one model.
func (f recordFlags) scope(sides attachSides) (unitScope, error) {
	s, err := newUnitScope(f.apps, f.units)
	if err != nil {
		return s, err
	}
	if s.active() && f.model == "" {
		return s, errors.New("--app and --unit need --model: an application name means nothing across models")
	}
	if s.active() && sides.host {
		fmt.Fprintln(os.Stderr, "juju-lens: --app and --unit narrow the unit probes and debug-log; --attach host still records every agent's traffic")
	}
	return s, nil
}

func runRecord(ctx context.Context, name string, f recordFlags, sides attachSides) error {
	if f.detachedChild {
		// The readiness pipe arrives without close-on-exec; keep it out of
		// the probe and log subprocesses so the parent sees EOF as soon as
		// this process dies, not when its last child does.
		syscall.CloseOnExec(readyFD)
	}
	if f.output == "" {
		f.output = filepath.Join("recordings", recording.SuggestedDirName(name, time.Now()))
	}
	scope, err := f.scope(sides)
	if err != nil {
		return err
	}
	layout := recording.NewLayout(f.output)
	if err := layout.Init(); err != nil {
		return err
	}
	// Under sudo, give the recording to the invoking user as it grows, so
	// `view --follow` and `index` need no root. The files created while
	// recording are handed over again at the end.
	defer func() { _ = recording.HandToInvoker(layout.Root) }()
	_ = recording.HandToInvoker(layout.Root)

	// Refuse to overwrite an in-flight recording. Two recorders writing the
	// same raw/ would produce corrupt JSONL.
	if pid, err := recording.ReadPidFile(layout.PidFile()); err == nil && recording.PidAlive(pid) {
		return fmt.Errorf("another recorder (PID %d) is already writing to %s; stop it with `juju-lens stop %s`",
			pid, layout.Root, layout.Root)
	}
	if err := recording.WritePidFile(layout.PidFile(), os.Getpid()); err != nil {
		return fmt.Errorf("writing pid file: %w", err)
	}
	defer recording.RemovePidFile(layout.PidFile())

	// Resolve the controller/model scope to a probe filter and a UUID->name
	// resolver via the juju client. An explicit scope must resolve; with no
	// scope we try the current controller and fall back to unfiltered capture
	// if juju is unavailable.
	topo, filter, scopeErr := resolveScope(f.controller, f.model)
	if scopeErr != nil {
		if f.controller != "" || f.model != "" {
			return scopeErr
		}
		if !sides.host {
			return fmt.Errorf("--attach %s needs the juju client: %w", sides, scopeErr)
		}
		fmt.Fprintf(os.Stderr, "juju-lens: could not query juju (%v); recording all agents unfiltered, without name resolution\n", scopeErr)
		topo, filter = nil, probeFilter{}
	}
	var resolver probe.NameResolver
	controllerLabel := f.controller
	if topo != nil {
		resolver = topo.resolve
		controllerLabel = topo.controllerName
		fmt.Fprintf(os.Stderr, "juju-lens: scope %s\n", describeScope(topo, filter))
	}

	man := recording.New("juju-lens", "record", "", name)
	man.Controller.Name = controllerLabel
	man.AttachMode = sides.String()
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("writing initial manifest: %w", err)
	}

	var probeCmd *exec.Cmd
	if sides.host {
		var target string
		probeCmd, target, err = buildProbeCommand(f, filter)
		if err != nil {
			return err
		}
		man.AttachTargets = []string{target}
		man.AddSource(recording.SourceStatus{Name: "rpc", Kind: "rpc", Started: time.Now().UTC()})
	}
	if sides.unit {
		// The unit probe is uploaded from here: the one juju-lens carries.
		if err = probeAvailable(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Optional log sources, all sharing the recording's context so they stop with
	// everything else: `juju debug-log` per model, plus per-model-type ingesters
	// (CAAS workload logs, IAAS machine journald). Controller scope (no single
	// model pinned) keeps discovering models added during the recording.
	var logIng *logIngester
	var k8sIng *k8sIngester
	var machIng *machineIngester
	var unitIng *unitIngester
	var logWG sync.WaitGroup
	var ready *readyPipe
	if f.detachedChild {
		var waitFor []string
		if sides.host {
			waitFor = append(waitFor, "host")
		}
		if sides.unit {
			waitFor = append(waitFor, "unit")
		}
		ready = newReadyPipe(waitFor...)
	}
	if topo != nil {
		models := modelsToStream(topo, filter)
		// Ground-truth status bootstrap (M8): snapshot `juju status` for each
		// scoped model before attaching the probe, so the Status pane is
		// populated from t0 rather than all-"unknown".
		captureStatusBootstrap(layout, topo.controllerName, models)
		discover := filter.modelUUID == ""
		// Controller scope: keep the UUID->name topology fresh so RPCs from a
		// model created mid-recording resolve to its name instead of falling
		// back to the raw UUID (which would show as a duplicate model in the
		// viewer, alongside the name-keyed row the log discovery creates).
		if discover {
			logWG.Add(1)
			go func() {
				defer logWG.Done()
				t := time.NewTicker(20 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						topo.refresh()
						// Bootstrap status for any model that appeared since t0
						// (e.g. an ephemeral test model created mid-recording).
						// Idempotent: models already captured are skipped, so
						// only the new ones cost a `juju status` call. Without
						// this their applications stay "unknown", since app
						// status is seeded from the bootstrap, not RPC traffic.
						captureStatusBootstrap(layout, topo.controllerName, modelsToStream(topo, filter))
					}
				}
			}()
		}
		if f.debugLog && len(models) > 0 {
			logIng = newLogIngester(layout, topo.controllerName, discover)
			logIng.include = scope.debugLogInclude()
			man.AddSource(recording.SourceStatus{Name: "debug-log", Kind: "juju-debug-log", Started: time.Now().UTC()})
			logIng.start(ctx, &logWG, models)
		}
		caas, iaas := splitByModelType(topo, models)
		// Workload logs of a CAAS model: through the unit probes' Pebble
		// connection when they run, which needs no cluster access, else with
		// `kubectl logs`.
		if f.k8sLog && (len(caas) > 0 || discover) {
			man.AddSource(recording.SourceStatus{Name: "k8s-log", Kind: "k8s-logs", Started: time.Now().UTC()})
			if !sides.unit {
				k8sIng = newK8sIngester(layout, topo.controllerName, discover, caas)
				k8sIng.start(ctx, &logWG)
			}
		}
		if f.machineLog && (len(iaas) > 0 || discover) {
			machIng = newMachineIngester(layout, topo.controllerName, discover, iaas)
			man.AddSource(recording.SourceStatus{Name: "machine-log", Kind: "machine-journald", Started: time.Now().UTC()})
			machIng.start(ctx, &logWG)
		}
		if sides.unit {
			byType := map[string]string{}
			for _, m := range caas {
				byType[m] = "caas"
			}
			for _, m := range iaas {
				byType[m] = "iaas"
			}
			var onAllLive func(int)
			if ready != nil {
				onAllLive = func(n int) {
					ready.partReady("unit", fmt.Sprintf("unit probes running on %d target(s)", n))
				}
			}
			unitIng = newUnitIngester(layout, topo.controllerName, discover, scope, byType, f.maxDuration, f.profile, f.linger, f.k8sLog, onAllLive)
			unitIng.start(ctx, &logWG)
		}
	}

	var stdout io.ReadCloser
	if probeCmd != nil {
		if stdout, err = probeCmd.StdoutPipe(); err != nil {
			return err
		}
		// Surface probe stderr so attach failures are visible to the operator.
		stderr, err := probeCmd.StderrPipe()
		if err != nil {
			return err
		}
		if err := probeCmd.Start(); err != nil {
			return fmt.Errorf("starting probe (%s): %w", probeCmd.Path, err)
		}
		go func() {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				fmt.Fprintf(os.Stderr, "probe: %s\n", sc.Text())
			}
		}()
	}

	fmt.Fprintf(os.Stderr, "juju-lens: recording %q with --attach %s (pid %d)\n", name, sides, os.Getpid())
	fmt.Fprintf(os.Stderr, "juju-lens: stop this recording with `juju-lens stop %s` (or SIGINT/SIGTERM)\n", layout.Root)

	// The sink demultiplexes captured messages into one rotating writer per
	// model. curTs tracks the message being written so files bucket by the
	// message's own capture hour.
	w := &rpcWriters{layout: layout}
	defer w.closeAll()

	// Incremental indexer: keep index.db fresh as raw/ grows so `view --follow`
	// can tail a live recording. A final authoritative full rebuild runs at
	// stop, so this only needs to be fresh, not perfect. idxDone signals that
	// the goroutine has flushed and closed its handle, so the final rebuild can
	// safely replace index.db without racing it.
	var idxDone chan struct{}
	if idxDB, derr := index.Open(layout.IndexDB()); derr == nil {
		// Before the live viewer can open it: SQLite gives index.db-wal and
		// -shm the owner of index.db.
		_ = recording.HandToInvoker(layout.Root)
		idxDone = make(chan struct{})
		indexer := index.NewIndexer(idxDB)
		go func() {
			defer close(idxDone)
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					_ = indexer.Sync(layout.Root)
					_ = idxDB.Close()
					return
				case <-t.C:
					_ = indexer.Sync(layout.Root)
				}
			}
		}()
	} else {
		fmt.Fprintf(os.Stderr, "juju-lens: live index disabled: %v\n", derr)
	}

	endReason := recording.EndReasonUnknown
	sigCh := make(chan os.Signal, 1)
	if f.detachedChild {
		// A daemon has no terminal to hang up; a stray SIGHUP (for example
		// from a CI runner tearing down the step's process tree) must not
		// end the recording. Only `stop` (SIGTERM) and the caps do.
		signal.Ignore(syscall.SIGHUP)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	} else {
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	}
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-ctx.Done():
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "juju-lens: got %s, stopping\n", sig)
			endReason = recording.EndReasonSignal
			cancel()
		}
	}()
	if f.maxDuration > 0 {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(f.maxDuration):
				fmt.Fprintf(os.Stderr, "juju-lens: hit --max-duration %s, stopping\n", f.maxDuration)
				endReason = recording.EndReasonMaxDuration
				cancel()
			}
		}()
	}
	if f.maxSize > 0 {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if w.total() >= f.maxSize {
						fmt.Fprintf(os.Stderr, "juju-lens: hit --max-size %d bytes, stopping\n", f.maxSize)
						endReason = recording.EndReasonMaxSize
						cancel()
						return
					}
				}
			}
		}()
	}

	// onDrops surfaces the probe's own userspace-side frame loss (M13: it
	// buffers between the kernel ring-buffer drain and the pipe to us, so a
	// slow consumer here — a burst of spans to index — can no longer cause a
	// silent kernel-level drop instead). Reported as it happens so a capture
	// gap is traceable to "we were slow here" rather than a mystery later.
	var totalDrops uint64
	onDrops := func(n uint64) {
		totalDrops = n
		fmt.Fprintf(os.Stderr, "juju-lens: probe reports %d frames dropped so far (it's falling behind — see docs/explanation/from-wire-to-viewer.md#limitations)\n", n)
	}
	var ingestErr error
	if probeCmd != nil {
		var onAttached func(int)
		if ready != nil {
			onAttached = ready.onAttached
		}
		ingestErr = probe.Ingest(ctx, stdout, resolver, w.sink, onDrops, onAttached)
	} else {
		// No RPC probe: the log sources (and unit probes) are the recording,
		// and they run until a signal or a cap cancels ctx. With no capture
		// side, the log sources are already started, so the recorder is
		// capturing now; unit probes report their own readiness.
		if ready != nil && !sides.unit {
			ready.partReady("logs", "recording logs and status only")
		}
		<-ctx.Done()
	}
	cancel()
	if totalDrops > 0 {
		fmt.Fprintf(os.Stderr, "juju-lens: recording finished with %d frames dropped by the probe; some hooks may show a \"capture gap\"\n", totalDrops)
	}
	if probeCmd != nil {
		// The probe is a child (local) or an ssh session; either way, killing
		// the process ends it and its remote peer.
		_ = probeCmd.Process.Kill()
		_ = probeCmd.Wait()
	}

	// Stop the log ingesters (ctx is already cancelled, which signals them and
	// ends the unit probe sessions) and wait for their goroutines to flush.
	if logIng != nil {
		logIng.stop()
	}
	if k8sIng != nil {
		k8sIng.stop()
	}
	if machIng != nil {
		machIng.stop()
	}
	logWG.Wait()
	if logIng != nil {
		man.FinishSourceRecords("debug-log", logIng.count(), nil)
	}
	if k8sIng != nil {
		man.FinishSourceRecords("k8s-log", k8sIng.count(), nil)
	}
	if machIng != nil {
		man.FinishSourceRecords("machine-log", machIng.count(), nil)
	}
	if unitIng != nil {
		unitIng.closeLogs()
		if f.k8sLog {
			man.FinishSourceRecords("k8s-log", unitIng.podLogCount(), nil)
		}
		for _, s := range unitIng.sessionSources() {
			man.AddSource(s)
		}
		// The hooks the units ran, to cross-check what the tap recorded.
		captureStatusLog(layout, topo.controllerName, man.Started)
	}

	if endReason == recording.EndReasonUnknown {
		if ingestErr != nil && !errors.Is(ingestErr, context.Canceled) {
			endReason = recording.EndReasonError
		} else {
			endReason = recording.EndReasonUserRequested
		}
	}
	if probeCmd != nil {
		man.FinishSource("rpc", nil)
	}
	man.Finalize(endReason)
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("finalising manifest: %w", err)
	}
	// Wait for the live indexer to flush and release index.db, then rebuild it
	// authoritatively (accurate hook labelling, no partial state). The rebuild
	// is in-place (reset, not remove) so a `view --follow` that has the DB open
	// sees the final data rather than being stranded on an unlinked file.
	if idxDone != nil {
		<-idxDone
	}
	if err := reindexInPlace(layout); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: indexing recording failed: %v (run `juju-lens index %s` to retry)\n",
			err, layout.Root)
	}
	var logLines int64
	if logIng != nil {
		logLines += logIng.count()
	}
	if k8sIng != nil {
		logLines += k8sIng.count()
	}
	if machIng != nil {
		logLines += machIng.count()
	}
	logMsg := ""
	if logLines > 0 {
		logMsg = fmt.Sprintf(", %d log lines", logLines)
	}
	if unitIng != nil {
		logMsg += fmt.Sprintf(", %d unit probe records", unitIng.count())
	}
	fmt.Fprintf(os.Stderr, "juju-lens: recording saved to %s (%d RPC messages%s captured)\n",
		layout.Root, w.count(), logMsg)
	if ingestErr != nil && !errors.Is(ingestErr, context.Canceled) {
		return ingestErr
	}
	return nil
}

// describeScope renders the resolved recording scope for the operator.
func describeScope(topo *jujuTopology, filter probeFilter) string {
	if filter.modelUUID != "" {
		return fmt.Sprintf("model %s (%s) on controller %s", topo.modelName[filter.modelUUID], filter.modelUUID, topo.controllerName)
	}
	if filter.controllerUUID != "" {
		return fmt.Sprintf("controller %s (%s), all models", topo.controllerName, filter.controllerUUID)
	}
	return "all controllers/models (unfiltered)"
}

// reindexInPlace opens the recording's existing index.db and rebuilds it in
// place (without unlinking the file), so a concurrent `view --follow` picks up
// the authoritative final data on its next tick.
func reindexInPlace(layout recording.Layout) error {
	db, err := index.Open(layout.IndexDB())
	if err != nil {
		return err
	}
	defer db.Close()
	return rebuildIndex(db, layout.Root, nil, true)
}

// buildProbeCommand constructs the exec.Cmd that runs the host probe, locally
// or on --ssh-target over `juju ssh`, and returns a human-readable target for
// the manifest. Locally the probe is this binary, as `juju-lens probe`; on a
// remote machine juju-lens uploads the probe for its architecture and runs it
// with sudo.
func buildProbeCommand(f recordFlags, filter probeFilter) (*exec.Cmd, string, error) {
	args := probeArgs(f, filter)
	if f.sshTarget == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, "", err
		}
		return exec.Command(self, append([]string{"probe"}, args...)...), "localhost", nil
	}
	if err := probeAvailable(); err != nil {
		return nil, "", err
	}
	ssh := []string{"ssh"}
	if f.controller != "" {
		ssh = append(ssh, "-m", f.controller+":controller")
	}
	ssh = append(ssh, f.sshTarget)
	out, err := exec.Command("juju", append(ssh, "uname -m")...).Output()
	if err != nil {
		return nil, "", fmt.Errorf("asking %s for its architecture over juju ssh: %w", f.sshTarget, err)
	}
	arch, err := goArchOf(string(out))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", f.sshTarget, err)
	}
	bin, err := openProbe(arch)
	if err != nil {
		return nil, "", err
	}
	path := uploadPath(newRunID())
	quoted := make([]string, 0, len(args)+3)
	quoted = append(quoted, "sudo", path, "--unlink-self")
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	cmd := exec.Command("juju", append(ssh, uploadScript(path, strings.Join(quoted, " "), false))...)
	cmd.Stdin = bin
	return cmd, f.sshTarget, nil
}

func probeArgs(f recordFlags, filter probeFilter) []string {
	var args []string
	if len(f.pids) > 0 {
		strs := make([]string, len(f.pids))
		for i, p := range f.pids {
			strs[i] = strconv.Itoa(p)
		}
		args = append(args, "--pid", strings.Join(strs, ","))
	}
	args = append(args, filter.args()...)
	return args
}

// rpcWriters owns one RotatingWriter per model and delivers captured messages
// to the right one. It is safe for the single ingest goroutine plus the
// max-size ticker (which only reads totals).
type rpcWriters struct {
	layout recording.Layout
	mu     sync.Mutex
	curTs  time.Time
	ws     map[string]*recording.RotatingWriter
	n      int64
}

func (w *rpcWriters) sink(cm wire.CapturedMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws == nil {
		w.ws = map[string]*recording.RotatingWriter{}
	}
	w.curTs = cm.Ts
	rw := w.ws[cm.Model]
	if rw == nil {
		rw = recording.NewRotatingWriter(w.layout.RPCFileFor(cm.Model), func() time.Time { return w.curTs })
		w.ws[cm.Model] = rw
	}
	line, err := cm.MarshalLine()
	if err != nil {
		return err
	}
	if _, err := rw.WriteLine(line); err != nil {
		return err
	}
	w.n++
	return nil
}

func (w *rpcWriters) total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var t int64
	for _, rw := range w.ws {
		t += rw.Total()
	}
	return t
}

func (w *rpcWriters) count() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func (w *rpcWriters) closeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, rw := range w.ws {
		_ = rw.Close()
	}
}
