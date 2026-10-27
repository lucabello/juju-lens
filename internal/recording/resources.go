package recording

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// Span attributes the probe's resource measurements give an ops root span.
// The figures themselves are the res_* attributes resAttrs writes.
const (
	// AttrResSource is unit.SourceTap or unit.SourceProc: what found the
	// execution and gave its boundaries.
	AttrResSource = "res_source"
	// AttrResOverlapped is "true" when another execution ran in the same
	// cgroup at the same time, so the cgroup figures cover both.
	AttrResOverlapped = "res_overlapped"
	// AttrResLingerMs is how long after the execution, or after the last
	// Pebble change it caused was ready, the probe kept sampling workloads.
	AttrResLingerMs = "res_linger_ms"
)

// ProbeContainer is the container name of the probe's own CPU and memory
// in ResourceSample rows.
const ProbeContainer = "juju-lens-probe"

// resAttrs flattens what an execution cost into res_* span attributes.
func resAttrs(e *unit.Execution) map[string]string {
	a := map[string]string{
		AttrResSource:        e.Source,
		"res_proc_start":     e.ProcStart.UTC().Format(time.RFC3339Nano),
		"res_cpu_ms":         ms(e.Tree.CPUUserMs + e.Tree.CPUSysMs),
		"res_cpu_user_ms":    ms(e.Tree.CPUUserMs),
		"res_cpu_sys_ms":     ms(e.Tree.CPUSysMs),
		"res_peak_rss_bytes": strconv.FormatUint(e.Tree.PeakRSSBytes, 10),
		"res_max_procs":      strconv.Itoa(e.Tree.MaxProcs),
		"res_probe_cpu_ms":   ms(e.ProbeCPUMs),
	}
	if e.EndSlackMs > 0 {
		a["res_end_slack_ms"] = ms(e.EndSlackMs)
	}
	if e.LingerMs > 0 {
		a[AttrResLingerMs] = strconv.FormatInt(e.LingerMs, 10)
	}
	if e.Overlapped {
		a[AttrResOverlapped] = "true"
	}
	if e.Truncated {
		a["res_truncated"] = "true"
	}
	c := e.Cgroup
	a["res_cgroup"] = c.Container
	for k, v := range map[string]string{
		"cpu_ms": ms(c.CPUMs), "cpu_user_ms": ms(c.CPUUserMs), "cpu_sys_ms": ms(c.CPUSysMs),
		"throttled_ms":   ms(c.ThrottledMs),
		"mem_base_bytes": strconv.FormatUint(c.MemBaseBytes, 10),
		"mem_peak_bytes": strconv.FormatUint(c.MemPeakBytes, 10),
		"io_read_bytes":  strconv.FormatUint(c.IOReadBytes, 10),
		"io_write_bytes": strconv.FormatUint(c.IOWriteBytes, 10),
		"cpu_stall_ms":   ms(c.CPUStallMs), "mem_stall_ms": ms(c.MemStallMs), "io_stall_ms": ms(c.IOStallMs),
		"mem_samples":  strconv.Itoa(c.MemSamples),
		"window_start": c.WindowStart.UTC().Format(time.RFC3339Nano),
		"window_end":   c.WindowEnd.UTC().Format(time.RFC3339Nano),
	} {
		a["res_cgroup_"+k] = v
	}
	if c.MemMaxBytes > 0 {
		a["res_cgroup_mem_max_bytes"] = strconv.FormatUint(c.MemMaxBytes, 10)
	}
	// res_cgroup_mem_peak_bytes is exact when it is the reset memory.peak
	// or a new lifetime peak; otherwise the highest polled reading.
	if c.MemPeakSource != "" {
		a["res_cgroup_mem_peak_source"] = c.MemPeakSource
		a["res_cgroup_mem_peak_exact"] = strconv.FormatBool(c.MemPeakSource != unit.MemPeakPolled)
	}
	if c.Error != "" {
		a["res_cgroup_error"] = c.Error
	}
	if len(e.Workloads) > 0 {
		b, _ := json.Marshal(e.Workloads)
		a["res_workloads"] = string(b)
	}
	if len(e.Processes) > 0 {
		b, _ := json.Marshal(e.Processes)
		a["res_processes"] = string(b)
	}
	if e.ProcessesTruncated > 0 {
		a["res_processes_truncated"] = strconv.Itoa(e.ProcessesTruncated)
	}
	return a
}

