package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lucabello/juju-lens/internal/probebin"
	"github.com/lucabello/juju-lens/internal/probecmd"
	"github.com/lucabello/juju-lens/internal/unit"
	"github.com/spf13/cobra"
)

// The probe is part of juju-lens. Run on this host, it is `juju-lens probe`.
// Uploaded to units, and to the machine of --ssh-target, it is the static
// probe binary for their architecture that juju-lens carries inside itself
// (internal/probebin).

// newProbeCmd is `juju-lens probe`: the probe's own command line, which
// `record` runs on this host for --attach host. It is hidden: nothing else
// is meant to call it.
func newProbeCmd(bi BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:                "probe [flags]",
		Short:              "Run the probe (what record runs; see juju-lens probe --help)",
		Hidden:             true,
		DisableFlagParsing: true,
		Run: func(_ *cobra.Command, args []string) {
			probecmd.Main(bi.Version, args)
		},
	}
}

// errNoProbe is what a juju-lens built without the embedded probes says when
// asked to upload one.
var errNoProbe = errors.New("this juju-lens was built without the embedded probes, so it can't upload one; use a release build or `just build`")

// probeAvailable says early whether this build carries the probes to upload.
func probeAvailable() error {
	if !probebin.Embedded {
		return errNoProbe
	}
	return nil
}

// openProbe returns the probe to upload to a host of goarch ("amd64" or
// "arm64").
func openProbe(goarch string) (io.ReadCloser, error) {
	rc, _, err := probebin.Open(goarch)
	if err != nil {
		return nil, errNoProbe
	}
	return rc, nil
}

// goArchOf maps what `uname -m` prints to a Go architecture.
func goArchOf(uname string) (string, error) {
	switch strings.TrimSpace(uname) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("unsupported architecture %q: juju-lens carries probes for amd64 and arm64", strings.TrimSpace(uname))
}

// archPrefix starts the line an upload script prints with the host's
// architecture, so the recorder knows which probe to send.
const archPrefix = "juju-lens-arch "

// uploadScript is the remote command that, if announceArch, prints the architecture, receives
// the probe on stdin into a file of this recorder, and runs run (which names
// that file) as a child. The shell runs the probe as a child rather than
// exec'ing it: when the session is ended, Kubernetes kills the session's own
// process with SIGKILL, and as a child the probe outlives that, finds its
// stdout closed, and releases the host before it exits.
func uploadScript(path, run string, announceArch bool) string {
	echo := ""
	if announceArch {
		echo = fmt.Sprintf(`echo "%s$(uname -m)"; `, archPrefix)
	}
	return fmt.Sprintf(`%[3]scat > %[1]s.part && chmod 700 %[1]s.part && mv %[1]s.part %[1]s && %[2]s`, path, run, echo)
}

// uploadPath is where the probe is uploaded to on a host.
func uploadPath(runID string) string { return "/tmp/" + unit.UploadPrefix + runID }

// shellQuote quotes one word for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=,@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
