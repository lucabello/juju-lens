//go:build linux

package unit

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The tap runs inside the charm's Python, loaded at interpreter start: by a
// .pth file in a virtualenv's site-packages, or, for charms that put their
// venv directory on PYTHONPATH (no .pth processing there), by a
// sitecustomize module in that directory.
var (
	//go:embed pytap/_juju_lens_tap.py
	tapModule []byte
	//go:embed pytap/sitecustomize.py
	tapSitecustomize []byte
	// A .pth runs lines that start with "import". This one can't print a
	// traceback into the charm's output, even if the module has gone.
	//go:embed pytap/_juju_lens_tap.pth
	tapPth []byte
)

const (
	tapModuleName     = "_juju_lens_tap.py"
	tapPthName        = "_juju_lens_tap.pth"
	sitecustomizeName = "sitecustomize.py"

	// SpoolName is the spool directory in TmpDir; it must match SPOOL in
	// the module.
	SpoolName = "juju-lens-tap"
	// spoolHeartbeat is touched every tick; the module stays inert unless
	// it is fresh, so a probe that died without cleaning up leaves nothing
	// active behind.
	spoolHeartbeat = "alive"
	// spoolConfig holds the features the module runs, read once by each
	// process as it starts.
	spoolConfig = "config.json"
)

// tapTarget is one place the tap is installed: a directory on a charm
// Python's path, and the file that loads the module from it. A charm with no
// venv has a target with no loader: its dir is the charm directory, and the
// tap can't go there.
type tapTarget struct {
	unit       string
	dir        string
	loader     string
	loaderBody []byte
}

// tapState is what the loader knows about one target directory.
type tapState struct {
	installed bool // installed at least once by this probe
	skipped   bool // reported as not installable
	hadCache  bool // __pycache__ existed before the first install
	// After a write or a full check, the files' identity: while it holds,
	// the files are ours and need no reading.
	modID, loaderID fileID
}

type fileID struct {
	ino   uint64
	size  int64
	mtime int64
}

func statID(path string) (fileID, bool) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fileID{}, false
	}
	return fileID{ino: st.Ino, size: st.Size, mtime: st.Mtim.Nano()}, true
}

// tapLoader keeps the tap installed in every charm on this host for as long
// as the probe runs: a refresh replaces the venv on a machine, and ensure
// puts the tap back.
type tapLoader struct {
	agentsDir string
	spool     string
	uid       int // the agent's, which hook processes run as
	features  []string
	logf      func(string, ...any)
	state     map[string]*tapState // by target dir
}

func newTapLoader(agentsDir, tmpDir string, uid int, features []string, logf func(string, ...any)) *tapLoader {
	return &tapLoader{
		agentsDir: agentsDir, spool: filepath.Join(tmpDir, SpoolName),
		uid: uid, features: features, logf: logf, state: map[string]*tapState{},
	}
}

// setup creates the spool directory for the agent's uid with mode 0700,
// writes the config, and touches the heartbeat. A spool left by a probe
// that died is reused; anything else in its place is refused.
func (l *tapLoader) setup() error {
	if err := os.Mkdir(l.spool, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(l.spool, &st); err != nil {
		return err
	}
	euid := os.Geteuid()
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || (int(st.Uid) != euid && int(st.Uid) != l.uid) {
		return fmt.Errorf("%s exists and is not a directory of ours", l.spool)
	}
	if int(st.Uid) != l.uid {
		if err := os.Lchown(l.spool, l.uid, -1); err != nil {
			return fmt.Errorf("handing %s to uid %d: %w", l.spool, l.uid, err)
		}
	}
	if err := os.Chmod(l.spool, 0o700); err != nil {
		return err
	}
	cfg, _ := json.Marshal(struct {
		Features []string `json:"features"`
	}{l.features})
	p := filepath.Join(l.spool, spoolConfig)
	if _, err := writeIfDifferent(p, append(cfg, '\n')); err != nil {
		return err
	}
	if err := l.handOver(p); err != nil {
		return err
	}
	return l.heartbeat()
}

// heartbeat keeps the module active for the next HEARTBEAT_MAX_AGE.
func (l *tapLoader) heartbeat() error {
	p := filepath.Join(l.spool, spoolHeartbeat)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return nil
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return err
	}
	return l.handOver(p)
}

// handOver gives a spool file to the agent's uid, so hook processes that
// don't run as root can read it.
func (l *tapLoader) handOver(path string) error {
	if l.uid == os.Geteuid() {
		return nil
	}
	return os.Lchown(path, l.uid, -1)
}

// targets finds where each charm on this host loads Python modules from.
func (l *tapLoader) targets() []tapTarget {
	charms, _ := filepath.Glob(filepath.Join(l.agentsDir, "unit-*", "charm"))
	var out []tapTarget
	for _, charm := range charms {
		unit := unitFromTag(filepath.Base(filepath.Dir(charm)))
		if unit == "" {
			continue
		}
		sites, _ := filepath.Glob(filepath.Join(charm, "venv", "lib", "python3*", "site-packages"))
		if len(sites) > 0 {
			for _, s := range sites {
				out = append(out, tapTarget{unit: unit, dir: s, loader: tapPthName, loaderBody: tapPth})
			}
			continue
		}
		venv := filepath.Join(charm, "venv")
		if st, err := os.Stat(venv); err == nil && st.IsDir() {
			out = append(out, tapTarget{unit: unit, dir: venv, loader: sitecustomizeName, loaderBody: tapSitecustomize})
			continue
		}
		out = append(out, tapTarget{unit: unit, dir: charm})
	}
	return out
}

