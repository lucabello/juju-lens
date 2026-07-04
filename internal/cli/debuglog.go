package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// logIngester streams `juju debug-log` for each scoped model and writes the raw
// text lines, verbatim, into raw/juju/<model>/debug-log-<hour>.log. It is the
// M3 debug-log source; the index parses these raw files into log_records.
//
// Capture is strictly per-model: debug-log is model-scoped, so a controller
// recording runs one subprocess per model into its own raw/juju/<model>/
// directory. In controller scope the ingester also rediscovers models on a
// ticker, so models created after recording began get their own stream too —
// mirroring the probe's dynamic PID watching. The recorder never mutates
// anything: debug-log is a read-only tail.
type logIngester struct {
	layout     recording.Layout
	controller string
	discover   bool // controller scope: keep rediscovering models

	ctx context.Context
	wg  *sync.WaitGroup

	mu     sync.Mutex
	active map[string]*logStream // by model short-name
	n      int64                 // total lines written across all streams
}

type logStream struct {
	model  string
	writer *recording.RotatingWriter
	cmd    *exec.Cmd
	lastTs time.Time // rotation clock for this stream (set before each write)
}

func newLogIngester(layout recording.Layout, controller string, discover bool) *logIngester {
	return &logIngester{
		layout:     layout,
		controller: controller,
		discover:   discover,
		active:     map[string]*logStream{},
	}
}

// start launches a debug-log tail for each initial model and, in controller
// scope, a watcher that starts streams for models that appear later. It returns
// after spawning; streams run until ctx is cancelled or stop is called.
func (li *logIngester) start(ctx context.Context, wg *sync.WaitGroup, initial []string) {
	li.ctx = ctx
	li.wg = wg
	li.ensureStreams(initial)
	if !li.discover {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mj, err := runJujuModels(li.controller)
				if err != nil {
					continue
				}
				names := make([]string, 0, len(mj.Models))
				for _, m := range mj.Models {
					names = append(names, m.ShortName)
				}
				li.ensureStreams(names)
			}
		}
	}()
}

// ensureStreams starts a debug-log stream for every model not already being
// tailed. Safe to call from both the initial start and the discovery ticker.
func (li *logIngester) ensureStreams(models []string) {
	for _, model := range models {
		if model == "" {
			continue
		}
		li.mu.Lock()
		if _, ok := li.active[model]; ok {
			li.mu.Unlock()
			continue
		}
		st := &logStream{model: model}
		li.active[model] = st // reserve the slot before launching
		li.mu.Unlock()

		if err := li.launch(st); err != nil {
			li.mu.Lock()
			delete(li.active, model)
			li.mu.Unlock()
			fmt.Fprintf(os.Stderr, "juju-lens: debug-log %s: %v\n", model, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "juju-lens: streaming juju debug-log for model %s\n", model)
	}
}

// launch starts one model's debug-log subprocess and its line-consumer.
func (li *logIngester) launch(st *logStream) error {
	st.writer = recording.NewRotatingWriter(li.layout.JujuLogFileFor(st.model), func() time.Time {
		if st.lastTs.IsZero() {
			return time.Now()
		}
		return st.lastTs
	})
	// --tail follows new lines from now; --date --ms --utc give the
	// deterministic timestamp ParseDebugLogLine expects.
	args := []string{"debug-log", "-m", li.controller + ":" + st.model, "--tail", "--date", "--ms", "--utc"}
	name, full := jujuCommand(args)
	cmd := exec.CommandContext(li.ctx, name, full...)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	st.cmd = cmd
	li.wg.Add(1)
	go func() {
		defer li.wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if rec, ok := recording.ParseDebugLogLine(line, st.model); ok {
				st.lastTs = rec.Ts
			}
			if _, err := st.writer.WriteLine([]byte(line)); err != nil {
				return
			}
			atomic.AddInt64(&li.n, 1)
		}
	}()
	return nil
}

// stop terminates every debug-log subprocess and flushes the writers. Callers
// should cancel the context first (which signals the subprocesses); stop then
// reaps them and closes files.
func (li *logIngester) stop() {
	li.mu.Lock()
	defer li.mu.Unlock()
	for _, st := range li.active {
		if st.cmd != nil && st.cmd.Process != nil {
			_ = st.cmd.Process.Kill()
			_ = st.cmd.Wait()
		}
		if st.writer != nil {
			_ = st.writer.Close()
		}
	}
}

func (li *logIngester) count() int64 { return atomic.LoadInt64(&li.n) }
