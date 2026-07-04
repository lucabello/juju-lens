package recording

import (
	"testing"
	"time"
)

func TestParseDebugLogLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		ok      bool
		unit    string
		entity  string
		level   string
		module  string
		message string
		traceID string
	}{
		{
			name:    "unit hook line",
			line:    `unit-loki-0: 2026-07-04 14:04:24.406 INFO juju.worker.uniter.operation ran "update-status" hook`,
			ok:      true,
			unit:    "loki/0",
			entity:  "unit-loki-0",
			level:   "INFO",
			module:  "juju.worker.uniter.operation",
			message: `ran "update-status" hook`,
		},
		{
			name:    "multi-dash app name",
			line:    `unit-nova-compute-3: 2026-07-04 14:04:24.406 WARNING unit.nova-compute/3.update-status something`,
			ok:      true,
			unit:    "nova-compute/3",
			entity:  "unit-nova-compute-3",
			level:   "WARNING",
			module:  "unit.nova-compute/3.update-status",
			message: "something",
		},
		{
			name:   "non-unit entity has no unit",
			line:   `machine-0: 2026-07-04 14:05:10.752 INFO juju.worker.machiner started`,
			ok:     true,
			unit:   "",
			entity: "machine-0",
			level:  "INFO",
			module: "juju.worker.machiner",
		},
		{
			name:    "inline trace id is extracted",
			line:    `unit-loki-0: 2026-07-04 14:04:24.406 DEBUG charm handling event trace-id=00112233445566778899aabbccddeeff done`,
			ok:      true,
			unit:    "loki/0",
			entity:  "unit-loki-0",
			level:   "DEBUG",
			module:  "charm",
			traceID: "00112233445566778899aabbccddeeff",
		},
		{name: "blank line", line: "", ok: false},
		{name: "no entity separator", line: "not a log line", ok: false},
		{name: "garbage timestamp", line: "unit-loki-0: not a timestamp here really", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, ok := ParseDebugLogLine(c.line, "cos-lite")
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if rec.Model != "cos-lite" {
				t.Errorf("model = %q, want cos-lite", rec.Model)
			}
			if rec.Unit != c.unit {
				t.Errorf("unit = %q, want %q", rec.Unit, c.unit)
			}
			if rec.Entity != c.entity {
				t.Errorf("entity = %q, want %q", rec.Entity, c.entity)
			}
			if rec.Level != c.level {
				t.Errorf("level = %q, want %q", rec.Level, c.level)
			}
			if c.module != "" && rec.Module != c.module {
				t.Errorf("module = %q, want %q", rec.Module, c.module)
			}
			if c.message != "" && rec.Message != c.message {
				t.Errorf("message = %q, want %q", rec.Message, c.message)
			}
			if rec.TraceID != c.traceID {
				t.Errorf("trace-id = %q, want %q", rec.TraceID, c.traceID)
			}
			want := time.Date(2026, 7, 4, 14, 4, 24, 406_000_000, time.UTC)
			if c.entity == "machine-0" {
				want = time.Date(2026, 7, 4, 14, 5, 10, 752_000_000, time.UTC)
			}
			if !rec.Ts.Equal(want) {
				t.Errorf("ts = %s, want %s", rec.Ts, want)
			}
		})
	}
}
