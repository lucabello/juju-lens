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

Kubernetes attach (kubectl-debug, daemonset) lands in M6.`,
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

	ingestErr := probe.Ingest(ctx, stdout, resolver, w.sink, nil)
	cancel()
	// The probe is a child (local) or an ssh session; either way, killing the
	// process group ends it and its remote peer.
	_ = probeCmd.Process.Kill()
	_ = probeCmd.Wait()

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
	if err := runIndex(layout.Root); err != nil {
		fmt.Fprintf(os.Stderr, "juju-lens: indexing recording failed: %v (run `juju-lens index %s` to retry)\n",
			err, layout.Root)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: recording saved to %s (%d RPC messages captured)\n",
		layout.Root, w.count())
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
