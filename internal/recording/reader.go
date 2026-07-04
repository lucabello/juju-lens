package recording

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/wire"
)

// SpanRow is the flattened, viewer-friendly projection of one synthesised RPC
// span. In the eBPF/RPC approach a span is a single captured Juju API call:
// start = the write (request) timestamp, end = the matching read (response)
// timestamp, name = "<facade>.<method>". The raw envelope is the source of
// truth; SpanRow is derived and re-buildable, so columns can grow without
// breaking on-disk compatibility.
type SpanRow struct {
	TraceID      string // hex
	SpanID       string // hex
	ParentSpanID string // hex ("" for roots)
	Name         string
	Start        time.Time
	End          time.Time
	StatusCode   string // "OK" or "ERROR"
	StatusMsg    string
	Service      string            // coarse origin: "agent", "jujuc", ...
	Controller   string            // controller name the agent belongs to
	Model        string            // model name the connection was attributed to
	ModelUUID    string            // model UUID
	App          string            // application the RPC came from
	Unit         string            // agent tag the RPC came from
	Hook         string            // set by extractors when derivable
	Relation     string            // set by extractors when derivable
	RawFile      string            // raw/rpc/<model>/calls-*.jsonl this came from (relative to root)
	RawOffset    int64             // byte offset of the request line within RawFile
	Attrs        map[string]string // flattened envelope fields for the details pane
}

// Duration is a convenience for the viewer.
func (s SpanRow) Duration() time.Duration { return s.End.Sub(s.Start) }

// LoadSpans reads every per-model calls file under raw/rpc/, pairs each
// request with its response by (pid, conn, request-id), and returns the
// synthesised spans in wall-clock order. Files with unreadable lines are
// tolerated line-by-line: a bad line is skipped with a warning rather than
// failing the whole recording.
func LoadSpans(root string) ([]SpanRow, error) {
	l := NewLayout(root)
	rpcRoot := filepath.Join(l.RawDir(), "rpc")
	modelDirs, err := os.ReadDir(rpcRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", rpcRoot, err)
	}

	p := newPairer()
	for _, md := range modelDirs {
		if !md.IsDir() {
			continue
		}
		dir := filepath.Join(rpcRoot, md.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: skipping %s: %v\n", dir, err)
			continue
		}
		names := make([]string, 0, len(files))
		for _, f := range files {
			if !f.IsDir() && strings.HasPrefix(f.Name(), "calls-") && strings.HasSuffix(f.Name(), ".jsonl") {
				names = append(names, f.Name())
			}
		}
		sort.Strings(names) // hour-keyed names sort into chronological order
		for _, name := range names {
			rel, _ := filepath.Rel(root, filepath.Join(dir, name))
			if err := p.consumeFile(filepath.Join(dir, name), rel); err != nil {
				fmt.Fprintf(os.Stderr, "juju-lens: skipping %s: %v\n", name, err)
			}
		}
	}
	rows := p.finish()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Start.Before(rows[j].Start) })
	return rows, nil
}

// pairer accumulates outstanding requests keyed by (pid, conn, request-id) and
// emits a SpanRow when the matching response arrives. Requests without a
// response (recorder stopped mid-flight) are flushed as zero-duration spans at
// finish() so nothing captured is silently dropped.
type pairer struct {
	pending map[reqKey]SpanRow
	rows    []SpanRow
}

type reqKey struct {
	pid  int
	conn uint64
	rid  uint64
}

func newPairer() *pairer { return &pairer{pending: map[reqKey]SpanRow{}} }

func (p *pairer) consumeFile(path, rel string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		lineOffset := offset
		offset += int64(len(line))
		trimmed := line
		if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
			trimmed = trimmed[:n-1]
		}
		if len(trimmed) > 0 {
			cm, uerr := wire.UnmarshalLine(trimmed)
			if uerr == nil {
				p.add(cm, rel, lineOffset)
			}
		}
		if err != nil {
			break // io.EOF or a real read error; either way we're done
		}
	}
	return nil
}

