//go:build linux

package unit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The tap writes one spool file per hook process, named
// "<unit with / as ->-<pid>-<start ns>.jsonl", one JSON record per line, each
// with a single write(2) on an O_APPEND descriptor. The spooler forwards
// every complete line as soon as it is written, and deletes each file once
// its process has exited and the file has been read to the end.
//
// It never polls to find new lines: inotify tells it which files changed.
// When the kernel's event queue overflows, events are lost, so it rescans
// the whole spool; reading resumes at each file's offset, so a rescan
// neither loses nor repeats a line.

const spoolSuffix = ".jsonl"

// procStartPrefix and procEndPrefix start the tap's first and last records
// of a process. The spooler looks for them to give the resource sampler
// each process's boundaries, and to tell a process that ended from one
// that was killed; it doesn't otherwise read the records it forwards.
var (
	procStartPrefix = []byte(`"k":"proc_start"`)
	procEndPrefix   = []byte(`"k":"proc_end"`)
)

// spoolFile is what the spooler knows about one spool file.
type spoolFile struct {
	name    string
	unit    string
	pid     int
	start   time.Time // when the tap created the file
	off     int64     // bytes read so far
	partial []byte    // the end of the file after its last newline
	lines   int64
	sawEnd  bool // a proc_end record went by
}

// spooler streams the tap's spool directory as Tap and SpoolEnd records.
type spooler struct {
	dir      string
	procRoot string
	host     string
	// procEnd says whether the tap writes proc_end records: only then can
	// a missing one mean the process was killed.
	procEnd bool
	emit    func(Record) error
	logf    func(string, ...any)
	// res, when set, is told where each process starts and ends.
	res *sampler

	ino   *os.File // the inotify descriptor, read through the runtime poller
	done  chan struct{}
	files map[string]*spoolFile
	// overflows counts queue overflows, each followed by a rescan.
	overflows int
}

// spoolEvents is one read of inotify events: the files that changed, the
// ones whose last writer closed them, and whether the queue overflowed.
type spoolEvents struct {
	changed  []string
	closed   []string
	overflow bool
}

func newSpooler(dir, procRoot string, procEnd bool, emit func(Record) error, logf func(string, ...any)) (*spooler, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	host, _ := os.Hostname()
	return &spooler{
		dir: dir, procRoot: procRoot, host: host, procEnd: procEnd, emit: emit, logf: logf,
		ino: os.NewFile(uintptr(fd), "inotify"), done: make(chan struct{}), files: map[string]*spoolFile{},
	}, nil
}

// watch reads inotify events and sends them, one read at a time, until the
// descriptor is closed. Run it in its own goroutine.
func (s *spooler) watch(out chan<- spoolEvents) {
	defer close(out)
	buf := make([]byte, 64<<10)
	for {
		n, err := s.ino.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				s.logf("unit probe: spool watch: %v", err)
			}
			return
		}
		var ev spoolEvents
		for i := 0; i+unix.SizeofInotifyEvent <= n; {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[i]))
			name := string(bytes.TrimRight(buf[i+unix.SizeofInotifyEvent:i+unix.SizeofInotifyEvent+int(raw.Len)], "\x00"))
			i += unix.SizeofInotifyEvent + int(raw.Len)
			switch {
			case raw.Mask&unix.IN_Q_OVERFLOW != 0:
				ev.overflow = true
			case !strings.HasSuffix(name, spoolSuffix):
			case raw.Mask&unix.IN_CLOSE_WRITE != 0:
				ev.closed = append(ev.closed, name)
			default:
				ev.changed = append(ev.changed, name)
			}
		}
		select {
		case out <- ev:
		case <-s.done:
			return
		}
	}
}

// close stops watch.
func (s *spooler) close() {
	close(s.done)
	_ = s.ino.Close()
}

// scan reads every file in the spool: at start, after an overflow, and as
// the probe stops. It finishes the files whose process has exited.
func (s *spooler) scan() error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), spoolSuffix) && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := s.read(n); err != nil {
			return err
		}
	}
	return s.reap()
}

