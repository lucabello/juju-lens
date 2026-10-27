//go:build linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/probe"
	"github.com/lucabello/juju-lens/internal/recording"
	"golang.org/x/sys/unix"
)

// The detach tests run the real daemon: `record --detach` re-executes
// os.Executable(), which under `go test` is this test binary. TestMain routes
// such re-executions back into the CLI, and the probe they run, `juju-lens
// probe`, into a fake probe, based on testModeEnv and fakeProbeEnv, so no
// eBPF, root or Juju is needed.
const (
	testModeEnv  = "JUJU_LENS_TEST_MODE"
	fakeProbeEnv = "JUJU_LENS_FAKE_PROBE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(testModeEnv) {
	case "cli":
		if len(os.Args) > 1 && os.Args[1] == "probe" && os.Getenv(fakeProbeEnv) != "" {
			runFakeProbe()
			os.Exit(0)
		}
		os.Args = append([]string{"juju-lens"}, os.Args[1:]...)
		if err := Execute(BuildInfo{Version: "test"}); err != nil {
			fmt.Fprintln(os.Stderr, "juju-lens:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeProbe stands in for juju-lens-probe. "attach" reports one attached
// agent and then streams an RPC frame every 50ms until killed; "fail" exits
// like a probe that can't attach; "none" runs and reports attaching to
// nothing; "old" runs but, like a probe predating attach-status frames,
// never reports anything.
func runFakeProbe() {
	switch os.Getenv(fakeProbeEnv) {
	case "attach":
		_ = probe.WriteFrame(os.Stdout, probe.Frame{TsUnixNano: time.Now().UnixNano(), Attached: 1})
		fmt.Fprintln(os.Stderr, "juju-lens-probe: attached 4 uprobes to pid 4242 (fake)")
		for {
			time.Sleep(50 * time.Millisecond)
			_ = probe.WriteFrame(os.Stdout, probe.Frame{TsUnixNano: time.Now().UnixNano(), PID: 4242, Dir: "write", Data: []byte("x")})
		}
	case "rpc":
		emit := func(dir string, data []byte) {
			_ = probe.WriteFrame(os.Stdout, probe.Frame{TsUnixNano: time.Now().UnixNano(), PID: 4242, Conn: 1, Unit: "grafana/0", Dir: dir, Data: data})
		}
		_ = probe.WriteFrame(os.Stdout, probe.Frame{TsUnixNano: time.Now().UnixNano(), Attached: 1})
		emit("write", []byte("GET /model/12345678-1234-1234-1234-1234567890ab/api HTTP/1.1\r\n\r\n"))
		emit("read", []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		for id := 1; ; id++ {
			time.Sleep(20 * time.Millisecond)
			emit("write", wsText(fmt.Sprintf(`{"request-id":%d,"type":"Uniter","request":"SetStatus","params":{}}`, id), true))
			emit("read", wsText(fmt.Sprintf(`{"request-id":%d,"response":{}}`, id), false))
		}
	case "fail":
		fmt.Fprintln(os.Stderr, "juju-lens-probe: loading eBPF collection: operation not permitted")
		os.Exit(1)
	case "none":
		_ = probe.WriteFrame(os.Stdout, probe.Frame{TsUnixNano: time.Now().UnixNano()})
		fallthrough
	default:
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
	}
}

// wsText builds a single short websocket text frame, masked as a client's.
func wsText(payload string, mask bool) []byte {
	b := []byte{0x81}
	if !mask {
		return append(append(b, byte(len(payload))), payload...)
	}
	key := []byte{1, 2, 3, 4}
	b = append(append(b, 0x80|byte(len(payload))), key...)
	for i := range len(payload) {
		b = append(b, payload[i]^key[i&3])
	}
	return b
}

// detachEnv makes the probe a recorder runs a fake of the given mode, and puts
// a failing `juju` on PATH (so scope resolution falls back to unfiltered
// capture without touching a real controller).
func detachEnv(t *testing.T, probeMode string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "juju"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv(testModeEnv, "cli")
	t.Setenv(fakeProbeEnv, probeMode)
}

// runCLI runs a juju-lens command in-process and returns its stdout.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// parseDetachOutput reads the documented `dir=` / `pid=` lines.
func parseDetachOutput(t *testing.T, out string) (string, int) {
	t.Helper()
	var dir string
	var pid int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "dir":
			dir = v
		case "pid":
			pid, _ = strconv.Atoi(v)
		}
	}
	if dir == "" || pid == 0 {
		t.Fatalf("unparseable --detach output: %q", out)
	}
	return dir, pid
}

// waitGone waits for a non-child process to exit.
func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for recording.PidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive after %s", pid, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startDetached runs a successful `record --detach` and registers cleanup.
func startDetached(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	detachEnv(t, "attach")
	dir := filepath.Join(t.TempDir(), "rec")
	args := append([]string{"record", "--detach", "--attach=host", "-o", dir}, extra...)
	out, err := runCLI(t, append(args, "ci-run")...)
	if err != nil {
		t.Fatalf("record --detach: %v", err)
	}
	gotDir, pid := parseDetachOutput(t, out)
	t.Cleanup(func() {
		if recording.PidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			waitGone(t, pid, 5*time.Second)
		}
	})
	return gotDir, pid
}

// TestDetachSuccess covers AC1, AC2, AC5, AC6 and AC7 (stop).
func TestDetachSuccess(t *testing.T) {
	dir, pid := startDetached(t)
	layout := recording.NewLayout(dir)

	if !filepath.IsAbs(dir) {
		t.Fatalf("dir %q should be absolute", dir)
	}
	if !recording.PidAlive(pid) {
		t.Fatalf("daemon %d not alive after --detach returned", pid)
	}
	if got, err := recording.ReadPidFile(layout.PidFile()); err != nil || got != pid {
		t.Fatalf("recorder.pid = %d, %v; want %d", got, err, pid)
	}
	if sid, _ := unix.Getsid(pid); sid != pid {
		t.Fatalf("daemon session id = %d, want it to lead its own session (%d)", sid, pid)
	}

	// AC5: a hangup, as when a CI step's shell goes away, must not stop it.
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !recording.PidAlive(pid) {
		t.Fatal("daemon died on SIGHUP")
	}

	// AC6: daemon output, including relayed probe output, is in recorder.log.
	logData, err := os.ReadFile(layout.RecorderLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"probe: juju-lens-probe: attached", "recorder ready"} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("recorder.log missing %q:\n%s", want, logData)
		}
	}

	// AC7: stop finalises the recording like a foreground one.
	if err := runStop(dir, stopFlags{timeout: 10 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(layout.PidFile()); !os.IsNotExist(err) {
		t.Fatalf("recorder.pid should be gone after stop, stat err = %v", err)
	}
	man, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if man.EndReason != recording.EndReasonSignal || man.Ended.IsZero() {
		t.Fatalf("manifest not finalised: end_reason=%q ended=%v", man.EndReason, man.Ended)
	}
	if _, err := os.Stat(layout.IndexDB()); err != nil {
		t.Fatalf("index.db not built: %v", err)
	}
}

// TestDetachMaxDuration covers AC7 for the caps.
func TestDetachMaxDuration(t *testing.T) {
	dir, pid := startDetached(t, "--max-duration=1s")
	waitGone(t, pid, 15*time.Second)
	man, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if man.EndReason != recording.EndReasonMaxDuration {
		t.Fatalf("end_reason = %q, want %q", man.EndReason, recording.EndReasonMaxDuration)
	}
	if _, err := os.Stat(recording.NewLayout(dir).PidFile()); !os.IsNotExist(err) {
		t.Fatalf("recorder.pid left behind: %v", err)
	}
}

// TestDetachMaxSize covers the --max-size cap on a detached recorder.
func TestDetachMaxSize(t *testing.T) {
	detachEnv(t, "rpc")
	dir := filepath.Join(t.TempDir(), "rec")
	out, err := runCLI(t, "record", "--detach", "--attach=host", "-o", dir, "--max-size=2000", "ci-run")
	if err != nil {
		t.Fatalf("record --detach: %v", err)
	}
	dir, pid := parseDetachOutput(t, out)
	t.Cleanup(func() {
		if recording.PidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	waitGone(t, pid, 20*time.Second)
	man, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if man.EndReason != recording.EndReasonMaxSize {
		t.Fatalf("end_reason = %q, want %q", man.EndReason, recording.EndReasonMaxSize)
	}
}

// assertNothingLeft checks a failed --detach left no recorder or pid file.
func assertNothingLeft(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(recording.NewLayout(dir).PidFile()); !os.IsNotExist(err) {
		pid, _ := recording.ReadPidFile(recording.NewLayout(dir).PidFile())
		t.Fatalf("recorder.pid left behind (pid %d, alive=%v)", pid, recording.PidAlive(pid))
	}
}

// TestDetachAttachFailure covers AC3: a probe that can't attach.
func TestDetachAttachFailure(t *testing.T) {
	detachEnv(t, "fail")
	dir := filepath.Join(t.TempDir(), "rec")
	_, err := runCLI(t, "record", "--detach", "--attach=host", "-o", dir, "ci-run")
	if err == nil {
		t.Fatal("want an error when the probe fails to attach")
	}
	for _, want := range []string{"exited before it was capturing", "operation not permitted", "recorder.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
	assertNothingLeft(t, dir)
}

// TestDetachTimeout covers AC4 and the zero-targets rule: a probe that runs
// but attaches to nothing is a failure, and the daemon is killed.
func TestDetachTimeout(t *testing.T) {
	detachEnv(t, "none")
	dir := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	_, err := runCLI(t, "record", "--detach", "--detach-timeout=1s", "--attach=host", "-o", dir, "ci-run")
	if err == nil || !strings.Contains(err.Error(), "--detach-timeout 1s") || !strings.Contains(err.Error(), "attached to no agent process") {
		t.Fatalf("want a no-targets timeout error, got %v", err)
	}
	if strings.Contains(err.Error(), "too old") {
		t.Fatalf("a probe that reported status must not be called too old: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	assertNothingLeft(t, dir)
}

// TestDetachTimeoutOldProbe covers AC10: a probe that never sends an
// attach-status frame gets an upgrade hint, not the no-targets message.
func TestDetachTimeoutOldProbe(t *testing.T) {
	detachEnv(t, "old")
	dir := filepath.Join(t.TempDir(), "rec")
	_, err := runCLI(t, "record", "--detach", "--detach-timeout=1s", "--attach=host", "-o", dir, "ci-run")
	if err == nil {
		t.Fatal("want a timeout error")
	}
	for _, want := range []string{"--detach-timeout 1s", "too old", "upgrade juju-lens-probe"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "attached to no agent process") {
		t.Fatalf("old-probe error reused the no-targets message: %v", err)
	}
	assertNothingLeft(t, dir)
}

// TestDetachRefusesLiveRecorder covers AC8: the dir is already owned.
func TestDetachRefusesLiveRecorder(t *testing.T) {
	dir, pid := startDetached(t)
	detachEnv(t, "attach")
	_, err := runCLI(t, "record", "--detach", "--attach=host", "-o", dir, "again")
	if err == nil || !strings.Contains(err.Error(), "already writing") {
		t.Fatalf("want refusal, got %v", err)
	}
	if got, _ := recording.ReadPidFile(recording.NewLayout(dir).PidFile()); got != pid {
		t.Fatalf("recorder.pid changed to %d, want %d", got, pid)
	}
}

// TestDetachTimeoutRequiresDetach covers AC8's flag guard.
func TestDetachTimeoutRequiresDetach(t *testing.T) {
	_, err := runCLI(t, "record", "--detach-timeout=5s", "-o", t.TempDir(), "x")
	if err == nil || !strings.Contains(err.Error(), "--detach-timeout requires --detach") {
		t.Fatalf("want flag error, got %v", err)
	}
}

// TestDetachedArgs checks the daemon gets the user's flags, minus the
// detach ones, with the output pinned.
func TestDetachedArgs(t *testing.T) {
	cmd := newRecordCmd()
	if err := cmd.ParseFlags([]string{"--detach", "--detach-timeout=5s", "--pid=1,2", "--debug-log=false", "-o", "rel", "--max-size=10"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(detachedArgs(cmd.Flags(), "-name", "/abs/rec"), " ")
	for _, want := range []string{"--pid=1", "--pid=2", "--debug-log=false", "--max-size=10", "--output=/abs/rec", "--detached-child", "-- -name"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	for _, bad := range []string{"--detach ", "--detach=", "--detach-timeout", "rel"} {
		if strings.Contains(got+" ", bad) {
			t.Fatalf("args %q should not contain %q", got, bad)
		}
	}
}

// TestDetachAttachNone checks that --attach none, which starts no probe, is
// ready as soon as the recorder runs and records no RPC source.
func TestDetachAttachNone(t *testing.T) {
	detachEnv(t, "attach")
	bin := strings.SplitN(os.Getenv("PATH"), ":", 2)[0] // detachEnv's fake bin dir
	fakeJuju := `#!/bin/sh
case "$1" in
controllers) echo '{"current-controller":"lab","controllers":{"lab":{"uuid":"c-uuid"}}}' ;;
models) echo '{"models":[]}' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "juju"), []byte(fakeJuju), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "rec")
	out, err := runCLI(t, "record", "--detach", "--detach-timeout=10s", "--attach=none", "-o", dir, "logs-only")
	if err != nil {
		t.Fatalf("record --detach --attach=none: %v", err)
	}
	_, pid := parseDetachOutput(t, out)
	t.Cleanup(func() {
		if recording.PidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			waitGone(t, pid, 5*time.Second)
		}
	})
	if err := runStop(dir, stopFlags{timeout: 10 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	man, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if man.AttachMode != "none" || len(man.AttachTargets) != 0 {
		t.Fatalf("attach_mode=%q targets=%v, want none and no targets", man.AttachMode, man.AttachTargets)
	}
	for _, s := range man.Sources {
		if s.Name == "rpc" {
			t.Fatalf("--attach none recorded an rpc source: %+v", man.Sources)
		}
	}
	if man.EndReason != recording.EndReasonSignal {
		t.Fatalf("end_reason = %q, want %q", man.EndReason, recording.EndReasonSignal)
	}
}

// TestAttachWithoutHostNeedsJuju checks that --attach none and unit fail when
// the juju client can't be queried, instead of recording nothing at all.
func TestAttachWithoutHostNeedsJuju(t *testing.T) {
	detachEnv(t, "attach")
	for _, attach := range []string{"none", "unit"} {
		_, err := runCLI(t, "record", "--attach="+attach, "-o", filepath.Join(t.TempDir(), "rec"), "x")
		if err == nil || !strings.Contains(err.Error(), "juju client") {
			t.Fatalf("--attach %s: want a juju client error, got %v", attach, err)
		}
	}
}
