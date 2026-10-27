package probecmd

import (
	"errors"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// A broken pipe on a descriptor other than stdout, such as the Pebble socket
// of a workload container that restarted, must not stop the unit probe.
func TestUnitContextSurvivesBrokenPipe(t *testing.T) {
	ctx, stop := unitContext()
	defer stop()
	defer signal.Reset(syscall.SIGPIPE)

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	if err := syscall.Close(fds[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.Write(fds[0], []byte("x")); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("write to a closed peer: %v, want EPIPE", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("a broken pipe on a socket stopped the unit probe")
	case <-time.After(200 * time.Millisecond):
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not stop the unit probe")
	}
}