// handle forwards what one batch of events says was written, and finishes
// the files whose last writer closed them.
func (s *spooler) handle(ev spoolEvents) error {
	if ev.overflow {
		s.overflows++
		s.logf("unit probe: spool events overflowed; rescanning")
		return s.scan()
	}
	for _, n := range ev.changed {
		if err := s.read(n); err != nil {
			return err
		}
	}
	for _, n := range ev.closed {
		if err := s.read(n); err != nil {
			return err
		}
		if f := s.files[n]; f != nil && !s.alive(f) {
			if err := s.finish(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// reap finishes every file whose process has exited. A file normally ends
// on its close event; this catches a process that left a child holding the
// file open, and files from before the probe started.
func (s *spooler) reap() error {
	names := make([]string, 0, len(s.files))
	for n := range s.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := s.files[n]
		if s.alive(f) {
			continue
		}
		if err := s.read(n); err != nil {
			return err
		}
		if err := s.finish(f); err != nil {
			return err
		}
	}
	return nil
}

// read forwards the complete lines written to a file since the last read.
// Only an error from emit is returned: a file that went away has nothing
// more to give.
func (s *spooler) read(name string) error {
	f := s.files[name]
	if f == nil {
		unitName, pid, start, ok := parseSpoolName(name)
		if !ok {
			return nil
		}
		f = &spoolFile{name: name, unit: unitName, pid: pid, start: start}
		s.files[name] = f
	}
	fd, err := os.OpenFile(filepath.Join(s.dir, name), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			delete(s.files, name)
		}
		return nil
	}
	defer fd.Close()
	data, err := io.ReadAll(io.NewSectionReader(fd, f.off, 1<<62))
	if err != nil || len(data) == 0 {
		return nil
	}
	f.off += int64(len(data))
	if len(f.partial) > 0 {
		data = append(f.partial, data...)
		f.partial = nil
	}
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		f.partial = data
		return nil
	}
	if last < len(data)-1 {
		f.partial = append([]byte(nil), data[last+1:]...)
	}
	for _, line := range bytes.Split(data[:last], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		t := &Tap{Host: s.host, Unit: f.unit, PID: f.pid, File: f.name}
		if json.Valid(line) {
			t.Record = json.RawMessage(line)
		} else {
			t.Text = string(line)
		}
		if err := s.emit(Record{Kind: KindTap, Tap: t}); err != nil {
			return err
		}
		f.lines++
		if !f.sawEnd && bytes.Contains(line, procEndPrefix) {
			f.sawEnd = true
			s.boundary(f, line, false)
		} else if f.lines == 1 && bytes.Contains(line, procStartPrefix) {
			s.boundary(f, line, true)
		}
	}
	return nil
}

// boundary tells the resource sampler when the tap says a process started
// or ended.
func (s *spooler) boundary(f *spoolFile, line []byte, start bool) {
	if s.res == nil {
		return
	}
	var r struct {
		T int64 `json:"t"`
	}
	if json.Unmarshal(line, &r) != nil || r.T == 0 {
		return
	}
	if start {
		s.res.tapStarted(f.pid, f.name, time.Unix(0, r.T).UTC())
	} else {
		s.res.tapEnded(f.pid, f.name, time.Unix(0, r.T).UTC())
	}
}

// flush forwards what is left of a file and finishes it if its process
// has exited: the resource sampler calls it when its poll finds a tapped
// process gone, before the spool events about it arrive.
func (s *spooler) flush(name string) error {
	if err := s.read(name); err != nil {
		return err
	}
	if f := s.files[name]; f != nil && !s.alive(f) {
		return s.finish(f)
	}
	return nil
}

// finish reports a file as complete, then deletes it. The resource
// sampler's execution of the process goes first.
func (s *spooler) finish(f *spoolFile) error {
	if s.res != nil {
		if err := s.res.tapFinished(f.pid, f.name); err != nil {
			return err
		}
	}
	end := &SpoolEnd{
		Host: s.host, Unit: f.unit, PID: f.pid, File: f.name, Lines: f.lines,
		Truncated: s.procEnd && !f.sawEnd, Partial: len(f.partial),
	}
	if err := s.emit(Record{Kind: KindSpoolEnd, SpoolEnd: end}); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, f.name))
	delete(s.files, f.name)
	return nil
}

// alive says whether the process that created a file still runs. A process
// with the same pid that started after the file was created is another one;
// the slack covers the boot time's one-second resolution.
func (s *spooler) alive(f *spoolFile) bool {
	start := procStart(s.procRoot, f.pid)
	return !start.IsZero() && !start.After(f.start.Add(2*time.Second))
}

// parseSpoolName reads "<unit with / as ->-<pid>-<start ns>.jsonl". The unit
// number follows the last dash of the unit part; a tap that had no unit name
// writes "unknown".
func parseSpoolName(name string) (unitName string, pid int, start time.Time, ok bool) {
	base := strings.TrimSuffix(name, spoolSuffix)
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return "", 0, time.Time{}, false
	}
	ns, err := strconv.ParseInt(base[i+1:], 10, 64)
	if err != nil {
		return "", 0, time.Time{}, false
	}
	base = base[:i]
	i = strings.LastIndexByte(base, '-')
	if i < 0 {
		return "", 0, time.Time{}, false
	}
	pid, err = strconv.Atoi(base[i+1:])
	if err != nil || pid <= 0 {
		return "", 0, time.Time{}, false
	}
	unitName = base[:i]
	if j := strings.LastIndexByte(unitName, '-'); j > 0 {
		unitName = unitName[:j] + "/" + unitName[j+1:]
	}
	return unitName, pid, time.Unix(0, ns).UTC(), true
}
