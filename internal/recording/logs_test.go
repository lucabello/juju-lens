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

func TestParseK8sLogLine(t *testing.T) {
	// Real shape from `kubectl logs --timestamps` against a Pebble-managed
	// workload container: kubectl's own RFC3339Nano prefix, then Pebble's own
	// near-identical echo of it, then the "[service] logline" Pebble wraps
	// the service's stdout with.
	line := `2026-07-04T19:13:25.277747005Z 2026-07-04T19:13:25.277Z [grafana] level=info msg="ready" trace-id=00112233445566778899aabbccddeeff`
	rec, ok := ParseK8sLogLine(line, "cos-lite", "grafana-0", "grafana")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if rec.Source != "k8s" {
		t.Errorf("source = %q, want k8s", rec.Source)
	}
	if rec.Unit != "grafana/0" {
		t.Errorf("unit = %q, want grafana/0", rec.Unit)
	}
	if rec.Entity != "grafana-0" {
		t.Errorf("entity = %q, want grafana-0", rec.Entity)
	}
	if rec.Module != "grafana" {
		t.Errorf("module = %q, want grafana", rec.Module)
	}
	// Both the kubectl and the Pebble timestamps are gone; only the
	// "[service] logline" Pebble wrapper remains.
	if rec.Message != `[grafana] level=info msg="ready" trace-id=00112233445566778899aabbccddeeff` {
		t.Errorf("message = %q", rec.Message)
	}
	if rec.TraceID != "00112233445566778899aabbccddeeff" {
		t.Errorf("trace-id = %q", rec.TraceID)
	}
	want := time.Date(2026, 7, 4, 19, 13, 25, 277747005, time.UTC)
	if !rec.Ts.Equal(want) {
		t.Errorf("ts = %s, want %s", rec.Ts, want)
	}
	// Multi-word app name still resolves.
	if rec, _ := ParseK8sLogLine(line, "cos-lite", "prometheus-k8s-0", "prometheus"); rec.Unit != "prometheus-k8s/0" {
		t.Errorf("unit = %q, want prometheus-k8s/0", rec.Unit)
	}
	// A container not running under Pebble has no second timestamp to strip:
	// the (non-timestamp) rest of the line is left untouched.
	if rec, _ := ParseK8sLogLine(`2026-07-04T19:13:25.277Z plain stdout, no pebble wrapper`, "m", "p-0", "c"); rec.Message != "plain stdout, no pebble wrapper" {
		t.Errorf("message = %q, want the line untouched past the kubectl timestamp", rec.Message)
	}
	if _, ok := ParseK8sLogLine("no-space-so-no-timestamp", "m", "p-0", "c"); ok {
		t.Error("expected ok = false for a line without a timestamp field")
	}
	if _, ok := ParseK8sLogLine("not-a-timestamp here", "m", "p-0", "c"); ok {
		t.Error("expected ok = false for an unparseable timestamp")
	}
}

func TestRedactK8sInlineTimestamp(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "prometheus logfmt ts=",
			msg:  `[prometheus] ts=2026-07-04T19:13:25.277Z caller=main.go:123 level=info msg="ready"`,
			want: `[prometheus] caller=main.go:123 level=info msg="ready"`,
		},
		{
			name: "logfmt time= quoted",
			msg:  `[loki] time="2026-07-04T19:13:25Z" level=info msg=ready`,
			want: `[loki] level=info msg=ready`,
		},
		{
			name: "avalanche bare clock",
			msg:  `[avalanche] 19:13:25 starting up`,
			want: `[avalanche] starting up`,
		},
		{
			name: "go stdlib log date+time",
			msg:  `[worker] 2026/07/04 19:13:25 listening on :8080`,
			want: `[worker] listening on :8080`,
		},
		{
			name: "bracketed ISO timestamp",
			msg:  `[nginx] [2026-07-04T19:13:25.277Z] GET / 200`,
			want: `[nginx] GET / 200`,
		},
		{
			name: "bracketed bare clock",
			msg:  `[app] [19:13:25] boot complete`,
			want: `[app] boot complete`,
		},
		{
			name: "bare ISO8601, no component tag",
			msg:  `2026-07-04T19:13:25.277Z ready`,
			want: `ready`,
		},
		{
			name: "syslog month-day time",
			msg:  `[sshd] Jul  4 19:13:25 sshd started`,
			want: `[sshd] sshd started`,
		},
		{
			name: "no recognizable timestamp: untouched",
			msg:  `[grafana] level=info msg="ready"`,
			want: `[grafana] level=info msg="ready"`,
		},
		{
			name: "clock mentioned mid-message: untouched, not stripped",
			msg:  `[app] retry scheduled for 19:13:25 UTC`,
			want: `[app] retry scheduled for 19:13:25 UTC`,
		},
		{
			name: "whole logline is the timestamp: left alone rather than emptied",
			msg:  `[app] 19:13:25`,
			want: `[app] 19:13:25`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactK8sInlineTimestamp(c.msg); got != c.want {
				t.Errorf("RedactK8sInlineTimestamp(%q) = %q, want %q", c.msg, got, c.want)
			}
		})
	}
}

