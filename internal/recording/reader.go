package recording

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// SpanRow is the flattened, viewer-friendly projection of an OTLP span. M1
// keeps this deliberately small; later milestones can add columns without
// breaking on-disk compatibility since the raw payloads are the source of
// truth.
type SpanRow struct {
	TraceID      string // hex
	SpanID       string // hex
	ParentSpanID string // hex ("" for roots)
	Name         string
	Start        time.Time
	End          time.Time
	StatusCode   string // "OK", "ERROR", "UNSET"
	StatusMsg    string
	Service      string            // resource attr "service.name"
	Model        string            // resource attr "juju.model"
	Unit         string            // resource attr "juju.unit" or span attr "executor.unit"
	Hook         string            // span attr "juju.hook" (if set)
	Relation     string            // span attr "juju.relation" (if set)
	Attrs        map[string]string // flattened for the details pane
}

// Duration is a convenience for the viewer.
func (s SpanRow) Duration() time.Duration { return s.End.Sub(s.Start) }

// LoadSpans reads every OTLP trace file under the recording's raw/otlp/
// directory and returns spans in wall-clock order. Files whose contents are
// not valid OTLP-JSON are skipped with a warning to stderr; a hand-copied
// recording never crashes the viewer.
func LoadSpans(root string) ([]SpanRow, error) {
	l := NewLayout(root)
	var rows []SpanRow
	entries, err := os.ReadDir(l.OTLPDir())
	if err != nil {
		if os.IsNotExist(err) {
			return rows, nil
		}
		return nil, fmt.Errorf("reading %s: %w", l.OTLPDir(), err)
	}
	// Consider only trace files; ignore logs/metrics for M1.
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "traces-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		got, err := loadSpanFile(filepath.Join(l.OTLPDir(), e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "juju-lens: skipping %s: %v\n", e.Name(), err)
			continue
		}
		rows = append(rows, got...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Start.Before(rows[j].Start) })
	return rows, nil
}

var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

func loadSpanFile(path string) ([]SpanRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 16<<20)

	var rows []SpanRow
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req tracepb.ExportTraceServiceRequest
		if err := unmarshalOpts.Unmarshal(line, &req); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		rows = append(rows, flatten(&req)...)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	return rows, nil
}

func flatten(req *tracepb.ExportTraceServiceRequest) []SpanRow {
	var rows []SpanRow
	for _, rs := range req.GetResourceSpans() {
		service := ""
		model := ""
		unit := ""
		for _, a := range rs.GetResource().GetAttributes() {
			switch a.Key {
			case "service.name":
				service = a.Value.GetStringValue()
			case "juju.model":
				model = a.Value.GetStringValue()
			case "juju.unit":
				unit = a.Value.GetStringValue()
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				row := SpanRow{
					TraceID:      hex.EncodeToString(sp.GetTraceId()),
					SpanID:       hex.EncodeToString(sp.GetSpanId()),
					ParentSpanID: hex.EncodeToString(sp.GetParentSpanId()),
					Name:         sp.GetName(),
					Start:        time.Unix(0, int64(sp.GetStartTimeUnixNano())).UTC(),
					End:          time.Unix(0, int64(sp.GetEndTimeUnixNano())).UTC(),
					StatusCode:   sp.GetStatus().GetCode().String(),
					StatusMsg:    sp.GetStatus().GetMessage(),
					Service:      service,
					Model:        model,
					Unit:         unit,
					Attrs:        map[string]string{},
				}
				for _, a := range sp.GetAttributes() {
					v := attrString(a.GetValue())
					row.Attrs[a.Key] = v
					switch a.Key {
					case "juju.hook":
						row.Hook = v
					case "juju.relation":
						row.Relation = v
					case "executor.unit":
						if row.Unit == "" {
							row.Unit = v
						}
					}
				}
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func attrString(v *commonAny) string {
	if v == nil {
		return ""
	}
	switch val := v.GetValue().(type) {
	case *commonAnyString:
		return val.StringValue
	case *commonAnyBool:
		if val.BoolValue {
			return "true"
		}
		return "false"
	case *commonAnyInt:
		return fmt.Sprintf("%d", val.IntValue)
	case *commonAnyDouble:
		return fmt.Sprintf("%g", val.DoubleValue)
	default:
		// Anything else stringifies through its protobuf representation.
		return fmt.Sprintf("%v", v)
	}
}

// The reader intentionally re-declares these OTLP common types as thin
// aliases so it does not need to import the deeply nested common/v1 package
// name in every helper. The aliases keep the flatten() function readable.
type (
	commonAny       = otelCommonAny
	commonAnyString = otelCommonAnyString
	commonAnyBool   = otelCommonAnyBool
	commonAnyInt    = otelCommonAnyInt
	commonAnyDouble = otelCommonAnyDouble
)
