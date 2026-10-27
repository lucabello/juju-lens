package unit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// clkTck is USER_HZ, the unit of /proc/<pid>/stat times. It is 100 on every
// Linux architecture Juju supports.
const clkTck = 100

// pstat is the part of /proc/<pid>/stat the resource sampler uses.
type pstat struct {
	pid, ppid                    int
	comm                         string
	utime, stime, cutime, cstime uint64 // clock ticks
	start                        uint64 // clock ticks since boot
	rss                          int64  // pages
}

// parseProcStat parses /proc/<pid>/stat. The command name is in parentheses
// and may itself contain spaces and parentheses, so fields are counted from
// the last ')'.
func parseProcStat(b []byte) (pstat, error) {
	l, r := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if l < 0 || r < l || r+2 > len(b) {
		return pstat{}, errors.New("malformed stat")
	}
	f := strings.Fields(string(b[r+2:])) // f[0] is field 3 (state)
	if len(f) < 22 {
		return pstat{}, fmt.Errorf("stat has %d fields after comm, want >= 22", len(f))
	}
	field := func(n int) uint64 { return parseUint([]byte(f[n-3])) }
	pid := parseUint(bytes.TrimSpace(b[:l]))
	rss, _ := strconv.ParseInt(f[24-3], 10, 64)
	return pstat{
		pid: int(pid), ppid: int(field(4)), comm: string(b[l+1 : r]),
		utime: field(14), stime: field(15), cutime: field(16), cstime: field(17),
		start: field(22), rss: rss,
	}, nil
}

// parseUint parses a decimal number, ignoring surrounding whitespace. It
// returns 0 for anything else, which is the safe reading for a counter.
func parseUint(b []byte) uint64 {
	v, err := strconv.ParseUint(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// execEnvKeys are the JUJU_* variables an Execution reports, the same ones
// the tap records in proc_start.
var execEnvKeys = []string{
	"JUJU_UNIT_NAME", "JUJU_MODEL_NAME", "JUJU_MODEL_UUID", "JUJU_DISPATCH_PATH",
	"JUJU_HOOK_NAME", "JUJU_ACTION_NAME", "JUJU_ACTION_UUID", "JUJU_RELATION",
	"JUJU_RELATION_ID", "JUJU_REMOTE_UNIT", "JUJU_REMOTE_APP", "JUJU_DEPARTING_UNIT",
	"JUJU_SECRET_ID", "JUJU_SECRET_LABEL", "JUJU_SECRET_REVISION", "JUJU_NOTICE_ID",
	"JUJU_NOTICE_TYPE", "JUJU_NOTICE_KEY", "JUJU_WORKLOAD_NAME", "JUJU_PEBBLE_CHECK_NAME",
	"JUJU_STORAGE_ID", "JUJU_CHARM_DIR", "JUJU_CONTEXT_ID", "JUJU_VERSION",
	"JUJU_DEBUG_AT",
}

// parseExecEnv extracts the execEnvKeys from a NUL-separated environ, keyed
// as the tap keys them: without the prefix, in lower case.
func parseExecEnv(b []byte) map[string]string {
	m := map[string]string{}
	for _, kv := range bytes.Split(b, []byte{0}) {
		k, v, ok := strings.Cut(string(kv), "=")
		if !ok || !strings.HasPrefix(k, "JUJU_") {
			continue
		}
		for _, want := range execEnvKeys {
			if k == want {
				m[strings.ToLower(k[len("JUJU_"):])] = v
				break
			}
		}
	}
	return m
}

// execKind tells from its environment what an execution is, and its name.
// kind is "" for a process with no Juju execution environment.
func execKind(env map[string]string) (kind, name string) {
	switch action := env["action_name"]; {
	case action == "juju-exec" || action == "juju-run":
		return ExecJuju, "juju-exec"
	case action != "":
		return ExecAction, action
	case env["hook_name"] != "":
		return ExecHook, env["hook_name"]
	case strings.HasPrefix(env["dispatch_path"], "hooks/"):
		return ExecHook, strings.TrimPrefix(env["dispatch_path"], "hooks/")
	case strings.HasPrefix(env["dispatch_path"], "actions/"):
		return ExecAction, strings.TrimPrefix(env["dispatch_path"], "actions/")
	}
	return "", ""
}

// cgroupDir maps a /proc/<pid>/cgroup file to the cgroup v2 directory under
// root (/sys/fs/cgroup). Inside a container with its own cgroup namespace
// the path is "/", which is the mount root.
func cgroupDir(b []byte, root string) string {
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(root, filepath.Clean("/"+p))
		}
	}
	return root
}

