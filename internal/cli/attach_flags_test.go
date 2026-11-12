//go:build linux

package cli

import (
	"strings"
	"testing"
)

// With unit as the default, the flags that only mean something for host must
// say so, rather than being ignored.
func TestHostOnlyFlagsNeedHost(t *testing.T) {
	for _, args := range [][]string{
		{"record", "x", "--ssh-target", "controller/0"},
		{"record", "x", "--attach", "unit", "--pid", "4242"},
		{"watch", "--ssh-target", "controller/0"},
		{"record", "x", "--attach", "none", "--ssh-target", "controller/0"},
	} {
		_, err := runCLI(t, args...)
		if err == nil || !strings.Contains(err.Error(), "needs --attach host") {
			t.Errorf("%v: error = %v, want one saying the flag needs --attach host", args, err)
		}
	}
	for _, args := range [][]string{
		{"record", "x", "--attach", "none", "--profile=false"},
		{"record", "x", "--attach", "host", "--linger", "1s"},
	} {
		_, err := runCLI(t, args...)
		if err == nil || !strings.Contains(err.Error(), "needs --attach unit") {
			t.Errorf("%v: error = %v, want one saying the flag needs --attach unit", args, err)
		}
	}
}
