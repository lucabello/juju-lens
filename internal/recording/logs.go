package recording

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogRecord is one parsed line of a log stream. It is the "log observation" in
// the correlation model (VISION §7): a text line attributed to a unit and a
// time, joined to spans by span-id when present or by (unit, time-window)
// otherwise. The raw text lines remain the source of truth under raw/; the
// LogRecord is derived and rebuildable.
//
// Three sources feed it, all reduced to this one shape (see Source):
//   - "debug-log": `juju debug-log`, per model, under raw/juju/<model>/ (M3).
//   - "k8s": workload-container stdout via `kubectl logs`, per unit+container,
//     under raw/k8s/<model>/<pod>/<container>/ (M6).
//   - "journal": machine journald via `juju ssh … journalctl`, per host, under
//     raw/machine/<model>/<host>/ (M6).
type LogRecord struct {
	Ts      time.Time
	Model   string // model the log stream belongs to (from the raw path)
	Source  string // "debug-log" | "k8s" | "journal"
	Entity  string // raw juju entity tag / pod name / host name
	Unit    string // "loki/0" when the entity resolves to a unit, else ""
	Level   string // INFO / WARNING / ERROR / DEBUG / TRACE / CRITICAL
	Module  string // debug-log module, k8s container, or journald identifier
	Message string
	TraceID string // best-effort, usually empty (logs rarely carry it)
	SpanID  string // best-effort
	RawFile string // raw/... file this came from (relative to root)
	RawLine int    // 1-based line number within RawFile
}

// debugLogTimeLayout is the timestamp `juju debug-log --date --ms --utc` emits.
const debugLogTimeLayout = "2006-01-02 15:04:05.000"

// traceHintRe pulls a trace-id out of a log message when a charm or juju logs
// one inline (e.g. "trace-id=abcdef… span-id=…"). It is best-effort: debug-log
// does not print structured labels, so most lines have no id and fall back to
// the (unit, time-window) join.
var traceHintRe = regexp.MustCompile(`(?i)\btrace[-_]?id[=:]\s*([0-9a-f]{16,32})\b`)
var spanHintRe = regexp.MustCompile(`(?i)\bspan[-_]?id[=:]\s*([0-9a-f]{8,16})\b`)

// ParseDebugLogLine parses one `juju debug-log --date --ms --utc` line into a
// LogRecord. model is supplied by the caller (the log stream is invoked
// per-model, so it is not in the line itself). It returns ok=false for blank or
// malformed lines, which the caller skips.
//
// Line shape: "<entity>: <date> <time> <LEVEL> <module> <message>", e.g.
//
//	unit-loki-0: 2026-07-04 14:04:24.406 INFO juju.worker.uniter.operation ran "update-status" hook
func ParseDebugLogLine(line, model string) (LogRecord, bool) {
	line = strings.TrimRight(line, "\r\n")
	entity, rest, ok := strings.Cut(line, ": ")
	if !ok || entity == "" {
		return LogRecord{}, false
	}
	// rest = "<date> <time> <LEVEL> <module> <message>". Split off the five
	// leading fields; the message (which may contain spaces) is the remainder.
	f := strings.SplitN(rest, " ", 5)
	if len(f) < 4 {
		return LogRecord{}, false
	}
	ts, err := time.ParseInLocation(debugLogTimeLayout, f[0]+" "+f[1], time.UTC)
	if err != nil {
		// Tolerate a missing millisecond fraction (--ms not passed).
		ts, err = time.ParseInLocation("2006-01-02 15:04:05", f[0]+" "+f[1], time.UTC)
		if err != nil {
			return LogRecord{}, false
		}
	}
	rec := LogRecord{
		Ts:     ts.UTC(),
		Model:  model,
		Source: "debug-log",
		Entity: entity,
		Unit:   unitFromEntity(entity),
		Level:  f[2],
		Module: f[3],
	}
	if len(f) == 5 {
		rec.Message = f[4]
		if m := traceHintRe.FindStringSubmatch(f[4]); m != nil {
			rec.TraceID = strings.ToLower(m[1])
		}
		if m := spanHintRe.FindStringSubmatch(f[4]); m != nil {
			rec.SpanID = strings.ToLower(m[1])
		}
	}
	return rec, true
}

// unitFromEntity turns a juju entity tag into a unit name: "unit-loki-0" ->
// "loki/0". Non-unit entities (machine-0, controller-0) return "".
func unitFromEntity(entity string) string {
	rest, ok := strings.CutPrefix(entity, "unit-")
	if !ok {
		return ""
	}
	return unitFromPod(rest)
}

// unitFromPod turns a Juju CAAS pod name into a unit name: "loki-0" -> "loki/0"
// (the pod for unit N of app <app> is "<app>-<N>"). It splits on the last dash,
// so multi-word app names like "prometheus-k8s-0" resolve to "prometheus-k8s/0".
func unitFromPod(pod string) string {
	i := strings.LastIndexByte(pod, '-')
	if i < 0 {
		return pod
	}
	return pod[:i] + "/" + pod[i+1:]
}

