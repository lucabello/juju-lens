package unit

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseProcStatCommWithParens(t *testing.T) {
	// comm may contain ") " itself; fields must be counted from the last ')'.
	line := "4242 (we) (ird) S 17 4242 4242 0 -1 4194560 100 0 0 0 " +
		"250 30 40 5 20 0 1 0 98765 1000000 300 18446744073709551615"
	st, err := parseProcStat([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	want := pstat{pid: 4242, ppid: 17, comm: "we) (ird", utime: 250, stime: 30, cutime: 40, cstime: 5, start: 98765, rss: 300}
	if st != want {
		t.Fatalf("got %+v, want %+v", st, want)
	}
	for _, line := range []string{"", "12 (sh", "12 (sh) S 1 2 3"} {
		if _, err := parseProcStat([]byte(line)); err == nil {
			t.Errorf("%q: want error", line)
		}
	}
}

func TestCgroupDir(t *testing.T) {
	for in, want := range map[string]string{
		"0::/\n": "/sys/fs/cgroup",
		"0::/system.slice/jujud-machine-0.service": "/sys/fs/cgroup/system.slice/jujud-machine-0.service",
		"12:cpu:/x\n0::/kubepods/pod1/ctr\n":       "/sys/fs/cgroup/kubepods/pod1/ctr",
		"0::/../../escape":                         "/sys/fs/cgroup/escape",
		"":                                         "/sys/fs/cgroup",
	} {
		if got := cgroupDir([]byte(in), "/sys/fs/cgroup"); got != want {
			t.Errorf("cgroupDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadCounters(t *testing.T) {
	files := map[string]string{
		"cpu.stat":        "usage_usec 900\nuser_usec 600\nsystem_usec 300\nnr_periods 0\nthrottled_usec 25\n",
		"memory.current":  "4096\n",
		"memory.max":      "max\n",
		"io.stat":         "8:0 rbytes=100 wbytes=10 rios=1 wios=1\n259:0 rbytes=50 wbytes=5 rios=1 wios=1\n",
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=777\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=999\n",
		"memory.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=3\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"memory.stat":     "anon 27103232\nfile 1187840\nkernel 3522560\nkernel_stack 573440\nshmem 4096\n",
		"memory.events":   "low 0\nhigh 2\nmax 1\noom 0\noom_kill 0\noom_group_kill 0\n",
		// memory.peak and io.pressure missing: older kernel, PSI partly off.
	}
	read := func(_ string, names []string) (map[string][]byte, error) {
		out := map[string][]byte{}
		for _, n := range names {
			if b, ok := files[n]; ok {
				out[n] = []byte(b)
			}
		}
		return out, nil
	}
	c, err := readCounters(read, "/sys/fs/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	want := Counters{
		CPUUsec: 900, CPUUserUsec: 600, CPUSysUsec: 300, ThrottledUsec: 25,
		MemBytes: 4096, IOReadBytes: 150, IOWriteBytes: 15,
		CPUStallUsec: 777, MemStallUsec: 3,
		MemStat:   &MemStat{AnonBytes: 27103232, FileBytes: 1187840, KernelBytes: 3522560, ShmemBytes: 4096},
		MemEvents: &MemEvents{High: 2, Max: 1},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
	down := func(string, []string) (map[string][]byte, error) { return nil, errors.New("pebble down") }
	if _, err := readCounters(down, "/"); err == nil {
		t.Fatal("want error when cpu.stat is unreadable")
	}
}

// Before Linux 5.18, memory.stat has no "kernel" line: its parts add up.
func TestParseMemStatOldKernel(t *testing.T) {
	m := parseMemStat([]byte("anon 10\nfile 20\nkernel_stack 1\npagetables 2\npercpu 3\nslab 4\nshmem 5\n"))
	if want := (MemStat{AnonBytes: 10, FileBytes: 20, KernelBytes: 10, ShmemBytes: 5}); *m != want {
		t.Fatalf("got %+v, want %+v", *m, want)
	}
}

// A reading of the cgroup shell splits back into its files.
func TestParseCgroupReading(t *testing.T) {
	got := parseCgroupReading([]byte("== cpu.stat\nusage_usec 9\nuser_usec 5\n== memory.current\n4096\n==\n"))
	want := map[string][]byte{"cpu.stat": []byte("usage_usec 9\nuser_usec 5\n"), "memory.current": []byte("4096\n")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExecKind(t *testing.T) {
	for _, c := range []struct {
		env        map[string]string
		kind, name string
	}{
		{map[string]string{"hook_name": "db-relation-changed", "dispatch_path": "hooks/db-relation-changed"}, ExecHook, "db-relation-changed"},
		{map[string]string{"dispatch_path": "hooks/install"}, ExecHook, "install"},
		{map[string]string{"action_name": "backup", "dispatch_path": "actions/backup"}, ExecAction, "backup"},
		{map[string]string{"action_name": "juju-exec", "context_id": "x"}, ExecJuju, "juju-exec"},
		{map[string]string{"unit_name": "otelcol/0"}, "", ""},
	} {
		if kind, name := execKind(c.env); kind != c.kind || name != c.name {
			t.Errorf("execKind(%v) = %s %s, want %s %s", c.env, kind, name, c.kind, c.name)
		}
	}
}