func TestParseJournalLine(t *testing.T) {
	// A trimmed real `journalctl -o json` object.
	line := `{"__REALTIME_TIMESTAMP":"1783192466043395","PRIORITY":"6","SYSLOG_IDENTIFIER":"systemd","_SYSTEMD_UNIT":"init.scope","MESSAGE":"Started user@1000.service."}`
	rec, ok := ParseJournalLine(line, "otelcol-scrape-test", "machine-0")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if rec.Source != "journal" {
		t.Errorf("source = %q, want journal", rec.Source)
	}
	if rec.Entity != "machine-0" {
		t.Errorf("entity = %q, want machine-0", rec.Entity)
	}
	if rec.Unit != "" {
		t.Errorf("unit = %q, want empty", rec.Unit)
	}
	if rec.Level != "INFO" {
		t.Errorf("level = %q, want INFO (PRIORITY 6)", rec.Level)
	}
	if rec.Module != "systemd" {
		t.Errorf("module = %q, want systemd", rec.Module)
	}
	if rec.Message != "Started user@1000.service." {
		t.Errorf("message = %q", rec.Message)
	}
	want := time.UnixMicro(1783192466043395).UTC()
	if !rec.Ts.Equal(want) {
		t.Errorf("ts = %s, want %s", rec.Ts, want)
	}
	// A binary MESSAGE (journald emits an array) is skipped, not misparsed.
	if _, ok := ParseJournalLine(`{"__REALTIME_TIMESTAMP":"1","MESSAGE":[1,2,3]}`, "m", "h"); ok {
		t.Error("expected ok = false for an array MESSAGE")
	}
	if _, ok := ParseJournalLine(`not json`, "m", "h"); ok {
		t.Error("expected ok = false for non-JSON")
	}
}

func TestParseLogLine(t *testing.T) {
	cases := []struct {
		rel    string
		line   string
		ok     bool
		source string
		model  string
	}{
		{
			rel:    "raw/juju/cos-lite/debug-log-2026-07-04T19.log",
			line:   `unit-loki-0: 2026-07-04 14:04:24.406 INFO juju.worker.uniter ran hook`,
			ok:     true,
			source: "debug-log",
			model:  "cos-lite",
		},
		{
			rel:    "raw/k8s/cos-lite/grafana-0/grafana/logs-2026-07-04T19.log",
			line:   `2026-07-04T19:13:25.277Z [grafana] ready`,
			ok:     true,
			source: "k8s",
			model:  "cos-lite",
		},
		{
			rel:    "raw/machine/otelcol-scrape-test/machine-0/journal-2026-07-04T19.log",
			line:   `{"__REALTIME_TIMESTAMP":"1783192466043395","MESSAGE":"hi"}`,
			ok:     true,
			source: "journal",
			model:  "otelcol-scrape-test",
		},
		{rel: "raw/unknown/m/f.log", line: "x", ok: false},
		{rel: "raw/k8s/cos-lite/grafana-0/logs.log", line: "x", ok: false}, // too shallow for k8s
		{rel: "notraw/juju/m/f.log", line: "x", ok: false},
	}
	for _, c := range cases {
		t.Run(c.rel, func(t *testing.T) {
			rec, ok := ParseLogLine(c.rel, c.line)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if rec.Source != c.source {
				t.Errorf("source = %q, want %q", rec.Source, c.source)
			}
			if rec.Model != c.model {
				t.Errorf("model = %q, want %q", rec.Model, c.model)
			}
		})
	}
}
