package cli

import (
	"strings"
	"testing"
)

func TestParseAttach(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "", want: "host"},
		{in: "host", want: "host"},
		{in: "host,host", want: "host"},
		{in: " host ", want: "host"},
		{in: "none", want: "none"},
		{in: "unit", want: "unit"},
		{in: "unit,host", want: "host,unit"},
		{in: "host,unit,unit", want: "host,unit"},
		{in: "none,host", wantErr: "can't be combined"},
		{in: "unit,none", wantErr: "can't be combined"},
		{in: "local", wantErr: "replaced by --attach host"},
		{in: "ssh", wantErr: "replaced by --attach host"},
		{in: "host,", wantErr: "want host, unit, none"},
		{in: "kubectl-debug", wantErr: "want host, unit, none"},
	} {
		got, err := parseAttach(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseAttach(%q) error = %v, want one containing %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("parseAttach(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
