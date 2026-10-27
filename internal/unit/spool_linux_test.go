//go:build linux

package unit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// spoolRig is a spooler on a temporary spool, collecting what it emits.
type spoolRig struct {
	dir  string
	sp   *spooler
	ch   chan spoolEvents
	recs []Record
}

func newSpoolRig(t *testing.T, watch bool) *spoolRig {
	t.Helper()
	r := &spoolRig{dir: t.TempDir()}
	sp, err := newSpooler(r.dir, "/proc", true, func(rec Record) error {
		r.recs = append(r.recs, rec)
		return nil
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	r.sp = sp
	t.Cleanup(sp.close)
	if watch {
		r.startWatch()
	}
	return r
}

func (r *spoolRig) startWatch() {
	r.ch = make(chan spoolEvents, 16)
	go r.sp.watch(r.ch)
}

// pump handles events until cond holds, failing after a few seconds.
func (r *spoolRig) pump(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case ev := <-r.ch:
			if err := r.sp.handle(ev); err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; records: %v", what, r.kinds())
		}
	}
}

// settle handles whatever events arrive within a short while.
func (r *spoolRig) settle(t *testing.T) {
	t.Helper()
	for {
		select {
		case ev := <-r.ch:
			if err := r.sp.handle(ev); err != nil {
				t.Fatal(err)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

func (r *spoolRig) taps() []*Tap {
	var out []*Tap
	for _, rec := range r.recs {
		if rec.Tap != nil {
			out = append(out, rec.Tap)
		}
	}
	return out
}

func (r *spoolRig) ends() []*SpoolEnd {
	var out []*SpoolEnd
	for _, rec := range r.recs {
		if rec.SpoolEnd != nil {
			out = append(out, rec.SpoolEnd)
		}
	}
	return out
}

func (r *spoolRig) kinds() []string {
	var out []string
	for _, rec := range r.recs {
		out = append(out, rec.Kind)
	}
	return out
}

// spoolFileFor names a spool file the way the tap does, for a process.
func spoolFileFor(dir, unitName string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%d-%d.jsonl", strings.ReplaceAll(unitName, "/", "-"), pid, time.Now().UnixNano()))
}

// openSpool opens a spool file as the tap does: append-only, one write per
// record.
func openSpool(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustWrite(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// exitedPID is the pid of a process that has already exited.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestSpoolForwardsCompleteLinesOnly(t *testing.T) {
	r := newSpoolRig(t, true)
	f := openSpool(t, spoolFileFor(r.dir, "otelcol-k8s/0", os.Getpid()))
	defer f.Close()

	mustWrite(t, f, `{"k":"proc_start"}`+"\n"+`{"k":"em`)
	r.pump(t, "the first line", func() bool { return len(r.taps()) == 1 })
	r.settle(t)
	if n := len(r.taps()); n != 1 {
		t.Fatalf("forwarded %d lines before the second one was complete", n)
	}
	mustWrite(t, f, `it"}`+"\n"+"not json\n")
	r.pump(t, "the rest", func() bool { return len(r.taps()) == 3 })

	taps := r.taps()
	if got := string(taps[1].Record); got != `{"k":"emit"}` {
		t.Fatalf("second line forwarded as %q", got)
	}
	if taps[2].Record != nil || taps[2].Text != "not json" {
		t.Fatalf("a line that isn't JSON: got %+v, want it as text", taps[2])
	}
	host, _ := os.Hostname()
	if tp := taps[0]; tp.Unit != "otelcol-k8s/0" || tp.PID != os.Getpid() || tp.Host != host || !strings.HasSuffix(tp.File, ".jsonl") {
		t.Fatalf("tap record tagged %+v", tp)
	}
	// The whole forwarded record is valid JSON with the line embedded.
	b, err := json.Marshal(Record{Kind: KindTap, Tap: taps[1]})
	if err != nil || !strings.Contains(string(b), `"rec":{"k":"emit"}`) {
		t.Fatalf("encoded as %s, %v", b, err)
	}
	if len(r.ends()) != 0 {
		t.Fatalf("the process is alive, but its file was finished: %v", r.ends())
	}
}

// TestSpoolFinishesFiles: a file is finished, reported and deleted only once
// its process has exited, and is truncated when no proc_end went by.
func TestSpoolFinishesFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		lines         string
		wantTruncated bool
	}{
		"ended":  {lines: `{"k":"proc_start"}` + "\n" + `{"t":1,"m":2,"k":"proc_end","rc":0}` + "\n", wantTruncated: false},
		"killed": {lines: `{"k":"proc_start"}` + "\n" + `{"k":"emit"}` + "\n", wantTruncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSpoolRig(t, true)
			proc := exec.Command("sleep", "30")
			if err := proc.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proc.Process.Kill(); _ = proc.Wait() }()
			path := spoolFileFor(r.dir, "otelcol/0", proc.Process.Pid)
			f := openSpool(t, path)
			mustWrite(t, f, tc.lines)
			_ = f.Close() // the close event comes while the process still runs
			r.pump(t, "both lines", func() bool { return len(r.taps()) == 2 })
			r.settle(t)
			if err := r.sp.reap(); err != nil {
				t.Fatal(err)
			}
			if len(r.ends()) != 0 {
				t.Fatalf("finished while the process runs: %+v", r.ends()[0])
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("file gone while the process runs: %v", err)
			}

			_ = proc.Process.Kill()
			_ = proc.Wait()
			if err := r.sp.reap(); err != nil {
				t.Fatal(err)
			}
			ends := r.ends()
			if len(ends) != 1 || ends[0].Lines != 2 || ends[0].Truncated != tc.wantTruncated || ends[0].PID != proc.Process.Pid {
				t.Fatalf("spool_end = %+v, want 2 lines, truncated %v", ends, tc.wantTruncated)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("finished file still there: %v", err)
			}
			if err := r.sp.reap(); err != nil || len(r.ends()) != 1 {
				t.Fatalf("finished twice: %v, %v", r.ends(), err)
			}
		})
	}
}

// TestSpoolFinishesOnClose: when the process has already exited by the
// time its file is closed, the close event alone finishes it.
func TestSpoolFinishesOnClose(t *testing.T) {
	r := newSpoolRig(t, true)
	f := openSpool(t, spoolFileFor(r.dir, "otelcol/0", exitedPID(t)))
	mustWrite(t, f, `{"k":"proc_end"}`+"\n")
	_ = f.Close()
	r.pump(t, "spool_end", func() bool { return len(r.ends()) == 1 })
	if e := r.ends()[0]; e.Lines != 1 || e.Truncated {
		t.Fatalf("spool_end = %+v", e)
	}
}

// TestSpoolReusedPID: a running process that started after the file was
// created isn't the one that wrote it.
func TestSpoolReusedPID(t *testing.T) {
	r := newSpoolRig(t, false)
	name := fmt.Sprintf("otelcol-0-%d-%d.jsonl", os.Getpid(), time.Now().Add(-time.Hour).UnixNano())
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(`{"k":"emit"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.sp.scan(); err != nil {
		t.Fatal(err)
	}
	if e := r.ends(); len(e) != 1 || !e[0].Truncated {
		t.Fatalf("spool_end = %+v, want the file finished as truncated", e)
	}
}

// TestSpoolOverflowLosesNothing fills the kernel's inotify queue before
// reading it, then writes to a file whose events the full queue drops. The
// overflow triggers a rescan that finds those lines, and every line is
// forwarded exactly once, in order.
func TestSpoolOverflowLosesNothing(t *testing.T) {
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Skip(err)
	}
	maxQueued, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || maxQueued > 1<<20 {
		t.Skipf("max_queued_events = %q", b)
	}
	r := newSpoolRig(t, false) // the watch exists, nothing reads it yet
	noise := []*os.File{
		openSpool(t, spoolFileFor(r.dir, "noise/0", os.Getpid())),
		openSpool(t, spoolFileFor(r.dir, "noise/1", os.Getpid())),
	}
	// Alternating files keeps the kernel from merging the events.
	nNoise := maxQueued + 10
	for i := range nNoise {
		mustWrite(t, noise[i%2], fmt.Sprintf(`{"i":%d}`+"\n", i))
	}
	// The queue is full: this file's creation and writes leave no event.
	lost := openSpool(t, spoolFileFor(r.dir, "otelcol/0", os.Getpid()))
	const nLost = 100
	for i := range nLost {
		mustWrite(t, lost, fmt.Sprintf(`{"i":%d}`+"\n", i))
	}
	r.startWatch()
	r.pump(t, "every line", func() bool { return len(r.taps()) >= nNoise+nLost })
	r.settle(t)
	if r.sp.overflows == 0 {
		t.Fatalf("the queue never overflowed with %d events", nNoise)
	}
	if n := len(r.taps()); n != nNoise+nLost {
		t.Fatalf("forwarded %d lines, want %d", n, nNoise+nLost)
	}
	next := map[string]int{}
	for _, tp := range r.taps() {
		var v struct{ I int }
		if err := json.Unmarshal(tp.Record, &v); err != nil {
			t.Fatal(err)
		}
		want := next[tp.Unit]
		switch tp.Unit {
		case "noise/0":
			want *= 2
		case "noise/1":
			want = want*2 + 1
		}
		if v.I != want {
			t.Fatalf("%s: got line %d, want %d", tp.Unit, v.I, want)
		}
		next[tp.Unit]++
	}
	if next["otelcol/0"] != nLost {
		t.Fatalf("forwarded %v lines per file, want %d from otelcol/0", next, nLost)
	}

	// The watch goes on after the rescan.
	mustWrite(t, lost, `{"i":"after"}`+"\n")
	r.pump(t, "a line after the rescan", func() bool { return len(r.taps()) == nNoise+nLost+1 })
}

func TestParseSpoolName(t *testing.T) {
	for name, want := range map[string]struct {
		unit string
		pid  int
		ok   bool
	}{
		"otelcol-0-123-1792681487190929852.jsonl":                    {"otelcol/0", 123, true},
		"opentelemetry-collector-k8s-12-9-1792681487190929852.jsonl": {"opentelemetry-collector-k8s/12", 9, true},
		"unknown-77-1792681487190929852.jsonl":                       {"unknown", 77, true},
		"otelcol-0-x-1792681487190929852.jsonl":                      {ok: false},
		"otelcol-0-123-.jsonl":                                       {ok: false},
		"config.jsonl":                                               {ok: false},
	} {
		u, pid, start, ok := parseSpoolName(name)
		if ok != want.ok || ok && (u != want.unit || pid != want.pid || start.UnixNano() != 1792681487190929852) {
			t.Errorf("%s: got %q %d %v %v, want %+v", name, u, pid, start, ok, want)
		}
	}
}
