package juju

import "testing"

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
