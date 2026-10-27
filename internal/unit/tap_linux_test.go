//go:build linux

package unit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the real module in a Python interpreter, loaded the way a
// charm loads it: by the .pth the loader installs in site-packages.

type tapEnv struct {
	site  string
	spool string
	l     *tapLoader
}

func newTapEnv(t *testing.T, features []string) *tapEnv {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	agentsDir, site, _ := charmFixture(t)
	l := newTapLoader(agentsDir, t.TempDir(), os.Geteuid(), features, t.Logf)
	if err := l.setup(); err != nil {
		t.Fatal(err)
	}
	l.ensure()
	return &tapEnv{site: site, spool: l.spool, l: l}
}

// hook runs code as a hook process would, with the venv's .pth processed.
func (e *tapEnv) hook(t *testing.T, hookEnv bool, code string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command("python3", "-c",
		"import site, sys; site.addsitedir(sys.argv[1]); exec(sys.argv[2])", e.site, code)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "JUJU_") && !strings.HasPrefix(kv, "PYTHON") {
			env = append(env, kv)
		}
	}
	env = append(env, "JUJU_LENS_TAP_DIR="+e.spool, "PYTHONDONTWRITEBYTECODE=1")
	if hookEnv {
		env = append(env, "JUJU_DISPATCH_PATH=hooks/config-changed", "JUJU_HOOK_NAME=config-changed",
			"JUJU_UNIT_NAME=otelcol/0", "JUJU_MODEL_NAME=cos")
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), exit
}

// records reads every record the tap wrote, by kind.
func (e *tapEnv) records(t *testing.T) map[string][]map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.spool, "*.jsonl"))
	out := map[string][]map[string]any{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			var r map[string]any
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("%s: %q: %v", f, sc.Text(), err)
			}
			out[r["k"].(string)] = append(out[r["k"].(string)], r)
		}
	}
	return out
}

func TestTapRecordsHookProcess(t *testing.T) {
	e := newTapEnv(t, []string{"proc"})
	if out, errs, code := e.hook(t, true, "print('ok')"); out != "ok\n" || errs != "" || code != 0 {
		t.Fatalf("hook printed %q, %q, exit %d", out, errs, code)
	}
	recs := e.records(t)
	if len(recs["proc_start"]) != 1 || len(recs["proc_end"]) != 1 {
		t.Fatalf("records = %v, want one proc_start and one proc_end", recs)
	}
	env := recs["proc_start"][0]["env"].(map[string]any)
	if env["hook_name"] != "config-changed" || env["unit_name"] != "otelcol/0" {
		t.Fatalf("proc_start env = %v", env)
	}
	if _, ok := recs["proc_end"][0]["exc"]; ok {
		t.Fatalf("clean exit recorded an exception: %v", recs["proc_end"][0])
	}
}

