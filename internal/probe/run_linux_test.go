//go:build linux

package probe

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunSinkOrdersAndWritesFrames checks the basic contract: frames sent on
// the channel are written to cfg.Out, in order, and runSink returns cleanly
// once the channel is closed and drained (M13).
func TestRunSinkOrdersAndWritesFrames(t *testing.T) {
	var out bytes.Buffer
	frames := make(chan Frame, 4)
	var drops atomic.Uint64

	frames <- Frame{PID: 1, Data: []byte("a")}
	frames <- Frame{PID: 2, Data: []byte("b")}
	close(frames)

	if err := runSink(AttachConfig{Out: &out}, frames, &drops, nil, nil, time.Hour); err != nil {
		t.Fatalf("runSink: %v", err)
	}

	got := readAllFrames(t, out.Bytes())
	if len(got) != 2 || got[0].PID != 1 || got[1].PID != 2 {
		t.Fatalf("frames out of order or missing: %+v", got)
	}
}

// TestRunSinkUsesOnFrameInstead checks that OnFrame, when set, is used
// instead of writing to Out — mirroring the pre-M13 contract Run() had.
func TestRunSinkUsesOnFrameInstead(t *testing.T) {
	var got []Frame
	frames := make(chan Frame, 2)
	var drops atomic.Uint64

	frames <- Frame{PID: 7}
	close(frames)

	cfg := AttachConfig{OnFrame: func(f Frame) error { got = append(got, f); return nil }}
	if err := runSink(cfg, frames, &drops, nil, nil, time.Hour); err != nil {
		t.Fatalf("runSink: %v", err)
	}
	if len(got) != 1 || got[0].PID != 7 {
		t.Fatalf("OnFrame not used: %+v", got)
	}
}

// TestRunSinkPropagatesWriteError checks that a failing sink stops runSink
// and surfaces the error, rather than silently dropping the rest of the
// stream.
func TestRunSinkPropagatesWriteError(t *testing.T) {
	frames := make(chan Frame, 2)
	var drops atomic.Uint64
	frames <- Frame{PID: 1}

	boom := errors.New("boom")
	cfg := AttachConfig{OnFrame: func(Frame) error { return boom }}
	if err := runSink(cfg, frames, &drops, nil, nil, time.Hour); !errors.Is(err, boom) {
		t.Fatalf("runSink error = %v, want wrapping %v", err, boom)
	}
}

// TestRunSinkReportsQueueDrops checks that a nonzero queueDrops count gets
// reported to the sink as a stats Frame (Frame.Drops) once the report
// interval elapses — this is the userspace-side drop-visibility mechanism
// M13 added (the channel-full path in Run() only increments the counter;
// runSink is what turns it into something a consumer can see). The reported
// value is cumulative (Ingest's onDrops contract expects a running total, not
// a per-interval delta), so the counter must NOT reset once reported, and a
// second tick with no further drops must not re-report the same value.
func TestRunSinkReportsQueueDrops(t *testing.T) {
	gotCh := make(chan Frame, 4)
	frames := make(chan Frame)
	var drops atomic.Uint64
	drops.Store(5)

	done := make(chan error, 1)
	cfg := AttachConfig{OnFrame: func(f Frame) error { gotCh <- f; return nil }}
	go func() { done <- runSink(cfg, frames, &drops, nil, nil, 20*time.Millisecond) }()

	var got Frame
	select {
	case got = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drop-stats frame")
	}
	if got.Drops != 5 || len(got.Data) != 0 {
		t.Fatalf("want one stats frame with Drops=5, got %+v", got)
	}
	if drops.Load() != 5 {
		t.Fatalf("queueDrops must stay cumulative, got %d", drops.Load())
	}

	// No further drops: a later tick must not re-report the same total.
	select {
	case dup := <-gotCh:
		t.Fatalf("unexpected second stats frame with no new drops: %+v", dup)
	case <-time.After(100 * time.Millisecond):
	}

	close(frames)
	if err := <-done; err != nil {
		t.Fatalf("runSink: %v", err)
	}
}

// TestRunSinkReportsAttachStatus checks that an attached-count change reaches
// the stream as a stats frame, which is what `record --detach` waits for, and
// that an unchanged count isn't re-sent.
func TestRunSinkReportsAttachStatus(t *testing.T) {
	gotCh := make(chan Frame, 4)
	pr, pw := io.Pipe()
	frames := make(chan Frame)
	var drops atomic.Uint64
	status := newAttachStatus()

	go func() {
		for {
			f, err := ReadFrame(pr)
			if err != nil {
				close(gotCh)
				return
			}
			gotCh <- f
		}
	}()
	done := make(chan error, 1)
	go func() { done <- runSink(AttachConfig{Out: pw}, frames, &drops, status, nil, time.Hour) }()

	// A first scan that finds nothing is still reported: it is how the
	// recorder knows this probe supports readiness (AC10).
	status.set(0)
	select {
	case got := <-gotCh:
		if got.Attached != 0 || got.Drops != 0 || len(got.Data) != 0 {
			t.Fatalf("want status frame with Attached=0, got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for zero attach-status frame")
	}

	status.set(2)
	select {
	case got := <-gotCh:
		if got.Attached != 2 || len(got.Data) != 0 {
			t.Fatalf("want status frame with Attached=2, got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for attach-status frame")
	}

	status.set(2)
	select {
	case dup := <-gotCh:
		t.Fatalf("unexpected frame for unchanged count: %+v", dup)
	case <-time.After(100 * time.Millisecond):
	}

	close(frames)
	if err := <-done; err != nil {
		t.Fatalf("runSink: %v", err)
	}
	_ = pw.Close()
}

// TestRunSinkSkipsAttachStatusForOnFrame checks the human-readable modes
// never see attach-status frames.
func TestRunSinkSkipsAttachStatusForOnFrame(t *testing.T) {
	var got []Frame
	frames := make(chan Frame)
	var drops atomic.Uint64
	status := newAttachStatus()
	status.set(1)

	done := make(chan error, 1)
	cfg := AttachConfig{OnFrame: func(f Frame) error { got = append(got, f); return nil }}
	go func() { done <- runSink(cfg, frames, &drops, status, nil, time.Hour) }()
	time.Sleep(50 * time.Millisecond)
	close(frames)
	if err := <-done; err != nil {
		t.Fatalf("runSink: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("OnFrame got status frames: %+v", got)
	}
}

// readAllFrames decodes every length-prefixed Frame in b.
func readAllFrames(t *testing.T, b []byte) []Frame {
	t.Helper()
	r := bytes.NewReader(b)
	var out []Frame
	for {
		f, err := ReadFrame(r)
		if err != nil {
			return out
		}
		out = append(out, f)
	}
}
