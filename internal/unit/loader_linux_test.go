//go:build linux

package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// charmFixture makes an agents directory with two charms: one with a venv
// whose site-packages takes a .pth, one whose venv directory goes on
// PYTHONPATH and so takes a sitecustomize.
func charmFixture(t *testing.T) (agentsDir, site, flat string) {
	t.Helper()
	agentsDir = t.TempDir()
	site = filepath.Join(agentsDir, "unit-otelcol-0", "charm", "venv", "lib", "python3.12", "site-packages")
	flat = filepath.Join(agentsDir, "unit-legacy-1", "charm", "venv")
	for path, body := range map[string]string{
		filepath.Join(site, "ops", "__init__.py"):                 "",
		filepath.Join(site, "__pycache__", "six.cpython-312.pyc"): "pyc",
		filepath.Join(site, "six.py"):                             "x = 1\n",
		filepath.Join(flat, "charms", "lib.py"):                   "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return agentsDir, site, flat
}

// snapshot maps every path under root to its type, mode and content hash.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			v += " " + hex.EncodeToString(sum[:])
		}
		out[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newTestLoader(t *testing.T, agentsDir string) *tapLoader {
	t.Helper()
	l := newTapLoader(agentsDir, t.TempDir(), os.Geteuid(), DefaultFeatures, t.Logf)
	if err := l.setup(); err != nil {
		t.Fatal(err)
	}
	return l
}

func events(ls []Loader) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Event+" "+l.Unit+" "+l.Loader)
	}
	return out
}

func TestTapRemoveLeavesCharmsAsFound(t *testing.T) {
	agentsDir, site, flat := charmFixture(t)
	before := snapshot(t, agentsDir)
	l := newTestLoader(t, agentsDir)

	got := events(l.ensure())
	want := []string{"installed legacy/1 sitecustomize.py", "installed otelcol/0 _juju_lens_tap.pth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first ensure = %q, want %q", got, want)
	}
	if got := l.ensure(); len(got) != 0 {
		t.Fatalf("second ensure reported %q, want nothing", events(got))
	}
	// Hooks ran and Python cached bytecode for the tap and the loader.
	for _, p := range []string{
		filepath.Join(site, "__pycache__", "_juju_lens_tap.cpython-312.pyc"),
		filepath.Join(flat, "__pycache__", "_juju_lens_tap.cpython-312.pyc"),
		filepath.Join(flat, "__pycache__", "sitecustomize.cpython-312.pyc"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("pyc"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got = events(l.remove())
	want = []string{"removed legacy/1 sitecustomize.py", "removed otelcol/0 _juju_lens_tap.pth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remove = %q, want %q", got, want)
	}
	if after := snapshot(t, agentsDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("charms differ after remove:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := os.Lstat(l.spool); !os.IsNotExist(err) {
		t.Fatalf("spool still there after remove: %v", err)
	}
}

func TestTapReinstalledAfterRefresh(t *testing.T) {
	agentsDir, site, _ := charmFixture(t)
	l := newTestLoader(t, agentsDir)
	l.ensure()

	// A refresh on a machine replaces the venv.
	if err := os.RemoveAll(site); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	refreshed := time.Now()
	// Installing writes into the directory too; dir_changed must still say
	// when the refresh replaced it.
	time.Sleep(50 * time.Millisecond)
	got := l.ensure()
	if len(got) != 1 || got[0].Event != LoaderReinstalled || got[0].Dir != site {
		t.Fatalf("ensure after refresh = %+v, want one reinstalled in %s", got, site)
	}
	if d := got[0].DirChanged.Sub(refreshed); d < -2*time.Second || d > 25*time.Millisecond {
		t.Fatalf("dir_changed = %s, want about %s", got[0].DirChanged, refreshed)
	}
	if b, err := os.ReadFile(filepath.Join(site, tapPthName)); err != nil || string(b) != string(tapPth) {
		t.Fatalf("loader not back: %q, %v", b, err)
	}

	// Edited in place, same size: still noticed.
	mod := filepath.Join(site, tapModuleName)
	b, _ := os.ReadFile(mod)
	b[0] ^= 1
	if err := os.WriteFile(mod, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := l.ensure(); len(got) != 1 || got[0].Event != LoaderReinstalled {
		t.Fatalf("ensure after edit = %+v, want reinstalled", got)
	}
}

func TestTapLeavesCharmSitecustomizeAlone(t *testing.T) {
	agentsDir, _, flat := charmFixture(t)
	own := filepath.Join(flat, sitecustomizeName)
	if err := os.WriteFile(own, []byte("import os\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, flat)
	l := newTestLoader(t, agentsDir)
	var skipped []Loader
	for _, e := range l.ensure() {
		if e.Event == LoaderSkipped {
			skipped = append(skipped, e)
		}
	}
	if len(skipped) != 1 || skipped[0].Unit != "legacy/1" {
		t.Fatalf("skipped = %+v, want legacy/1", skipped)
	}
	if got := l.ensure(); len(got) != 0 {
		t.Fatalf("skip reported again: %+v", got)
	}
	l.remove()
	if after := snapshot(t, flat); !reflect.DeepEqual(after, before) {
		t.Fatalf("charm with its own sitecustomize changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestTapSkipsCharmWithoutVenv(t *testing.T) {
	agentsDir, _, _ := charmFixture(t)
	charm := filepath.Join(agentsDir, "unit-bash-2", "charm")
	if err := os.MkdirAll(filepath.Join(charm, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, charm)
	l := newTestLoader(t, agentsDir)
	var skipped []Loader
	for _, e := range l.ensure() {
		if e.Event == LoaderSkipped {
			skipped = append(skipped, e)
		}
	}
	if len(skipped) != 1 || skipped[0].Unit != "bash/2" || skipped[0].Reason != ReasonNoVenv {
		t.Fatalf("skipped = %+v, want bash/2 with no venv", skipped)
	}
	if got := l.ensure(); len(got) != 0 {
		t.Fatalf("skip reported again: %+v", got)
	}
	if got := events(l.remove()); len(got) != 2 {
		t.Fatalf("remove = %q, want the two charms with a venv", got)
	}
	if after := snapshot(t, charm); !reflect.DeepEqual(after, before) {
		t.Fatalf("charm without venv changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestTapSpoolRefusesForeignPath(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(tmp, SpoolName)); err != nil {
		t.Fatal(err)
	}
	l := newTapLoader(t.TempDir(), tmp, os.Geteuid(), DefaultFeatures, t.Logf)
	if err := l.setup(); err == nil {
		t.Fatal("setup accepted a symlink as the spool")
	}
}