func TestTapRecordsUncaughtException(t *testing.T) {
	e := newTapEnv(t, []string{"proc"})
	if _, _, code := e.hook(t, true, "raise ValueError('boom')"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	end := e.records(t)["proc_end"]
	if len(end) != 1 || end[0]["exc"] != "ValueError" || end[0]["exc_msg"] != "boom" || end[0]["rc"] != float64(1) {
		t.Fatalf("proc_end = %v, want rc 1 and ValueError boom", end)
	}
}

// TestTapInert covers every condition that must keep the module from doing
// anything: the hook still runs and prints only its own output, and the
// spool gets no file.
func TestTapInert(t *testing.T) {
	for name, tc := range map[string]struct {
		features []string
		hookEnv  bool
		prepare  func(t *testing.T, e *tapEnv)
	}{
		"not a hook process": {features: []string{"proc"}},
		"no feature on":      {features: []string{}, hookEnv: true},
		"unknown feature":    {features: []string{"telepathy"}, hookEnv: true},
		"stale heartbeat": {features: []string{"proc"}, hookEnv: true, prepare: func(t *testing.T, e *tapEnv) {
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(filepath.Join(e.spool, spoolHeartbeat), old, old); err != nil {
				t.Fatal(err)
			}
		}},
		"spool readable by others": {features: []string{"proc"}, hookEnv: true, prepare: func(t *testing.T, e *tapEnv) {
			if err := os.Chmod(e.spool, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTapEnv(t, tc.features)
			if tc.prepare != nil {
				tc.prepare(t, e)
			}
			if out, errs, code := e.hook(t, tc.hookEnv, "print('ok')"); out != "ok\n" || errs != "" || code != 0 {
				t.Fatalf("hook printed %q, %q, exit %d", out, errs, code)
			}
			if recs := e.records(t); len(recs) != 0 {
				t.Fatalf("inert tap wrote %v", recs)
			}
		})
	}
}

// TestTapLeftoversInert runs hooks with what a probe that died before
// cleaning up leaves behind: the loader and the module, without the spool;
// and the loader alone, without the module.
func TestTapLeftoversInert(t *testing.T) {
	e := newTapEnv(t, []string{"proc"})
	if err := os.RemoveAll(e.spool); err != nil {
		t.Fatal(err)
	}
	if out, errs, code := e.hook(t, true, "print('ok')"); out != "ok\n" || errs != "" || code != 0 {
		t.Fatalf("with loader and module, no spool: hook printed %q, %q, exit %d", out, errs, code)
	}
	if err := os.Remove(filepath.Join(e.site, tapModuleName)); err != nil {
		t.Fatal(err)
	}
	if out, errs, code := e.hook(t, true, "print('ok')"); out != "ok\n" || errs != "" || code != 0 {
		t.Fatalf("with loader only: hook printed %q, %q, exit %d", out, errs, code)
	}
}

// TestTapSurvivesBrokenOps runs a hook against an ops whose internals don't
// look like what the tap patches: the hook runs as without the tap, and the
// tap says what it couldn't patch.
func TestTapSurvivesBrokenOps(t *testing.T) {
	for name, tc := range map[string]struct {
		framework string
		want      map[string]any
	}{
		"target missing": {
			framework: "class Framework:\n" +
				"    def _reemit(self): pass\n" +
				"    def observe(self, *a): pass\n" +
				"class EventBase:\n" +
				"    def defer(self): pass\n",
			want: map[string]any{"what": "patch target missing", "target": "Framework._emit"},
		},
		"patch raises": {
			framework: "class _Frozen(type):\n" +
				"    def __setattr__(cls, name, value): raise TypeError('frozen')\n" +
				"class Framework(metaclass=_Frozen):\n" +
				"    def _reemit(self): pass\n" +
				"    def _emit(self, event): pass\n" +
				"    def observe(self, *a): pass\n" +
				"class EventBase:\n" +
				"    def defer(self): pass\n",
			want: map[string]any{"what": "patch failed", "module": "ops.framework", "exc": "TypeError", "exc_msg": "frozen"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTapEnv(t, []string{"proc", "ops"})
			pkg := filepath.Join(e.site, "ops") // the fixture's charm venv has an empty ops package
			for file, body := range map[string]string{
				"__init__.py":  "__version__ = '0.0'\n",
				"framework.py": tc.framework,
			} {
				if err := os.WriteFile(filepath.Join(pkg, file), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code := "import ops.framework\nops.framework.Framework().observe(None, None)\nprint('ok')"
			if out, errs, exit := e.hook(t, true, code); out != "ok\n" || errs != "" || exit != 0 {
				t.Fatalf("hook printed %q, %q, exit %d", out, errs, exit)
			}
			recs := e.records(t)
			for _, w := range recs["tap_warn"] {
				match := true
				for k, v := range tc.want {
					match = match && w[k] == v
				}
				if match {
					return
				}
			}
			t.Fatalf("tap_warn records = %v, want one with %v", recs["tap_warn"], tc.want)
		})
	}
}
