package recording

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LogRecord is one parsed line of `juju debug-log`. It is the M3 "log
// observation" in the correlation model (VISION §7): a text line attributed to
// a unit and a time, joined to spans by span-id when present or by
// (unit, time-window) otherwise. The raw text lines remain the source of truth
// under raw/juju/<model>/; LogRecord is derived and rebuildable.
type LogRecord struct {
	Ts      time.Time
	Model   string // model the log stream belongs to (from the raw path)
	Entity  string // raw juju entity tag, e.g. "unit-loki-0" or "machine-0"
	Unit    string // "loki/0" when the entity is a unit, else ""
	Level   string // INFO / WARNING / ERROR / DEBUG / TRACE / CRITICAL
	Module  string // e.g. "juju.worker.uniter.operation"
	Message string
	TraceID string // best-effort, usually empty (debug-log rarely carries it)
	SpanID  string // best-effort
	RawFile string // raw/juju/<model>/debug-log-*.log this came from (relative to root)
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
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return rest
	}
	return rest[:i] + "/" + rest[i+1:]
}

// LoadLogs reads every raw/juju/<model>/debug-log-*.log file, parses each line,
// and returns the records in wall-clock order. Unparseable lines are skipped.
// Like LoadSpans it tolerates a missing tree (returns nil) so callers can index
// recordings that captured no logs.
func LoadLogs(root string) ([]LogRecord, error) {
	l := NewLayout(root)
	jujuRoot := filepath.Join(l.RawDir(), "juju")
	modelDirs, err := os.ReadDir(jujuRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LogRecord
	for _, md := range modelDirs {
		if !md.IsDir() {
			continue
		}
		model := md.Name()
		dir := filepath.Join(jujuRoot, model)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(files))
		for _, fi := range files {
			if !fi.IsDir() && strings.HasPrefix(fi.Name(), "debug-log-") && strings.HasSuffix(fi.Name(), ".log") {
				names = append(names, fi.Name())
			}
		}
		sort.Strings(names) // hour-keyed names sort chronologically
		for _, name := range names {
			rel, _ := filepath.Rel(root, filepath.Join(dir, name))
			recs := parseLogFile(filepath.Join(dir, name), rel, model)
			out = append(out, recs...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out, nil
}

func parseLogFile(path, rel, model string) []LogRecord {
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
		rec, ok := ParseDebugLogLine(sc.Text(), model)
		if !ok {
			continue
		}
		rec.RawFile = rel
		rec.RawLine = lineNo
		out = append(out, rec)
	}
	return out
}