// ms renders milliseconds to the microsecond.
func ms(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

// execRoot is the root span of an execution only the probe's /proc poll
// saw: a charm not built on ops, `juju exec`, or a hook that ran before the
// tap was installed. It has no children and no exit status.
func execRoot(model, host string, e *unit.Execution, rel string, offset int64) SpanRow {
	key := opsKey{model, host, fmt.Sprintf("proc-%d-%d", e.PID, e.ProcStart.UnixNano())}
	row := SpanRow{
		TraceID: opsTraceID(key), SpanID: opsSpanID(key, 0),
		Start: e.Start, End: e.End, Service: ServiceOps,
		Model: model, ModelUUID: e.Env["model_uuid"], App: appOf(e.Unit), Unit: e.Unit,
		RawFile: rel, RawOffset: offset,
		Attrs: map[string]string{"kind": OpsRoot, "pid": strconv.Itoa(e.PID), "command": e.Command},
	}
	if m := e.Env["model_name"]; m != "" {
		row.Model = m
	}
	if len(e.Env) > 0 {
		b, _ := json.Marshal(e.Env)
		row.Attrs["env"] = string(b)
	}
	// Only the /proc poll found it: the tap recorded nothing of it, though
	// the probe saw it run (tap_reason comes from the tap coverage when the
	// recording is indexed).
	row.Attrs["tap"], row.Attrs["seen_by"] = TapMissing, SeenByProc
	switch e.Kind {
	case unit.ExecHook:
		row.Name, row.Hook = e.Name, e.Name
		row.Attrs["exec"], row.Attrs["hook_kind"] = ExecHook, bareHookKind(e.Name, e.Env)
	default:
		// An action, or `juju exec`, which Juju runs as the action
		// juju-exec, as the status log shows it.
		row.Name = e.Name + " action"
		row.Hook = row.Name
		row.Attrs["exec"] = ExecAction
	}
	for k, v := range resAttrs(e) {
		row.Attrs[k] = v
	}
	return row
}

// ResourceSample is one cgroup's cumulative counters at one time, from a
// unit probe's resources record. The probe's own CPU and memory are a
// sample of container ProbeContainer.
type ResourceSample struct {
	Model, Host, Unit string // Unit: the host's only unit, "" on a machine with several
	TS                time.Time
	unit.Counters
}

// DiskSample is one filesystem's usage at one time.
type DiskSample struct {
	Model, Host, Unit string
	TS                time.Time
	unit.Disk
}

// PebbleChangeRow is one workload Pebble change, at the latest status a
// probe reported for it.
type PebbleChangeRow struct {
	Model, Host, Unit string
	unit.PebbleChange
}

// UnitResources is what a unit probe file holds besides tap records.
type UnitResources struct {
	Samples []ResourceSample
	Disks   []DiskSample
	Changes []PebbleChangeRow
}

// ResourceReader picks the resources and Pebble change records out of the
// lines of raw/unit/. Feed it every line in order: a host's unit comes from
// its session's hello.
type ResourceReader struct {
	units map[string]string // model + host -> its only unit
}

// NewResourceReader returns an empty reader.
func NewResourceReader() *ResourceReader {
	return &ResourceReader{units: map[string]string{}}
}

var (
	helloPrefix        = []byte(`{"kind":"` + unit.KindHello + `"`)
	resourcesPrefix    = []byte(`{"kind":"` + unit.KindResources + `"`)
	pebbleChangePrefix = []byte(`{"kind":"` + unit.KindPebbleChange + `"`)
)

// Feed reads one line of raw/unit/<model>/<host>/, read from rel. Lines of
// other kinds (most of them: tap records) are skipped without decoding.
func (rr *ResourceReader) Feed(model string, line []byte, rel string, out *UnitResources) {
	if !bytes.HasPrefix(line, helloPrefix) && !bytes.HasPrefix(line, resourcesPrefix) && !bytes.HasPrefix(line, pebbleChangePrefix) {
		return
	}
	var r unit.Record
	if err := json.Unmarshal(line, &r); err != nil {
		return
	}
	host := hostOf(rel)
	hk := model + "\x00" + host
	switch {
	case r.Kind == unit.KindHello && r.Hello != nil:
		rr.units[hk] = ""
		if len(r.Hello.Units) == 1 {
			rr.units[hk] = r.Hello.Units[0]
		}
	case r.Kind == unit.KindResources && r.Resources != nil:
		u := rr.units[hk]
		for _, c := range r.Resources.Containers {
			out.Samples = append(out.Samples, ResourceSample{Model: model, Host: host, Unit: u, TS: r.TS, Counters: c})
		}
		if r.Resources.Burst {
			break // workload counters only: no probe figures, no disks
		}
		out.Samples = append(out.Samples, ResourceSample{Model: model, Host: host, Unit: u, TS: r.TS, Counters: unit.Counters{
			Container: ProbeContainer,
			CPUUsec:   uint64(r.Resources.ProbeCPUMs * 1000),
			MemBytes:  r.Resources.ProbeRSSBytes,
		}})
		for _, d := range r.Resources.Disks {
			out.Disks = append(out.Disks, DiskSample{Model: model, Host: host, Unit: u, TS: r.TS, Disk: d})
		}
	case r.Kind == unit.KindPebbleChange && r.PebbleChange != nil:
		out.Changes = append(out.Changes, PebbleChangeRow{Model: model, Host: host, Unit: rr.units[hk], PebbleChange: *r.PebbleChange})
	}
}

// LoadUnitResources reads every unit probe file under raw/unit/ and returns
// its resource samples and Pebble changes, in file order.
func LoadUnitResources(root string) (UnitResources, error) {
	var out UnitResources
	rr := NewResourceReader()
	err := eachUnitLine(root, func(model, rel string, line []byte, _ int64) {
		rr.Feed(model, line, rel, &out)
	})
	return out, err
}

// eachUnitLine calls fn with every complete line of the unit probe files
// under raw/unit/, file by file in path order.
func eachUnitLine(root string, fn func(model, rel string, line []byte, offset int64)) error {
	paths, err := filepath.Glob(filepath.Join(NewLayout(root).RawDir(), "unit", "*", "*", "*.jsonl"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		model, ok := UnitFileModel(rel)
		if !ok {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading %s: %w", p, err)
		}
		var off int64
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				break // a partial last line is not a record yet
			}
			if i > 0 {
				fn(model, rel, data[:i], off)
			}
			off += int64(i + 1)
			data = data[i+1:]
		}
	}
	return nil
}
