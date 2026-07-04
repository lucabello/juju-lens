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

// machineIngester follows the systemd journal of every machine in an IAAS model
// over `juju ssh … journalctl -f -o json` and writes the raw JSON lines under
// raw/machine/<model>/machine-<id>/. It is the M6 machine log source: host-level
// signal (systemd, snapd, workload services) that never reaches `juju
// debug-log`, which only carries the machine agent's own charm output.
//
// It reconciles on a ticker like the k8s ingester: new machines and, in
// controller scope, new IAAS models get streams as they appear, and a stream
// that ends (ssh dropped, machine rebooted) is relaunched next tick. `juju ssh`
// with a read-only `journalctl` never mutates the machine.
type machineIngester struct {
	layout     recording.Layout
	controller string
	discover   bool // controller scope: keep rediscovering IAAS models

	ctx context.Context
	wg  *sync.WaitGroup

	mu     sync.Mutex
	models map[string]bool           // IAAS model short-names in scope
	active map[string]*journalStream // by "model\x00machineID"
	n      int64                     // total lines written across all streams
}

type journalStream struct {
	key    string
	writer *recording.RotatingWriter
	cmd    *exec.Cmd
}

func newMachineIngester(layout recording.Layout, controller string, discover bool, models []string) *machineIngester {
	mi := &machineIngester{
		layout:     layout,
		controller: controller,
		discover:   discover,
		models:     map[string]bool{},
		active:     map[string]*journalStream{},
	}
	for _, m := range models {
		mi.models[m] = true
	}
	return mi
}

// start reconciles once, then keeps reconciling on a ticker until ctx is done.
func (mi *machineIngester) start(ctx context.Context, wg *sync.WaitGroup) {
	mi.ctx = ctx
	mi.wg = wg
	mi.reconcile()
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
				mi.reconcile()
			}
		}
	}()
}

// reconcile discovers IAAS models (controller scope) and, for each in-scope
// model, starts a journald stream for every machine not already followed.
func (mi *machineIngester) reconcile() {
	if mi.discover {
		if mj, err := runJujuModels(mi.controller); err == nil {
			mi.mu.Lock()
			for _, m := range mj.Models {
				if m.ModelType == "iaas" && m.ShortName != "" {
					mi.models[m.ShortName] = true
				}
			}
			mi.mu.Unlock()
		}
	}
	mi.mu.Lock()
	models := make([]string, 0, len(mi.models))
	for m := range mi.models {
		models = append(models, m)
	}
	mi.mu.Unlock()

	for _, model := range models {
		ids, err := runJujuMachines(mi.controller, model)
		if err != nil {
			continue // model gone / juju hiccup: retry next tick
		}
		for _, id := range ids {
			mi.ensureStream(model, id)
		}
	}
}

// ensureStream launches a journald tail for one machine unless one is already
// running for it.
func (mi *machineIngester) ensureStream(model, machineID string) {
	key := model + "\x00" + machineID
	mi.mu.Lock()
	if _, ok := mi.active[key]; ok {
		mi.mu.Unlock()
		return
	}
	st := &journalStream{key: key}
	mi.active[key] = st // reserve the slot before launching
	mi.mu.Unlock()

	if err := mi.launch(st, model, machineID); err != nil {
		mi.mu.Lock()
		delete(mi.active, key)
		mi.mu.Unlock()
		return
	}
	fmt.Fprintf(os.Stderr, "juju-lens: streaming journald for %s machine %s\n", model, machineID)
}

func (mi *machineIngester) launch(st *journalStream, model, machineID string) error {
	host := "machine-" + machineID
	st.writer = recording.NewRotatingWriter(mi.layout.MachineLogFileFor(model, host),
		func() time.Time { return time.Now() })
	// -f follows new entries (the debug-log --tail analogue); -o json emits one
	// JSON object per line that ParseJournalLine decodes. --pty=false keeps the
	// stream line-buffered rather than going through a terminal.
	name, full := jujuCommand([]string{
		"ssh", "-m", mi.controller + ":" + model, "--pty=false", machineID,
		"journalctl -f -o json --no-pager",
	})
	cmd := exec.CommandContext(mi.ctx, name, full...)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	st.cmd = cmd
	mi.wg.Add(1)
	go func() {
		defer mi.wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if _, err := st.writer.WriteLine(sc.Bytes()); err != nil {
				break
			}
			atomic.AddInt64(&mi.n, 1)
		}
		// The stream ended (ssh dropped, reboot). Drop it so the next reconcile
		// reconnects.
		mi.mu.Lock()
		if mi.active[st.key] == st {
			delete(mi.active, st.key)
		}
		mi.mu.Unlock()
		_ = st.writer.Close()
	}()
	return nil
}

// stop terminates every ssh subprocess and flushes writers. Callers cancel the
// context first (which signals the subprocesses); stop then reaps them.
func (mi *machineIngester) stop() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	for _, st := range mi.active {
		if st.cmd != nil && st.cmd.Process != nil {
			_ = st.cmd.Process.Kill()
			_ = st.cmd.Wait()
		}
		if st.writer != nil {
			_ = st.writer.Close()
		}
	}
}

func (mi *machineIngester) count() int64 { return atomic.LoadInt64(&mi.n) }
