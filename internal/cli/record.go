package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
	"github.com/lucabello/juju-lens/internal/wire"

	"github.com/spf13/cobra"
)

type recordFlags struct {
	output      string
	attach      string
	controller  string
	model       string
	sshTarget   string
	probePath   string
	pids        []int
	debugLog    bool
	k8sLog      bool
	machineLog  bool
	maxDuration time.Duration
	maxSize     int64
}

func newRecordCmd() *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record <name>",
		Short: "Record Juju API RPCs captured at the TLS boundary via eBPF",
		Long: `record attaches juju-lens-probe to a controller's jujud/containeragent
processes, captures every Juju API RPC at the crypto/tls boundary, and writes
them to a recording directory. It stops cleanly on Ctrl-C, on SIGTERM
(delivered by 'juju-lens stop'), or when a --max-* cap is reached.

The <name> argument labels the recording; it becomes part of the directory
name and is stored in manifest.json.

record never mutates the controller: it only attaches read-only uprobes.
Attach modes:

  local   run the probe on this host (for a locally-installed jujud snap)
  ssh     run the probe on a machine controller over 'juju ssh' (default when
          --controller or --ssh-target is given)

Alongside the RPC probe, record ingests logs for the scoped models: 'juju
debug-log', workload-container stdout for CAAS models ('kubectl logs'), and
machine journald for IAAS models ('juju ssh … journalctl'). Disable any of
them with --debug-log=false / --k8s-log=false / --machine-log=false.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecord(cmd.Context(), args[0], *f)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "recording directory (default: ./recordings/<ts>--<name>)")
	cmd.Flags().StringVar(&f.attach, "attach", "", "attach mode: local|ssh (default: auto-detect)")
	cmd.Flags().StringVar(&f.controller, "controller", "", "scope to this juju controller (all its models); default: current controller")
	cmd.Flags().StringVar(&f.model, "model", "", "scope to a single model, as \"model\" or \"controller:model\"")
	cmd.Flags().StringVar(&f.sshTarget, "ssh-target", "", "machine to 'juju ssh' into for ssh attach (e.g. 'controller/0')")
	cmd.Flags().StringVar(&f.probePath, "probe-path", "", "path to the juju-lens-probe binary (default: next to juju-lens, then $PATH)")
	cmd.Flags().IntSliceVar(&f.pids, "pid", nil, "restrict the probe to these PIDs (default: all jujud/containeragent)")
	cmd.Flags().BoolVar(&f.debugLog, "debug-log", true, "also stream 'juju debug-log' for the scoped models")
	cmd.Flags().BoolVar(&f.k8sLog, "k8s-log", true, "also stream workload-container logs for CAAS models ('kubectl logs')")
	cmd.Flags().BoolVar(&f.machineLog, "machine-log", true, "also stream machine journald for IAAS models ('juju ssh … journalctl')")
	cmd.Flags().DurationVar(&f.maxDuration, "max-duration", 0, "stop recording after this duration (0 = no limit)")
	cmd.Flags().Int64Var(&f.maxSize, "max-size", 0, "stop recording after this many bytes of raw/ (0 = no limit)")
	return cmd
}

func runRecord(ctx context.Context, name string, f recordFlags) error {
	if f.output == "" {
		f.output = filepath.Join("recordings", recording.SuggestedDirName(name, time.Now()))
	}
	layout := recording.NewLayout(f.output)
	if err := layout.Init(); err != nil {
		return err
	}

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

	mode := f.attach
	if mode == "" {
		if f.sshTarget != "" {
			mode = "ssh"
		} else {
			mode = "local"
		}
	}

	// Resolve the controller/model scope to a probe filter and a UUID->name
	// resolver via the juju client. An explicit scope must resolve; with no
	// scope we try the current controller and fall back to unfiltered capture
	// if juju is unavailable.
	topo, filter, scopeErr := resolveScope(f.controller, f.model)
	if scopeErr != nil {
		if f.controller != "" || f.model != "" {
			return scopeErr
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
	man.AttachMode = mode
	if err := man.Save(layout.Root); err != nil {
		return fmt.Errorf("writing initial manifest: %w", err)
	}

	probeCmd, target, err := buildProbeCommand(mode, f, filter)
	if err != nil {
		return err
	}
	if target != "" {
		man.AttachTargets = []string{target}
	}
	man.AddSource(recording.SourceStatus{Name: "rpc", Kind: "rpc", Started: time.Now().UTC()})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Optional log sources, all sharing the recording's context so they stop with
	// everything else: `juju debug-log` per model, plus per-model-type ingesters
	// (CAAS workload logs, IAAS machine journald). Controller scope (no single
	// model pinned) keeps discovering models added during the recording.
	var logIng *logIngester
	var k8sIng *k8sIngester
	var machIng *machineIngester
	var logWG sync.WaitGroup
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
			man.AddSource(recording.SourceStatus{Name: "debug-log", Kind: "juju-debug-log", Started: time.Now().UTC()})
			logIng.start(ctx, &logWG, models)
		}
		caas, iaas := splitByModelType(topo, models)
		if f.k8sLog && (len(caas) > 0 || discover) {
			k8sIng = newK8sIngester(layout, topo.controllerName, discover, caas)
			man.AddSource(recording.SourceStatus{Name: "k8s-log", Kind: "k8s-logs", Started: time.Now().UTC()})
			k8sIng.start(ctx, &logWG)
		}
		if f.machineLog && (len(iaas) > 0 || discover) {
			machIng = newMachineIngester(layout, topo.controllerName, discover, iaas)
			man.AddSource(recording.SourceStatus{Name: "machine-log", Kind: "machine-journald", Started: time.Now().UTC()})
			machIng.start(ctx, &logWG)
		}
	}

	stdout, err := probeCmd.StdoutPipe()
	if err != nil {
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

	fmt.Fprintf(os.Stderr, "juju-lens: recording %q via %s attach (pid %d)\n", name, mode, os.Getpid())
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
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
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
		fmt.Fprintf(os.Stderr, "juju-lens: probe reports %d frames dropped so far (it's falling behind — see docs/profiling-and-architecture.md)\n", n)
	}
	ingestErr := probe.Ingest(ctx, stdout, resolver, w.sink, onDrops)
	cancel()
	if totalDrops > 0 {
		fmt.Fprintf(os.Stderr, "juju-lens: recording finished with %d frames dropped by the probe; some hooks may show a \"capture gap\"\n", totalDrops)
	}
	// The probe is a child (local) or an ssh session; either way, killing the
	// process group ends it and its remote peer.
	_ = probeCmd.Process.Kill()
	_ = probeCmd.Wait()

	// Stop the log ingesters (ctx is already cancelled, which signals them) and
	// wait for their goroutines to flush.
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

	if endReason == recording.EndReasonUnknown {
		if ingestErr != nil && !errors.Is(ingestErr, context.Canceled) {
			endReason = recording.EndReasonError
		} else {
			endReason = recording.EndReasonUserRequested
		}
	}
	man.FinishSource("rpc", nil)
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

// buildProbeCommand constructs the exec.Cmd that runs the probe for the chosen
// attach mode and returns a human-readable target for the manifest.
func buildProbeCommand(mode string, f recordFlags, filter probeFilter) (*exec.Cmd, string, error) {
	probeArgs := probeArgs(f, filter)
	switch mode {
	case "local":
		bin, err := findProbeBinary(f.probePath)
		if err != nil {
			return nil, "", err
		}
		return exec.Command(bin, probeArgs...), "localhost", nil
	case "ssh":
		target := f.sshTarget
		if target == "" {
			target = "controller/0" // sensible default machine controller
		}
		remoteProbe := f.probePath
		if remoteProbe == "" {
			remoteProbe = "juju-lens-probe" // assume it is on the remote PATH
		}
		args := []string{"ssh"}
		if f.controller != "" {
			args = append(args, "-m", f.controller+":controller")
		}
		args = append(args, target, "--", "sudo", remoteProbe)
		args = append(args, probeArgs...)
		return exec.Command("juju", args...), target, nil
	case "kubectl-debug", "daemonset":
		return nil, "", fmt.Errorf("attach mode %q (Kubernetes) lands in M6", mode)
	default:
		return nil, "", fmt.Errorf("unknown attach mode %q (want local|ssh)", mode)
	}
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

// findProbeBinary locates juju-lens-probe: an explicit --probe-path wins, then
// a sibling of the running juju-lens binary, then $PATH.
func findProbeBinary(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--probe-path %q: %w", explicit, err)
		}
		return explicit, nil
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "juju-lens-probe")
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	if p, err := exec.LookPath("juju-lens-probe"); err == nil {
		return p, nil
	}
	return "", errors.New("juju-lens-probe not found; build it (`go build ./cmd/juju-lens-probe`) and pass --probe-path")
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