// storageMounts returns the mount points of Juju storage from a mountinfo
// file: anything mounted under /var/lib/juju/storage.
func storageMounts(mountinfo []byte) []string {
	var out []string
	for _, line := range strings.Split(string(mountinfo), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if mp := f[4]; strings.HasPrefix(mp, "/var/lib/juju/storage/") {
			out = append(out, mp)
		}
	}
	return out
}

// cgroupFiles are the files of a cgroup v2 directory readCounters uses.
var cgroupFiles = []string{
	"cpu.stat", "memory.current", "memory.peak", "memory.max", "io.stat",
	"cpu.pressure", "memory.pressure", "io.pressure", "memory.stat", "memory.events",
}

// filesReader reads the named files of a directory and returns the ones it
// could read, by name. err is the first failure: of one file, or of the
// whole read.
type filesReader func(dir string, names []string) (files map[string][]byte, err error)

// readLocalFiles is the filesReader of a cgroup the probe can see itself.
func readLocalFiles(dir string, names []string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(names))
	var first error
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		files[n] = b
	}
	return files, first
}

// readCounters reads a cgroup v2 directory's counters through read, which is
// local file reads or one Pebble pull for all the files. Missing optional
// files (memory.peak on older kernels, pressure files with PSI disabled)
// leave their fields zero, or nil; it fails only when cpu.stat itself can't
// be read.
func readCounters(read filesReader, dir string) (Counters, error) {
	var c Counters
	files, err := read(dir, cgroupFiles)
	b, ok := files["cpu.stat"]
	if !ok {
		if err == nil {
			err = fmt.Errorf("%s: not readable", filepath.Join(dir, "cpu.stat"))
		}
		return c, err
	}
	cpu := parseFlatKeyed(b)
	c.CPUUsec, c.CPUUserUsec, c.CPUSysUsec = cpu["usage_usec"], cpu["user_usec"], cpu["system_usec"]
	c.ThrottledUsec = cpu["throttled_usec"]
	if b, ok := files["memory.current"]; ok {
		c.MemBytes = parseUint(b)
	}
	if b, ok := files["memory.peak"]; ok {
		c.MemPeakBytes = parseUint(b)
	}
	if b, ok := files["memory.max"]; ok {
		c.MemMaxBytes = parseUint(b) // "max" parses as 0: no limit
	}
	if b, ok := files["io.stat"]; ok {
		c.IOReadBytes, c.IOWriteBytes = parseIOStat(b)
	}
	for _, p := range []struct {
		file string
		dst  *uint64
	}{
		{"cpu.pressure", &c.CPUStallUsec}, {"memory.pressure", &c.MemStallUsec}, {"io.pressure", &c.IOStallUsec},
	} {
		if b, ok := files[p.file]; ok {
			*p.dst = parsePressureSomeTotal(b)
		}
	}
	if b, ok := files["memory.stat"]; ok {
		c.MemStat = parseMemStat(b)
	}
	if b, ok := files["memory.events"]; ok {
		ev := parseFlatKeyed(b)
		c.MemEvents = &MemEvents{High: ev["high"], Max: ev["max"], OOM: ev["oom"], OOMKill: ev["oom_kill"]}
	}
	return c, nil
}

