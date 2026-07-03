package juju

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStringifyRoundTripsCommonTypes(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"127.0.0.1:4317", "127.0.0.1:4317"},
		{true, "true"},
		{false, "false"},
		{float64(1), "1"},
		{float64(0.1), "0.1"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := stringify(c.in); got != c.want {
			t.Errorf("stringify(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOTELKeysStableOrder(t *testing.T) {
	// 'enabled' must be last so we finish configuring before Juju starts
	// exporting.
	if OTELKeys[len(OTELKeys)-1] != "open-telemetry-enabled" {
		t.Fatalf("enabled must be last, got order: %v", OTELKeys)
	}
	// endpoint must precede enabled for the same reason.
	var enabledAt, endpointAt int
	for i, k := range OTELKeys {
		switch k {
		case "open-telemetry-enabled":
			enabledAt = i
		case "open-telemetry-endpoint":
			endpointAt = i
		}
	}
	if endpointAt > enabledAt {
		t.Fatalf("endpoint must precede enabled, got %v", OTELKeys)
	}
}

// TestClientUsesLongControllerFlagAndOmitsWhenEmpty proves the wrapper
// invokes `juju --controller <name>` (long form, as juju's help displays
// it) and omits the flag entirely when Controller is empty, which lets
// juju's own default of "the active controller" take over.
func TestClientUsesLongControllerFlagAndOmitsWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	shim := dir + "/juju"
	argFile := dir + "/args"
	if err := writeArgvShim(shim, argFile); err != nil {
		t.Skipf("cannot write shim: %v", err)
	}
	if _, err := exec.Command(shim, "--version").Output(); err != nil {
		t.Skipf("shim not executable in this environment: %v", err)
	}

	t.Run("with controller", func(t *testing.T) {
		_ = os.Remove(argFile)
		c := &Client{Bin: shim, Controller: "prod"}
		if _, err := c.ReadOTELConfig(); err != nil {
			t.Fatalf("ReadOTELConfig: %v", err)
		}
		got := readFile(t, argFile)
		if !strings.Contains(got, "--controller prod") {
			t.Errorf("shim argv missing --controller prod; got: %q", got)
		}
	})
	t.Run("without controller uses juju's active default", func(t *testing.T) {
		_ = os.Remove(argFile)
		c := &Client{Bin: shim}
		if _, err := c.ReadOTELConfig(); err != nil {
			t.Fatalf("ReadOTELConfig: %v", err)
		}
		got := readFile(t, argFile)
		if strings.Contains(got, "--controller") {
			t.Errorf("shim argv must omit --controller when empty; got: %q", got)
		}
	})
}
