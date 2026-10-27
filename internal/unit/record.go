// Package unit is the unit side of `record --attach unit`: what
// juju-lens-probe does inside a Kubernetes unit's charm container, or on a
// machine next to the machine agent, and the records it streams back to the
// recorder over its `juju ssh` session.
//
// The probe writes one JSON Record per line on stdout. This file holds the
// record types, shared by the probe and the recorder; it has no OS-specific
// code.
package unit

import (
	"encoding/json"
	"time"
)

// Record kinds.
const (
	// KindHello is the first record of a session: the probe owns the unit
	// and found its Juju agent.
	KindHello = "hello"
	// KindHeartbeat is written every HeartbeatEvery while the probe runs.
	// Writing it is also how the probe notices the recorder went away.
	KindHeartbeat = "heartbeat"
	// KindBusy is the only record of a probe that found another live probe
	// on the same unit, from a different recording. It exits right after.
	KindBusy = "busy"
	// KindLoader reports a change to the tap's installation in one charm.
	KindLoader = "loader"
	// KindTap carries one line the tap wrote in a hook process, forwarded
	// as soon as it was written.
	KindTap = "tap"
	// KindSpoolEnd says a hook process's spool file is complete: the
	// process exited and every line of it was forwarded.
	KindSpoolEnd = "spool_end"
	// KindExecution reports one finished execution and what it cost. For
	// a process the tap recorded, it comes before the process's spool_end.
	KindExecution = "execution"
	// KindResources is a snapshot of the cgroups', disks' and the probe's
	// own counters, written every ResourcesEvery; and, while a burst window
	// is open, of the workload containers' counters every BurstEvery.
	KindResources = "resources"
	// KindPebbleChange is a workload Pebble change that appeared or
	// changed status.
	KindPebbleChange = "pebble_change"
	// KindPebbleLog is one line a workload service wrote, from its
	// container's Pebble, when the probe runs with Config.PebbleLogs.
	KindPebbleLog = "pebble_log"
)

// Loader events.
const (
	// LoaderInstalled: the probe installed the tap in a charm venv; hooks
	// that started before this time ran without it.
	LoaderInstalled = "installed"
	// LoaderReinstalled: the tap had gone (a refresh replaced the venv)
	// and is back. Hooks between DirChanged and this record ran without it.
	LoaderReinstalled = "reinstalled"
	// LoaderSkipped: the tap can't be installed there; Reason says why,
	// ReasonNoVenv for a charm with no venv at all.
	LoaderSkipped = "skipped"
	// LoaderRemoved: the probe removed the tap as the recording ended.
	LoaderRemoved = "removed"
	// LoaderUnavailable: the probe couldn't set up the spool, so no charm
	// on the host gets the tap; Reason says why.
	LoaderUnavailable = "unavailable"
)

// ReasonNoVenv is the Reason of a LoaderSkipped for a charm with no venv:
// one not built on ops, whose hooks the tap can't see. Its Dir is the charm
// directory.
const ReasonNoVenv = "the charm has no venv: it isn't built on ops"

// HeartbeatEvery is how often a running probe writes a heartbeat.
const HeartbeatEvery = 5 * time.Second

// ResourcesEvery is how often a running probe writes a resources snapshot.
const ResourcesEvery = 5 * time.Second

// BurstEvery is how often the workload containers' counters are written
// while a hook runs and for the linger time after the last Pebble change
// it caused is ready.
const BurstEvery = 100 * time.Millisecond

// DefaultLinger is the probe's default linger time: how long after the last
// Pebble change a hook caused is ready the workloads are still sampled every
// BurstEvery, so a workload that restarts and grows is seen in detail.
const DefaultLinger = 30 * time.Second

// StaleAfter is how long a probe can go without a successful heartbeat
// before the next probe on the unit treats it as left behind and stops it.
const StaleAfter = 30 * time.Second

// Tap features, written to the spool's config; each hook process reads it
// as it starts.
const (
	FeatureProc   = "proc"   // proc_start and proc_end
	FeatureOps    = "ops"    // events, observers, hook tools, Pebble calls
	FeatureStacks = "stacks" // stack samples, tagged with the running span
)

// DefaultFeatures are the tap features a probe turns on.
var DefaultFeatures = []string{FeatureProc, FeatureOps, FeatureStacks}

