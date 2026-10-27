package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// detachedChildFlag is the hidden flag `record --detach` passes to the daemon
// it re-executes, so the daemon knows to report readiness and ignore SIGHUP.
const detachedChildFlag = "detached-child"

// readyFD is the descriptor the daemon inherits for its readiness pipe: the
// first entry of exec.Cmd.ExtraFiles is always fd 3.
const readyFD = 3

// readyLine is what the daemon writes on the readiness pipe once it is
// capturing. Anything else, including EOF, means it didn't get there.
const readyLine = "ready"

// statusLine is what the daemon writes on the readiness pipe on the host
// probe's first attach-status frame, before it has necessarily attached to
// anything. It lets the parent tell a probe that found no agents from one too
// old to send status frames at all, which would otherwise time out
// identically.
const statusLine = "status"

// partPrefix starts the line the daemon writes when one capture side is
// capturing ("part host", "part unit"), so a parent that times out can say
// which side didn't get there.
const partPrefix = "part "

// logTailLines bounds how much of recorder.log a failed --detach prints.
const logTailLines = 20

// readyPipe is the daemon's end of the readiness pipe. The parent never sees
// "ready" until every capture side is capturing: for host, once the probe
// has reported an attached agent, so a probe that attaches to nothing ends in
// a timeout, not a false success; for unit, once every unit in scope has a
// probe running.
type readyPipe struct {
	mu         sync.Mutex
	pipe       *os.File
	opened     bool
	hostStatus bool
	done       bool
	pending    map[string]bool
}

// newReadyPipe waits for the named sides. With none, the first partReady
// makes the daemon ready.
func newReadyPipe(sides ...string) *readyPipe {
	r := &readyPipe{pending: map[string]bool{}}
	for _, s := range sides {
		r.pending[s] = true
	}
	return r
}

// openLocked opens the pipe, once.
func (r *readyPipe) openLocked() {
	if !r.opened {
		r.opened = true
		r.pipe = os.NewFile(readyFD, "ready-pipe")
	}
}

// onAttached is the host probe's attach-status callback: the first call
// tells the parent the probe speaks the readiness protocol, the first with
// n>0 makes the host side ready.
func (r *readyPipe) onAttached(n int) {
	r.mu.Lock()
	r.openLocked()
	if !r.hostStatus && !r.done && r.pipe != nil {
		r.hostStatus = true
		_, _ = fmt.Fprintln(r.pipe, statusLine)
	}
	r.mu.Unlock()
	if n > 0 {
		r.partReady("host", fmt.Sprintf("probe attached to %d agent process(es)", n))
	}
}

// partReady marks a side as capturing. When no side is left, it tells the
// parent the recorder is ready and closes the pipe.
func (r *readyPipe) partReady(side, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.openLocked()
	if r.pipe == nil {
		return
	}
	if r.pending[side] {
		delete(r.pending, side)
		_, _ = fmt.Fprintln(r.pipe, partPrefix+side)
	}
	fmt.Fprintf(os.Stderr, "juju-lens: %s\n", why)
	if len(r.pending) > 0 {
		return
	}
	r.done = true
	fmt.Fprintln(os.Stderr, "juju-lens: recorder ready")
	_, _ = fmt.Fprintln(r.pipe, readyLine)
	_ = r.pipe.Close()
}