func (p *pairer) add(cm wire.CapturedMessage, rawFile string, offset int64) {
	key := reqKey{pid: cm.PID, conn: cm.Conn, rid: cm.Msg.RequestID}
	if cm.Msg.IsRequest() {
		row := SpanRow{
			Name:       spanName(cm.Msg),
			Start:      cm.Ts.UTC(),
			End:        cm.Ts.UTC(),
			Service:    serviceOf(cm),
			Controller: cm.Controller,
			Model:      cm.Model,
			ModelUUID:  cm.ModelUUID,
			App:        cm.App,
			Unit:       cm.Unit,
			StatusCode: "OK",
			RawFile:    rawFile,
			RawOffset:  offset,
			Attrs:      envelopeAttrs(cm.Msg),
		}
		row.TraceID, row.SpanID = spanIDs(cm.Msg, key)
		p.pending[key] = row
		return
	}
	// Response half: close out the pending request if we have it.
	row, ok := p.pending[key]
	if !ok {
		return // orphan response (recorder started mid-connection); ignore
	}
	delete(p.pending, key)
	row.End = cm.Ts.UTC()
	if row.End.Before(row.Start) {
		row.End = row.Start
	}
	if cm.Msg.Error != "" {
		row.StatusCode = "ERROR"
		row.StatusMsg = cm.Msg.Error
		row.Attrs["error"] = cm.Msg.Error
		if cm.Msg.ErrorCode != "" {
			row.Attrs["error-code"] = cm.Msg.ErrorCode
		}
	}
	if len(cm.Msg.Response) > 0 {
		row.Attrs["response"] = compactJSON(cm.Msg.Response)
	}
	p.rows = append(p.rows, row)
}

func (p *pairer) finish() []SpanRow {
	for _, row := range p.pending {
		p.rows = append(p.rows, row)
	}
	p.pending = map[reqKey]SpanRow{}
	return p.rows
}

// spanName is "<facade>.<method>", falling back gracefully when one part is
// missing so a partially-decoded envelope still gets a readable label.
func spanName(e wire.Envelope) string {
	switch {
	case e.Type != "" && e.Request != "":
		return e.Type + "." + e.Request
	case e.Request != "":
		return e.Request
	case e.Type != "":
		return e.Type
	default:
		return "(rpc)"
	}
}

// serviceOf gives a coarse origin label for the details pane. The probe knows
// the target binary; until it plumbs that through we distinguish jujuc hook
// tools (their own facade) from ordinary agent API calls.
func serviceOf(cm wire.CapturedMessage) string {
	if cm.Msg.Type == "JujucServer" || strings.HasPrefix(cm.Msg.Type, "Jujuc") {
		return "jujuc"
	}
	return "agent"
}

// spanIDs returns the span's trace/span ids. When the wire envelope carries
// them (upstream tracing on) they are used verbatim; otherwise they are
// synthesised deterministically from the request key so re-indexing the same
// raw data yields identical ids.
func spanIDs(e wire.Envelope, key reqKey) (traceID, spanID string) {
	if e.SpanID != "" {
		traceID = e.TraceID
		if traceID == "" {
			traceID = e.SpanID
		}
		return traceID, e.SpanID
	}
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(key.pid))
	h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], key.conn)
	h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], key.rid)
	h.Write(buf[:])
	sum := h.Sum64()
	sid := make([]byte, 8)
	binary.BigEndian.PutUint64(sid, sum)
	tid := make([]byte, 16)
	binary.BigEndian.PutUint64(tid[8:], sum)
	binary.BigEndian.PutUint64(tid[:8], uint64(key.pid))
	return hex.EncodeToString(tid), hex.EncodeToString(sid)
}

// envelopeAttrs flattens the envelope's scalar fields for the details pane.
// Params/response bodies are compacted so the pane stays greppable.
func envelopeAttrs(e wire.Envelope) map[string]string {
	m := map[string]string{
		"facade":     e.Type,
		"method":     e.Request,
		"request-id": fmt.Sprintf("%d", e.RequestID),
	}
	if e.Version != 0 {
		m["version"] = fmt.Sprintf("%d", e.Version)
	}
	if e.ID != "" {
		m["id"] = e.ID
	}
	if e.TraceID != "" {
		m["trace-id"] = e.TraceID
	}
	if len(e.Params) > 0 {
		m["params"] = compactJSON(e.Params)
	}
	// Drop empties so the pane isn't cluttered with blank rows.
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	return m
}

func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}
