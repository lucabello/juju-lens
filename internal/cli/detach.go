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

// readyLine is what the daemon writes on the readiness pipe once the probe has
// attached. Anything else, including EOF, means it didn't get there.
const readyLine = "ready"

// logTailLines bounds how much of recorder.log a failed --detach prints.
const logTailLines = 20

// readySignaller returns the daemon's onAttached callback: on the first call
// it tells the waiting parent the recorder is capturing, then closes the pipe.
// The parent never sees "ready" unless the probe has reported an attached
// agent, so a probe that attaches to nothing ends in a timeout, not a false
// success.
func readySignaller() func(int) {
	var once sync.Once
	return func(n int) {
		once.Do(func() {
			pipe := os.NewFile(readyFD, "ready-pipe")
			if pipe == nil {
				return
			}
			fmt.Fprintf(os.Stderr, "juju-lens: probe attached to %d agent process(es); recorder ready\n", n)
			_, _ = fmt.Fprintln(pipe, readyLine)
			_ = pipe.Close()
		})
	}
}

// runDetached starts the recorder as a session-leading daemon and waits on a
// pipe until it reports the probe attached, it exits, or detachTimeout passes.
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
	go func() {
		line, _ := bufio.NewReader(readR).ReadString('\n')
		ready <- strings.TrimSpace(line) == readyLine
	}()

	fail := func(format string, args ...any) error {
		cleanupStalePid(layout, pid)
		return fmt.Errorf("%s\n%s", fmt.Sprintf(format, args...), describeLog(logPath))
	}

	select {
	case ok := <-ready:
		if !ok {
			err := <-exited
			return fail("detached recorder (pid %d) exited before the probe attached: %v", pid, exitDescription(err))
		}
	case <-time.After(f.detachTimeout):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
		return fail("detached recorder (pid %d) not ready after --detach-timeout %s: the probe attached to no agent process; killed it", pid, f.detachTimeout)
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
// the actual cause (a bad --probe-path, an ssh or permission error) lands.
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