// runDetached starts the recorder as a session-leading daemon and waits on a
// pipe until it reports it is capturing, it exits, or detachTimeout passes.
//
// It re-executes this binary rather than forking, because a Go process can't
// safely fork without exec. The daemon writes its own recorder.pid exactly as a
// foreground recorder does, so `stop` and the --max-* caps need no changes.
func runDetached(cmd *cobra.Command, name string, f recordFlags) error {
	if f.output == "" {
		f.output = filepath.Join("recordings", recording.SuggestedDirName(name, time.Now()))
	}
	dir, err := filepath.Abs(f.output)
	if err != nil {
		return err
	}
	layout := recording.NewLayout(dir)
	// Check before touching recorder.log: appending to a live recorder's log
	// would interleave two recorders' output.
	if pid, err := recording.ReadPidFile(layout.PidFile()); err == nil && recording.PidAlive(pid) {
		return fmt.Errorf("another recorder (PID %d) is already writing to %s; stop it with `juju-lens stop %s`",
			pid, layout.Root, layout.Root)
	}
	if err := layout.Init(); err != nil {
		return err
	}
	logPath := layout.RecorderLog()
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer logFile.Close()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devNull.Close()
	readR, readW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readR.Close()

	exe, err := os.Executable()
	if err != nil {
		_ = readW.Close()
		return fmt.Errorf("locating juju-lens binary: %w", err)
	}
	child := exec.Command(exe, detachedArgs(cmd.Flags(), name, dir)...)
	child.Stdin = devNull
	child.Stdout = logFile
	child.Stderr = logFile
	child.ExtraFiles = []*os.File{readW}
	// New session: no controlling terminal, and the daemon leads its own
	// process group, so a timeout can kill it and the probe together.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		_ = readW.Close()
		return fmt.Errorf("starting detached recorder: %w", err)
	}
	// Only the daemon may hold the write end, so its exit reads as EOF here.
	_ = readW.Close()
	pid := child.Process.Pid

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	ready := make(chan bool, 1)
	var sawStatus atomic.Bool
	var mu sync.Mutex
	sidesReady := map[string]bool{}
	go func() {
		br := bufio.NewReader(readR)
		for {
			line, err := br.ReadString('\n')
			switch l := strings.TrimSpace(line); {
			case l == statusLine:
				sawStatus.Store(true)
				continue
			case strings.HasPrefix(l, partPrefix):
				mu.Lock()
				sidesReady[strings.TrimPrefix(l, partPrefix)] = true
				mu.Unlock()
				continue
			case l == readyLine:
				ready <- true
				return
			}
			if err != nil {
				ready <- false
				return
			}
		}
	}()

	fail := func(format string, args ...any) error {
		cleanupStalePid(layout, pid)
		return fmt.Errorf("%s\n%s", fmt.Sprintf(format, args...), describeLog(logPath))
	}

	select {
	case ok := <-ready:
		if !ok {
			err := <-exited
			return fail("detached recorder (pid %d) exited before it was capturing: %v", pid, exitDescription(err))
		}
	case <-time.After(f.detachTimeout):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
		sides, _ := parseAttach(f.attach)
		mu.Lock()
		hostMissing := sides.host && !sidesReady["host"]
		unitMissing := sides.unit && !sidesReady["unit"]
		mu.Unlock()
		var why []string
		switch {
		case hostMissing && !sawStatus.Load():
			why = append(why, "the probe never reported its attach status, so it may be too old to support --detach; upgrade juju-lens-probe on the target machines (plain `record` still works with it)")
		case hostMissing:
			why = append(why, "the probe attached to no agent process")
		}
		if unitMissing {
			why = append(why, "not every unit in scope has a running unit probe")
		}
		return fail("detached recorder (pid %d) not ready after --detach-timeout %s: %s; killed it", pid, f.detachTimeout, strings.Join(why, "; "))
	}

	if got, err := recording.ReadPidFile(layout.PidFile()); err != nil || got != pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
		return fail("detached recorder (pid %d) reported ready but %s doesn't name it", pid, layout.PidFile())
	}
	// The daemon outlives us; it is reparented to init (or the nearest
	// subreaper) and reaped there.
	_ = child.Process.Release()
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "dir=%s\n", dir)
	fmt.Fprintf(out, "pid=%d\n", pid)
	return nil
}

// detachedArgs rebuilds the record invocation for the daemon from the flags
// the user actually set, minus the --detach ones, with --output pinned to an
// absolute path so the parent and daemon agree on the directory.
func detachedArgs(flags *pflag.FlagSet, name, dir string) []string {
	args := []string{"record"}
	flags.Visit(func(fl *pflag.Flag) {
		switch fl.Name {
		case "detach", "detach-timeout", "output", detachedChildFlag:
			return
		}
		if sv, ok := fl.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				args = append(args, "--"+fl.Name+"="+v)
			}
			return
		}
		args = append(args, "--"+fl.Name+"="+fl.Value.String())
	})
	return append(args, "--output="+dir, "--"+detachedChildFlag, "--", name)
}

// cleanupStalePid removes recorder.pid if it still names the dead daemon. A
// clean exit removes it already; a SIGKILL can't.
func cleanupStalePid(layout recording.Layout, pid int) {
	if got, err := recording.ReadPidFile(layout.PidFile()); err == nil && got == pid {
		_ = recording.RemovePidFile(layout.PidFile())
	}
}

func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// describeLog points at the daemon's log and quotes its tail, which is where
// the actual cause (an ssh or permission error) lands.
func describeLog(path string) string {
	tail, err := tailLines(path, logTailLines)
	if err != nil || len(tail) == 0 {
		return fmt.Sprintf("see %s", path)
	}
	return fmt.Sprintf("last lines of %s:\n%s", path, strings.Join(tail, "\n"))
}

func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(bytes.TrimRight(data, "\n")), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, errors.New("empty log")
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