// parseMemStat takes the breakdown of a cgroup's memory from memory.stat.
// Kernels before 5.18 have no "kernel" line; its parts are summed instead.
func parseMemStat(b []byte) *MemStat {
	m := parseFlatKeyed(b)
	kernel, ok := m["kernel"]
	if !ok {
		kernel = m["kernel_stack"] + m["pagetables"] + m["sec_pagetables"] + m["percpu"] + m["slab"] + m["vmalloc"]
	}
	return &MemStat{AnonBytes: m["anon"], FileBytes: m["file"], KernelBytes: kernel, ShmemBytes: m["shmem"]}
}

// parseFlatKeyed parses "key value" lines (cpu.stat, memory.stat).
func parseFlatKeyed(b []byte) map[string]uint64 {
	m := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			m[f[0]] = parseUint([]byte(f[1]))
		}
	}
	return m
}

// parseIOStat sums read and written bytes over every device in io.stat
// ("8:0 rbytes=1 wbytes=2 rios=...").
func parseIOStat(b []byte) (rbytes, wbytes uint64) {
	for _, line := range strings.Split(string(b), "\n") {
		for _, kv := range strings.Fields(line) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case "rbytes":
				rbytes += parseUint([]byte(v))
			case "wbytes":
				wbytes += parseUint([]byte(v))
			}
		}
	}
	return rbytes, wbytes
}

// parsePressureSomeTotal returns the cumulative "some" stall time, in
// microseconds, from a PSI file.
func parsePressureSomeTotal(b []byte) uint64 {
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "some ")
		if !ok {
			continue
		}
		for _, kv := range strings.Fields(rest) {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				return parseUint([]byte(v))
			}
		}
	}
	return 0
}

// usage turns two counter readings into the delta across an execution.
// memPeak is the highest of memN polled readings in between; when the
// cgroup's lifetime memory.peak rose from c0 to c1, its new value is the
// exact peak instead.
func usage(c0, c1 Counters, from, to time.Time, memPeak uint64, memN int) ContainerUsage {
	u := ContainerUsage{
		Container: c1.Container, WindowStart: from.UTC(), WindowEnd: to.UTC(),
		CPUMs:        usecDelta(c0.CPUUsec, c1.CPUUsec),
		CPUUserMs:    usecDelta(c0.CPUUserUsec, c1.CPUUserUsec),
		CPUSysMs:     usecDelta(c0.CPUSysUsec, c1.CPUSysUsec),
		ThrottledMs:  usecDelta(c0.ThrottledUsec, c1.ThrottledUsec),
		MemBaseBytes: c0.MemBytes, MemMaxBytes: c1.MemMaxBytes,
		MemPeakBytes: max(memPeak, c0.MemBytes, c1.MemBytes),
		IOReadBytes:  sub(c0.IOReadBytes, c1.IOReadBytes),
		IOWriteBytes: sub(c0.IOWriteBytes, c1.IOWriteBytes),
		CPUStallMs:   usecDelta(c0.CPUStallUsec, c1.CPUStallUsec),
		MemStallMs:   usecDelta(c0.MemStallUsec, c1.MemStallUsec),
		IOStallMs:    usecDelta(c0.IOStallUsec, c1.IOStallUsec),
		MemSamples:   memN,
	}
	u.MemPeakSource = MemPeakPolled
	if c0.MemPeakBytes > 0 && c1.MemPeakBytes > c0.MemPeakBytes {
		u.MemPeakBytes, u.MemPeakSource = c1.MemPeakBytes, MemPeakHigh
	}
	switch {
	case c0.Error != "":
		u.Error = c0.Error
	case c1.Error != "":
		u.Error = c1.Error
	}
	return u
}

// sub is b - a for a cumulative counter, 0 when it went backwards (the
// container restarted).
func sub(a, b uint64) uint64 {
	if b < a {
		return 0
	}
	return b - a
}

func usecDelta(a, b uint64) float64 { return float64(sub(a, b)) / 1000 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