// ParseK8sLogLine parses one line of `kubectl logs --timestamps` for a workload
// container. The caller supplies the model, pod and container (all in the raw
// path, not the line). Line shape: "<RFC3339Nano ts> <message>", e.g.
//
//	2026-07-04T19:13:25.277Z [grafana] level=info msg="ready"
//
// The container is recorded as the Module and the pod resolves to a Unit, so a
// workload log line correlates to the same unit timeline as that unit's hooks.
func ParseK8sLogLine(line, model, pod, container string) (LogRecord, bool) {
	line = strings.TrimRight(line, "\r\n")
	tsStr, msg, ok := strings.Cut(line, " ")
	if !ok {
		return LogRecord{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return LogRecord{}, false
	}
	rec := LogRecord{
		Ts:      ts.UTC(),
		Model:   model,
		Source:  "k8s",
		Entity:  pod,
		Unit:    unitFromPod(pod),
		Module:  container,
		Message: msg,
	}
	fillLogHints(&rec, msg)
	return rec, true
}

// journalPriority maps a syslog PRIORITY (0..7) to the level names the rest of
// the tool uses, so journald lines sit alongside debug-log lines uniformly.
var journalPriority = map[string]string{
	"0": "CRITICAL", "1": "CRITICAL", "2": "CRITICAL", "3": "ERROR",
	"4": "WARNING", "5": "INFO", "6": "INFO", "7": "DEBUG",
}

// ParseJournalLine parses one line of `journalctl -o json` (one JSON object per
// line). The caller supplies the model and host (from the raw path). Fields with
// non-UTF-8 values are emitted by journald as arrays; those (and lines without a
// string MESSAGE) are skipped. The host is the Entity; there is no per-unit
// systemd service on a Juju machine, so Unit is left empty and these lines join
// to spans only by the (model, time-window) fallback.
func ParseJournalLine(line, model, host string) (LogRecord, bool) {
	var j map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &j); err != nil {
		return LogRecord{}, false
	}
	msg, ok := jsonString(j["MESSAGE"])
	if !ok {
		return LogRecord{}, false
	}
	usec, ok := jsonString(j["__REALTIME_TIMESTAMP"])
	if !ok {
		return LogRecord{}, false
	}
	micros, err := strconv.ParseInt(usec, 10, 64)
	if err != nil {
		return LogRecord{}, false
	}
	ident, _ := jsonString(j["SYSLOG_IDENTIFIER"])
	if ident == "" {
		ident, _ = jsonString(j["_SYSTEMD_UNIT"])
	}
	prio, _ := jsonString(j["PRIORITY"])
	rec := LogRecord{
		Ts:      time.UnixMicro(micros).UTC(),
		Model:   model,
		Source:  "journal",
		Entity:  host,
		Level:   journalPriority[prio],
		Module:  ident,
		Message: msg,
	}
	fillLogHints(&rec, msg)
	return rec, true
}

// jsonString returns the string value of a journald field, or ok=false when the
// field is absent or (for binary values) an array rather than a string.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// fillLogHints best-effort extracts a trace/span id logged inline in a message.
func fillLogHints(rec *LogRecord, msg string) {
	if m := traceHintRe.FindStringSubmatch(msg); m != nil {
		rec.TraceID = strings.ToLower(m[1])
	}
	if m := spanHintRe.FindStringSubmatch(msg); m != nil {
		rec.SpanID = strings.ToLower(m[1])
	}
}

// ParseLogLine dispatches a raw line to the right parser based on its raw/
// relative path, filling Source/Model and the source-specific identity. It is
// the single decode point shared by the full-rebuild LoadLogs and the
// incremental Indexer, so both agree on how every log tree is interpreted. The
// caller sets RawFile/RawLine. Recognised layouts (rel path segments):
//
//	raw/juju/<model>/debug-log-*.log
//	raw/k8s/<model>/<pod>/<container>/*.log
//	raw/machine/<model>/<host>/*.log
func ParseLogLine(rel, line string) (LogRecord, bool) {
	p := strings.Split(filepath.ToSlash(rel), "/")
	if len(p) < 3 || p[0] != "raw" {
		return LogRecord{}, false
	}
	kind, model := p[1], p[2]
	switch kind {
	case "juju":
		return ParseDebugLogLine(line, model)
	case "k8s":
		if len(p) < 5 {
			return LogRecord{}, false
		}
		return ParseK8sLogLine(line, model, p[3], p[4])
	case "machine":
		if len(p) < 4 {
			return LogRecord{}, false
		}
		return ParseJournalLine(line, model, p[3])
	}
	return LogRecord{}, false
}

// logTrees are the raw/ sub-trees LoadLogs and the incremental Indexer walk,
// one per log source. Every "*.log" file underneath is decoded by ParseLogLine
// from its relative path, so adding a source is a matter of writing its parser.
var logTrees = []string{"juju", "k8s", "machine"}

// LoadLogs reads every log file under the raw/{juju,k8s,machine}/ trees, decodes
// each line via ParseLogLine, and returns the records in wall-clock order.
// Unparseable lines are skipped. Like LoadSpans it tolerates missing trees
// (returns nil) so callers can index recordings that captured no logs.
func LoadLogs(root string) ([]LogRecord, error) {
	l := NewLayout(root)
	var out []LogRecord
	for _, kind := range logTrees {
		kindRoot := filepath.Join(l.RawDir(), kind)
		paths, err := logFilesUnder(kindRoot)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			rel, _ := filepath.Rel(root, path)
			out = append(out, parseLogFile(path, rel)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out, nil
}

// logFilesUnder returns every "*.log" file below root, sorted by path so
// hour-keyed names within a stream read chronologically. A missing root yields
// no files (not an error).
func logFilesUnder(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".log") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func parseLogFile(path, rel string) []LogRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []LogRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long log lines
	lineNo := 0
	for sc.Scan() {
		lineNo++
		rec, ok := ParseLogLine(rel, sc.Text())
		if !ok {
			continue
		}
		rec.RawFile = rel
		rec.RawLine = lineNo
		out = append(out, rec)
	}
	return out
}
