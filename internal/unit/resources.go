package unit

import "time"

// The probe also measures what each charm execution costs, with no
// privileges and no instrumentation beyond the tap: the agent's cgroup
// (the charm container on Kubernetes, the machine agent's service on
// machines), every workload container's cgroup through its Pebble socket,
// the execution's own process tree from /proc, and disk usage. The types
// below are the records it writes for that; they have no OS-specific code.
//
// It learns when a hook starts and ends from the tap's proc_start and
// proc_end records, which are exact. For what the tap can't see (charms not
// built on ops, `juju exec`, a hook that started before the tap was
// installed) it polls /proc for the agent's children.

// Execution sources: what found an execution and gave its boundaries.
const (
	// SourceTap: the tap recorded the process; Start and End are its
	// proc_start and proc_end records.
	SourceTap = "tap"
	// SourceProc: only the /proc poll saw it; Start is the process start
	// time and End the first poll that found it gone.
	SourceProc = "proc"
)

// Execution kinds. A child of the agent with no Juju execution environment
// (the uniter's own helpers, such as the tmux check for debug-hooks) isn't
// an execution, and isn't measured.
const (
	ExecHook   = "hook"
	ExecAction = "action"
	ExecJuju   = "exec" // `juju exec`
)

// Execution is one finished hook, action or `juju exec`, and what it cost.
// For a process the tap recorded it is written before that process's
// SpoolEnd.
type Execution struct {
	Unit string `json:"unit"`
	Kind string `json:"kind"` // ExecHook, ExecAction or ExecJuju
	Name string `json:"name"` // hook or action name, or "juju-exec"
	// Env holds the execution's JUJU_* variables the tap also records,
	// without the prefix and in lower case ("remote_app").
	Env     map[string]string `json:"env,omitempty"`
	PID     int               `json:"pid"`
	Command string            `json:"command"`

	// Source is SourceTap or SourceProc. TapFile is the process's spool
	// file when the tap recorded it.
	Source  string `json:"source"`
	TapFile string `json:"tap_file,omitempty"`
	// ProcStart is the process start time from /proc (10 ms resolution).
	ProcStart time.Time `json:"proc_start"`
	// Start and End bound the execution: the tap's proc_start and proc_end
	// for SourceTap, else ProcStart and the first poll that found the
	// process gone, late by at most EndSlackMs.
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	EndSlackMs float64   `json:"end_slack_ms,omitempty"`
	// Truncated: the tap recorded the process but it exited without a
	// proc_end (killed); End is when the probe found it gone.
	Truncated bool `json:"truncated,omitempty"`
	// Overlapped is true when another execution ran in the same cgroup at
	// the same time (several units on one machine). The cgroup deltas then
	// cover both, and Tree's CPU is unknown.
	Overlapped bool `json:"overlapped,omitempty"`

	Tree TreeUsage `json:"tree"`
	// Cgroup is the agent's cgroup across the execution: the charm
	// container on Kubernetes, the machine agent's service on machines.
	Cgroup    ContainerUsage   `json:"cgroup"`
	Workloads []ContainerUsage `json:"workloads,omitempty"`
	Processes []Proc           `json:"processes,omitempty"`
	// ProcessesTruncated counts processes not listed because of the cap.
	ProcessesTruncated int `json:"processes_truncated,omitempty"`
	// ProbeCPUMs is the probe's own CPU during the execution; already
	// subtracted from Cgroup when Hello.ProbeInCgroup.
	ProbeCPUMs float64 `json:"probe_cpu_ms"`
	// LingerMs is how long after the execution, or after the last Pebble
	// change it caused was ready, workloads were still sampled in bursts.
	LingerMs int64 `json:"linger_ms,omitempty"`
}

// TreeUsage is the resource usage of the execution's process tree itself.
type TreeUsage struct {
	// CPUUserMs/CPUSysMs come from the agent's reaped-children counters
	// (cutime/cstime): exact for every process of the tree. Zero when
	// Overlapped.
	CPUUserMs float64 `json:"cpu_user_ms"`
	CPUSysMs  float64 `json:"cpu_sys_ms"`
	// PeakRSSBytes is the largest sum of RSS across the tree seen by a poll.
	PeakRSSBytes uint64 `json:"peak_rss_bytes"`
	// MaxProcs is the largest number of live processes seen at once.
	MaxProcs int `json:"max_procs"`
}

// ContainerUsage is a cgroup's usage across one execution.
type ContainerUsage struct {
	Container string `json:"container"`
	// The deltas cover [WindowStart, WindowEnd]: from when the probe
	// noticed the execution, up to an idle poll after it started, to the
	// first reading after it ended.
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	CPUMs        float64   `json:"cpu_ms"`
	CPUUserMs    float64   `json:"cpu_user_ms"`
	CPUSysMs     float64   `json:"cpu_sys_ms"`
	ThrottledMs  float64   `json:"throttled_ms"`
	MemBaseBytes uint64    `json:"mem_base_bytes"`
	MemPeakBytes uint64    `json:"mem_peak_bytes"`
	// MemPeakSource says how MemPeakBytes was found; MemPeakReset and
	// MemPeakHigh are exact.
	MemPeakSource string  `json:"mem_peak_source,omitempty"`
	MemMaxBytes   uint64  `json:"mem_max_bytes,omitempty"` // 0: no limit
	IOReadBytes   uint64  `json:"io_read_bytes"`
	IOWriteBytes  uint64  `json:"io_write_bytes"`
	CPUStallMs    float64 `json:"cpu_stall_ms"`
	MemStallMs    float64 `json:"mem_stall_ms"`
	IOStallMs     float64 `json:"io_stall_ms"`
	// MemSamples is how many memory readings MemPeakBytes is the largest of.
	MemSamples int    `json:"mem_samples"`
	Error      string `json:"error,omitempty"`
}

