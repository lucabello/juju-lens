package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/probebin"
)

func TestGoArchOf(t *testing.T) {
	for uname, want := range map[string]string{"x86_64\n": "amd64", "aarch64": "arm64", "arm64": "arm64"} {
		if got, err := goArchOf(uname); err != nil || got != want {
			t.Errorf("goArchOf(%q) = %q, %v; want %q", uname, got, err, want)
		}
	}
	if _, err := goArchOf("riscv64"); err == nil || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("riscv64: %v, want an unsupported-architecture error naming it", err)
	}
}

// TestUploadScript: the script announces the architecture only when asked, so
// the host probe's binary stdout isn't preceded by a text line, receives the
// probe into a .part file and renames it, and runs the probe as a child.
func TestUploadScript(t *testing.T) {
	unit := uploadScript("/tmp/p", "sudo /tmp/p --unit", true)
	host := uploadScript("/tmp/p", "sudo /tmp/p --unlink-self --model-uuid abc", false)
	if !strings.HasPrefix(unit, `echo "juju-lens-arch $(uname -m)"; cat > /tmp/p.part`) || !strings.HasSuffix(unit, "mv /tmp/p.part /tmp/p && sudo /tmp/p --unit") {
		t.Errorf("unit script %q", unit)
	}
	if strings.Contains(host, "juju-lens-arch") || !strings.HasPrefix(host, "cat > /tmp/p.part") || strings.Contains(host, "exec ") {
		t.Errorf("host script %q", host)
	}
	if got := shellQuote("a b'c"); got != `'a b'\''c'` {
		t.Errorf("shellQuote = %s", got)
	}
	if got := shellQuote("--model-uuid"); got != "--model-uuid" {
		t.Errorf("shellQuote of a plain word = %s", got)
	}
}

// TestLocalHostProbeIsJujuLens: the host probe on this machine is juju-lens
// itself, run as `juju-lens probe`.
func TestLocalHostProbeIsJujuLens(t *testing.T) {
	cmd, target, err := buildProbeCommand(recordFlags{}, probeFilter{modelUUID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if cmd.Path != self || len(cmd.Args) < 2 || cmd.Args[1] != "probe" || target != "localhost" {
		t.Errorf("command %v, target %q; want %s probe …", cmd.Args, target, self)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--model-uuid abc") {
		t.Errorf("probe args lost: %v", cmd.Args)
	}
}

// TestOpenProbe: a build with the embedded probes opens the one for the
// architecture asked, and a build without them says how to get one.
func TestOpenProbe(t *testing.T) {
	if !probebin.Embedded {
		if err := probeAvailable(); err == nil || !strings.Contains(err.Error(), "just build") {
			t.Errorf("without the embedded probes: %v", err)
		}
		return
	}
	if err := probeAvailable(); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		rc, err := openProbe(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		head := make([]byte, 4)
		_, _ = io.ReadFull(rc, head)
		rc.Close()
		if string(head) != "\x7fELF" {
			t.Errorf("%s: probe starts %q, want an ELF file", arch, head)
		}
	}
}
