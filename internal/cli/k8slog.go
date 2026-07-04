package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// k8sIngester follows the stdout of every workload container in a CAAS model and
// writes it, verbatim, under raw/k8s/<model>/<pod>/<container>/. It is the M6
// K8s log source: workload-process output that never reaches `juju debug-log`
// (which only carries the charm/container-agent). The charm container is skipped
// for exactly that reason — debug-log already covers it.
//
// A Juju CAAS model maps to a Kubernetes namespace of the same name and each
// unit to a pod "<app>-<n>", so the ingester tails `kubectl logs` per pod
// container and attributes each line back to a unit via the raw path. It
// reconciles on a ticker: new pods (units) and, in controller scope, new CAAS
// models get streams as they appear, and a stream that ends (pod restart, common
// in these clusters) is relaunched on the next tick. Like the probe and
// debug-log it only reads: `kubectl logs` never mutates the cluster.
type k8sIngester struct {
	layout     recording.Layout
	controller string
	discover   bool // controller scope: keep rediscovering CAAS models

	ctx context.Context
	wg  *sync.WaitGroup

	mu     sync.Mutex
	models map[string]bool       // namespace (== model short-name) in scope
	active map[string]*podStream // by "model\x00pod\x00container"
	n      int64                 // total lines written across all streams
}

type podStream struct {
	key    string
	writer *recording.RotatingWriter
	cmd    *exec.Cmd
}

func newK8sIngester(layout recording.Layout, controller string, discover bool, models []string) *k8sIngester {
	ki := &k8sIngester{
		layout:     layout,
		controller: controller,
		discover:   discover,
		models:     map[string]bool{},
		active:     map[string]*podStream{},
	}
	for _, m := range models {
		ki.models[m] = true
	}
	return ki
}

// start reconciles once, then keeps reconciling on a ticker until ctx is done.
func (ki *k8sIngester) start(ctx context.Context, wg *sync.WaitGroup) {
	ki.ctx = ctx
	ki.wg = wg
	ki.reconcile()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ki.reconcile()
			}
		}
	}()
}

// reconcile discovers CAAS models (controller scope) and, for every in-scope
// namespace, starts a stream for each workload container not already followed.
func (ki *k8sIngester) reconcile() {
	if ki.discover {
		if mj, err := runJujuModels(ki.controller); err == nil {
			ki.mu.Lock()
			for _, m := range mj.Models {
				if m.ModelType == "caas" && m.ShortName != "" {
					ki.models[m.ShortName] = true
				}
			}
			ki.mu.Unlock()
		}
	}
	ki.mu.Lock()
	namespaces := make([]string, 0, len(ki.models))
	for ns := range ki.models {
		namespaces = append(namespaces, ns)
	}
	ki.mu.Unlock()

	for _, ns := range namespaces {
		pods, err := k8sPodContainers(ki.ctx, ns)
		if err != nil {
			continue // cluster unreachable / namespace gone: try again next tick
		}
		for pod, containers := range pods {
			for _, c := range containers {
				ki.ensureStream(ns, pod, c)
			}
		}
	}
}

// ensureStream launches a `kubectl logs -f` tail for one pod container unless one
// is already running for it.
func (ki *k8sIngester) ensureStream(model, pod, container string) {
	key := model + "\x00" + pod + "\x00" + container
	ki.mu.Lock()
	if _, ok := ki.active[key]; ok {
		ki.mu.Unlock()
		return
	}
	st := &podStream{key: key}
	ki.active[key] = st // reserve the slot before launching
	ki.mu.Unlock()

	if err := ki.launch(st, model, pod, container); err != nil {
		ki.mu.Lock()
		delete(ki.active, key)
		ki.mu.Unlock()
		return
	}
	fmt.Fprintf(os.Stderr, "juju-lens: streaming k8s logs for %s pod %s/%s\n", model, pod, container)
}

func (ki *k8sIngester) launch(st *podStream, model, pod, container string) error {
	st.writer = recording.NewRotatingWriter(ki.layout.K8sLogFileFor(model, pod, container),
		func() time.Time { return time.Now() })
	// --tail=0 follows only new lines (the debug-log --tail analogue); --timestamps
	// prepends the RFC3339Nano time ParseK8sLogLine reads.
	name, full := kubectlCommand([]string{
		"logs", "-f", "--tail=0", "--timestamps", "-n", model, pod, "-c", container,
	})
	cmd := exec.CommandContext(ki.ctx, name, full...)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	st.cmd = cmd
	ki.wg.Add(1)
	go func() {
		defer ki.wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if _, err := st.writer.WriteLine(sc.Bytes()); err != nil {
				break
			}
			atomic.AddInt64(&ki.n, 1)
		}
		// The stream ended (pod restart is common here). Drop it so the next
		// reconcile relaunches against the fresh container.
		ki.mu.Lock()
		if ki.active[st.key] == st {
			delete(ki.active, st.key)
		}
		ki.mu.Unlock()
		_ = st.writer.Close()
	}()
	return nil
}

// stop terminates every kubectl subprocess and flushes writers. Callers cancel
// the context first (which signals the subprocesses); stop then reaps them.
func (ki *k8sIngester) stop() {
	ki.mu.Lock()
	defer ki.mu.Unlock()
	for _, st := range ki.active {
		if st.cmd != nil && st.cmd.Process != nil {
			_ = st.cmd.Process.Kill()
			_ = st.cmd.Wait()
		}
		if st.writer != nil {
			_ = st.writer.Close()
		}
	}
}

func (ki *k8sIngester) count() int64 { return atomic.LoadInt64(&ki.n) }

// k8sPodContainers lists the workload containers of every pod in a namespace,
// keyed by pod name. The charm sidecar is excluded: its output is the
// container-agent log, already captured by debug-log.
func k8sPodContainers(ctx context.Context, namespace string) (map[string][]string, error) {
	name, full := kubectlCommand([]string{"get", "pods", "-n", namespace, "-o", "json"})
	out, err := exec.CommandContext(ctx, name, full...).Output()
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	pods := make(map[string][]string, len(resp.Items))
	for _, it := range resp.Items {
		for _, c := range it.Spec.Containers {
			if c.Name == "charm" {
				continue
			}
			pods[it.Metadata.Name] = append(pods[it.Metadata.Name], c.Name)
		}
	}
	return pods, nil
}