// Record is one line of probe output. At most one of the pointer fields is
// set, matching Kind.
type Record struct {
	Kind         string        `json:"kind"`
	TS           time.Time     `json:"ts"`
	Hello        *Hello        `json:"hello,omitempty"`
	Busy         *Busy         `json:"busy,omitempty"`
	Loader       *Loader       `json:"loader,omitempty"`
	Tap          *Tap          `json:"tap,omitempty"`
	SpoolEnd     *SpoolEnd     `json:"spool_end,omitempty"`
	Execution    *Execution    `json:"execution,omitempty"`
	Resources    *Resources    `json:"resources,omitempty"`
	PebbleChange *PebbleChange `json:"pebble_change,omitempty"`
	PebbleLog    *PebbleLog    `json:"pebble_log,omitempty"`
}

// Hello describes the probe and what it found on the host.
type Hello struct {
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	Substrate string `json:"substrate"` // "kubernetes" or "machine"
	Agent     string `json:"agent"`     // agent binary name
	AgentPID  int    `json:"agent_pid"`
	// AgentStart is when the agent process started: on Kubernetes, about
	// when the charm container did.
	AgentStart time.Time `json:"agent_start"`
	Units      []string  `json:"units,omitempty"` // units whose agent directories are on this host
	BootTime   time.Time `json:"boot_time"`
	// Cgroup is the agent's cgroup directory, which the probe measures.
	Cgroup string `json:"cgroup,omitempty"`
	// Workloads are the workload containers reachable over Pebble.
	Workloads []string `json:"workloads,omitempty"`
	// IdleHz and RunHz are the /proc poll rates while no execution runs
	// and while one does.
	IdleHz int `json:"idle_hz,omitempty"`
	RunHz  int `json:"run_hz,omitempty"`
	// ProbeInCgroup: the probe shares the agent's cgroup, so its own CPU is
	// subtracted from the cgroup deltas it reports.
	ProbeInCgroup bool `json:"probe_in_cgroup,omitempty"`
	// LingerMs is how long workloads are still sampled every BurstEvery
	// after the last Pebble change a hook caused is ready.
	LingerMs int64 `json:"linger_ms,omitempty"`
	// MemPeakReset: the agent's cgroup has a resettable memory.peak (Linux
	// 6.12 and later, on a cgroup filesystem the probe can write), so each
	// execution's cgroup peak is exact. Kubernetes mounts it read-only.
	MemPeakReset bool `json:"mem_peak_reset,omitempty"`
}

// Busy names the probe that already owns the unit.
type Busy struct {
	PID int `json:"pid"`
	// HeartbeatAgeMs is how long ago that probe last wrote a heartbeat.
	HeartbeatAgeMs int64 `json:"heartbeat_age_ms"`
}

// Loader is one change to the tap's installation.
type Loader struct {
	Event  string `json:"event"`
	Unit   string `json:"unit,omitempty"`
	Dir    string `json:"dir,omitempty"`
	Loader string `json:"loader,omitempty"` // "_juju_lens_tap.pth" or "sitecustomize.py"
	Reason string `json:"reason,omitempty"`
	// DirChanged is when Dir last changed, for installed and reinstalled.
	DirChanged time.Time `json:"dir_changed,omitzero"`
}

// Tap is one line of a hook process's spool file. Host, Unit and PID say
// where it came from; File is the spool file, unique to the process.
type Tap struct {
	Host string `json:"host"`
	Unit string `json:"unit"`
	PID  int    `json:"pid"`
	File string `json:"file"`
	// Record is the tap's record, as written. A line that isn't valid
	// JSON is in Text instead.
	Record json.RawMessage `json:"rec,omitempty"`
	Text   string          `json:"text,omitempty"`
}

// SpoolEnd closes the Tap records of one spool file.
type SpoolEnd struct {
	Host  string `json:"host"`
	Unit  string `json:"unit"`
	PID   int    `json:"pid"`
	File  string `json:"file"`
	Lines int64  `json:"lines"`
	// Truncated: the process exited without a proc_end record, so it was
	// killed (SIGKILL, OOM) or ended without running its exit handlers.
	Truncated bool `json:"truncated,omitempty"`
	// Partial is the length of a last line that never got its newline;
	// it isn't forwarded.
	Partial int `json:"partial,omitempty"`
}

// PebbleLog is one line of a workload service's output, as the container's
// Pebble buffered it: what `kubectl logs` shows for the container, less
// Pebble's own lines.
type PebbleLog struct {
	Container string    `json:"container"`
	Time      time.Time `json:"time"` // when Pebble read the line
	Service   string    `json:"service"`
	Message   string    `json:"message"`
}