// ensure installs the tap where it is missing or not ours any more, and
// reports what it did.
func (l *tapLoader) ensure() []Loader {
	var events []Loader
	for _, t := range l.targets() {
		st := l.state[t.dir]
		if st == nil {
			st = &tapState{}
			l.state[t.dir] = st
		}
		if t.loader == "" {
			if !st.skipped {
				st.skipped = true
				events = append(events, Loader{Event: LoaderSkipped, Unit: t.unit, Dir: t.dir, Reason: ReasonNoVenv})
			}
			continue
		}
		modPath, loaderPath := filepath.Join(t.dir, tapModuleName), filepath.Join(t.dir, t.loader)
		if st.installed {
			m, ok1 := statID(modPath)
			ld, ok2 := statID(loaderPath)
			if ok1 && ok2 && m == st.modID && ld == st.loaderID {
				continue
			}
		}
		if cur, err := os.ReadFile(loaderPath); err == nil && !bytes.Equal(cur, t.loaderBody) {
			// A sitecustomize of the charm's own: leave it alone.
			if !st.skipped {
				st.skipped = true
				events = append(events, Loader{Event: LoaderSkipped, Unit: t.unit, Dir: t.dir, Loader: t.loader,
					Reason: "the charm has its own " + t.loader})
			}
			continue
		}
		if !st.installed {
			_, err := os.Lstat(filepath.Join(t.dir, "__pycache__"))
			st.hadCache = err == nil
		}
		// Before writing, which changes it too.
		changedAt := dirChanged(t.dir)
		// Module first, loader second: a loader never points at nothing.
		changed, err := writeIfDifferent(modPath, tapModule)
		if err == nil {
			var c bool
			c, err = writeIfDifferent(loaderPath, t.loaderBody)
			changed = changed || c
		}
		if err != nil {
			if !st.skipped {
				st.skipped = true
				events = append(events, Loader{Event: LoaderSkipped, Unit: t.unit, Dir: t.dir, Loader: t.loader,
					Reason: err.Error()})
			}
			continue
		}
		st.modID, _ = statID(modPath)
		st.loaderID, _ = statID(loaderPath)
		switch {
		case !st.installed:
			st.installed, st.skipped = true, false
			events = append(events, Loader{Event: LoaderInstalled, Unit: t.unit, Dir: t.dir, Loader: t.loader,
				DirChanged: changedAt})
		case changed:
			st.skipped = false
			events = append(events, Loader{Event: LoaderReinstalled, Unit: t.unit, Dir: t.dir, Loader: t.loader,
				DirChanged: changedAt})
		}
	}
	return events
}

// dirChanged is when a directory's inode last changed: for a venv that a
// refresh replaced, about when the new one appeared. Read it before writing
// into the directory.
func dirChanged(dir string) time.Time {
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Unix()).UTC()
}

// remove uninstalls the tap from every charm, leaving each directory as it
// found it, and deletes the spool. Python processes already running keep
// the module loaded, but have nowhere to write.
func (l *tapLoader) remove() []Loader {
	byDir := map[string]tapTarget{}
	for _, t := range l.targets() {
		byDir[t.dir] = t
	}
	dirs := make([]string, 0, len(l.state))
	for d := range l.state {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var events []Loader
	for _, d := range dirs {
		st := l.state[d]
		t, ok := byDir[d]
		if !ok || !st.installed {
			continue
		}
		// Loader first, module second: a loader never points at nothing.
		removed := removeIfOurs(filepath.Join(d, t.loader), t.loaderBody)
		removePyc(d, t.loader)
		removed = removeIfOurs(filepath.Join(d, tapModuleName), tapModule) || removed
		removePyc(d, tapModuleName)
		if !st.hadCache {
			_ = os.Remove(filepath.Join(d, "__pycache__")) // only if empty
		}
		if removed {
			events = append(events, Loader{Event: LoaderRemoved, Unit: t.unit, Dir: d, Loader: t.loader})
		}
	}
	_ = os.RemoveAll(l.spool)
	return events
}

func removeIfOurs(path string, body []byte) bool {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, body) {
		return os.Remove(path) == nil
	}
	return false
}

// writeIfDifferent writes body to path, through a temporary file and a
// rename, unless path already holds exactly body.
func writeIfDifferent(path string, body []byte) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, body) {
		return false, nil
	}
	tmp := path + ".juju-lens.tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// removePyc deletes the bytecode Python cached for a module it imported.
func removePyc(dir, file string) {
	if !strings.HasSuffix(file, ".py") {
		return
	}
	pycs, _ := filepath.Glob(filepath.Join(dir, "__pycache__", strings.TrimSuffix(file, ".py")+".*.pyc"))
	for _, f := range pycs {
		_ = os.Remove(f)
	}
}