// Proc is one process seen inside an execution's tree.
type Proc struct {
	PID      int       `json:"pid"`
	PPID     int       `json:"ppid"`
	Command  string    `json:"command"`
	Start    time.Time `json:"start"`
	LastSeen time.Time `json:"last_seen"`
	// CPUMs is the CPU the process had used at the last poll that saw it.
	CPUMs        float64 `json:"cpu_ms"`
	PeakRSSBytes uint64  `json:"peak_rss_bytes"`

	comm string // /proc/<pid>/stat comm when Command was read; changes on exec
}

// Resources is a periodic snapshot of cumulative counters, for time series
// between and across executions.
type Resources struct {
	Containers []Counters `json:"containers"`
	Disks      []Disk     `json:"disks,omitempty"`
	// ProbeCPUMs and ProbeRSSBytes are the probe's own cumulative CPU and
	// current RSS, so its overhead is visible in every recording.
	ProbeCPUMs    float64 `json:"probe_cpu_ms"`
	ProbeRSSBytes uint64  `json:"probe_rss_bytes"`
	// Burst: a snapshot of the workload containers only, taken every
	// BurstEvery while a hook runs and lingers; it has no disks and no
	// probe figures.
	Burst bool `json:"burst,omitempty"`
}

// How a ContainerUsage's MemPeakBytes was found.
const (
	// MemPeakReset: memory.peak through a file descriptor reset as the
	// execution started (Linux 6.12 and later): exact.
	MemPeakReset = "reset"
	// MemPeakHigh: the cgroup's lifetime memory.peak rose during the
	// execution, so its new value is the execution's peak: exact.
	MemPeakHigh = "high-water"
	// MemPeakPolled: the highest memory.current the probe read during the
	// execution, about RunHz times a second; a shorter peak is missed.
	MemPeakPolled = "polled"
)

// Counters is one cgroup's cumulative counters at a point in time.
type Counters struct {
	Container     string `json:"container"`
	CPUUsec       uint64 `json:"cpu_usec"`
	CPUUserUsec   uint64 `json:"cpu_user_usec"`
	CPUSysUsec    uint64 `json:"cpu_sys_usec"`
	ThrottledUsec uint64 `json:"throttled_usec"`
	MemBytes      uint64 `json:"mem_bytes"`
	MemPeakBytes  uint64 `json:"mem_peak_bytes,omitempty"`
	MemMaxBytes   uint64 `json:"mem_max_bytes,omitempty"`
	IOReadBytes   uint64 `json:"io_read_bytes"`
	IOWriteBytes  uint64 `json:"io_write_bytes"`
	CPUStallUsec  uint64 `json:"cpu_stall_usec"`
	MemStallUsec  uint64 `json:"mem_stall_usec"`
	IOStallUsec   uint64 `json:"io_stall_usec"`
	// MemStat and MemEvents are nil where memory.stat or memory.events
	// couldn't be read.
	MemStat   *MemStat   `json:"mem_stat,omitempty"`
	MemEvents *MemEvents `json:"mem_events,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// MemStat is where a cgroup's memory is, from memory.stat: anonymous memory
// (heaps, stacks) apart from the page cache, which the kernel can reclaim.
type MemStat struct {
	AnonBytes   uint64 `json:"anon"`
	FileBytes   uint64 `json:"file"`   // page cache, shmem included
	KernelBytes uint64 `json:"kernel"` // kernel memory charged to the cgroup
	ShmemBytes  uint64 `json:"shmem"`  // tmpfs and shared memory
}

// MemEvents are a cgroup's cumulative memory.events counters.
type MemEvents struct {
	High    uint64 `json:"high"`     // reclaim forced by memory.high
	Max     uint64 `json:"max"`      // usage about to exceed memory.max
	OOM     uint64 `json:"oom"`      // allocations that failed at memory.max
	OOMKill uint64 `json:"oom_kill"` // processes the OOM killer killed
}

// Disk is the usage of one filesystem visible to the agent.
type Disk struct {
	Path       string `json:"path"`
	SizeBytes  uint64 `json:"size_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	AvailBytes uint64 `json:"avail_bytes"`
}

// PebbleChange is a Pebble change in a workload container (a replan, a
// service start, an exec) that appeared or changed status while recording.
type PebbleChange struct {
	Container string    `json:"container"`
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Status    string    `json:"status"`
	Ready     bool      `json:"ready"`
	Err       string    `json:"err,omitempty"`
	Spawn     time.Time `json:"spawn"`
	ReadyTime time.Time `json:"ready_time,omitzero"`
}
