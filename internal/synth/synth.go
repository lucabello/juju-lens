// Package synth generates deterministic OTLP recordings that mimic the
// shape of the spans Juju emits during real workloads. It exists so the
// viewer can be developed and tested without a live controller.
//
// M1 ships a single "trivial" scenario: two apps (grafana, prometheus)
// relating over the grafana-source endpoint, producing an install-time
// sequence of hooks culminating in a matched pair of relation-joined and
// relation-changed events. That is enough to exercise the viewer's core
// timeline plumbing.
package synth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lucabello/juju-lens/internal/otlpsink"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tv1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Scenario is the identifier for a synth scenario.
type Scenario string

const (
	// ScenarioTrivial produces two applications, one relation, and about
	// a dozen spans. Time origin defaults to 2026-07-03T14:30:12Z UTC.
	ScenarioTrivial Scenario = "trivial"
)

// KnownScenarios lists every scenario the tool currently supports.
func KnownScenarios() []Scenario { return []Scenario{ScenarioTrivial} }

// Options tune how a scenario is emitted. Any zero value falls back to a
// sensible default that keeps recordings deterministic across runs.
type Options struct {
	// Start is the wall-clock instant assigned to the first event. When
	// zero, a fixed 2026-07-03T14:30:12Z instant is used so recordings
	// are byte-for-byte identical between runs.
	Start time.Time
	// ControllerName is stamped as service.name on the "controller-side"
	// resource. Defaults to "juju-controller".
	ControllerName string
	// ModelName is stamped as juju.model. Defaults to "default".
	ModelName string
}

func (o Options) filled() Options {
	if o.Start.IsZero() {
		o.Start = time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	}
	if o.ControllerName == "" {
		o.ControllerName = "juju-controller"
	}
	if o.ModelName == "" {
		o.ModelName = "default"
	}
	return o
}

// Generate builds an OTLP trace export request for the given scenario. It
// does not touch the filesystem or the network; callers write the returned
// message somewhere useful. This makes Generate trivially testable.
func Generate(sc Scenario, opts Options) (*tracepb.ExportTraceServiceRequest, error) {
	switch sc {
	case ScenarioTrivial:
		return generateTrivial(opts.filled()), nil
	default:
		return nil, fmt.Errorf("synth: unknown scenario %q", sc)
	}
}

// Emit generates the scenario and writes it through a fresh Sinks/writer to
// the given otlpsink.Writer. It is a thin convenience wrapper used by
// `juju-lens synth`.
func Emit(ctx context.Context, sc Scenario, opts Options, w otlpsink.Writer) error {
	if w == nil {
		return errors.New("synth: writer must not be nil")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	req, err := Generate(sc, opts)
	if err != nil {
		return err
	}
	sinks := &otlpsink.Sinks{Traces: w}
	_ = sinks // reserved for future scenarios that produce logs/metrics too
	// Marshal via the same code path the receiver uses so the JSONL layout
	// stays consistent between "recorded from a live controller" and
	// "synthesised locally".
	return writeAsJSONL(w, req)
}

// writeAsJSONL is a tiny shim over the exported JSON marshaller in otlpsink.
// It's here (and not in otlpsink) because synth is the only caller that
// needs it outside the gRPC path.
func writeAsJSONL(w otlpsink.Writer, req *tracepb.ExportTraceServiceRequest) error {
	b, err := otlpsink.MarshalJSON(req)
	if err != nil {
		return err
	}
	if _, err := w.WriteLine(b); err != nil {
		return err
	}
	return nil
}

// --- Scenario builders ---

// spanBuilder is a helper for producing spans with mostly-shared
// attributes. It intentionally does not implement OTel semantics faithfully
// — the important thing is that the viewer can consume the shape.
type spanBuilder struct {
	traceID []byte
	origin  time.Time
	cursor  time.Duration
	next    byte
}

func newSpanBuilder(traceID byte, origin time.Time) *spanBuilder {
	tid := make([]byte, 16)
	for i := range tid {
		tid[i] = traceID
	}
	return &spanBuilder{traceID: tid, origin: origin}
}

func (b *spanBuilder) newSpanID() []byte {
	b.next++
	sid := make([]byte, 8)
	for i := range sid {
		sid[i] = b.next
	}
	return sid
}

// span makes a span that starts "cursor" nanoseconds into the scenario and
// lasts "d". The cursor is advanced by "d" plus 1ms slack so the next
// sibling span looks realistic.
func (b *spanBuilder) span(name string, parent []byte, d time.Duration, attrs ...*commonpb.KeyValue) *tv1.Span {
	start := b.origin.Add(b.cursor)
	end := start.Add(d)
	b.cursor += d + time.Millisecond
	return &tv1.Span{
		TraceId:           b.traceID,
		SpanId:            b.newSpanID(),
		ParentSpanId:      parent,
		Name:              name,
		Kind:              tv1.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(end.UnixNano()),
		Attributes:        attrs,
		Status:            &tv1.Status{Code: tv1.Status_STATUS_CODE_OK},
	}
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   k,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
	}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   k,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}},
	}
}

func resource(controllerName, modelName, service, unit string) *resourcepb.Resource {
	return &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strAttr("service.name", service),
			strAttr("juju.controller", controllerName),
			strAttr("juju.model", modelName),
			strAttr("juju.unit", unit),
		},
	}
}

func generateTrivial(opts Options) *tracepb.ExportTraceServiceRequest {
	// Two Juju units in one model. Each unit gets its own ResourceSpans
	// entry because in a real deployment they run in different processes
	// and would flush independently.
	grafanaB := newSpanBuilder(0xa1, opts.Start)
	prometheusB := newSpanBuilder(0xa2, opts.Start.Add(50*time.Millisecond))

	grafanaSpans := []*tv1.Span{
		grafanaB.span("uniter.RunOperation: install", nil, 500*time.Millisecond,
			strAttr("executor.state", "runHook install"),
			strAttr("executor.unit", "grafana/0"),
			strAttr("juju.hook", "install"),
		),
	}
	installID := grafanaSpans[len(grafanaSpans)-1].SpanId
	grafanaSpans = append(grafanaSpans,
		grafanaB.span("hook: install", installID, 480*time.Millisecond,
			strAttr("juju.hook", "install"),
			strAttr("juju.unit", "grafana/0"),
		),
		grafanaB.span("uniter.RunOperation: config-changed", nil, 200*time.Millisecond,
			strAttr("executor.state", "runHook config-changed"),
			strAttr("executor.unit", "grafana/0"),
			strAttr("juju.hook", "config-changed"),
		),
		grafanaB.span("uniter.RunOperation: start", nil, 300*time.Millisecond,
			strAttr("executor.state", "runHook start"),
			strAttr("executor.unit", "grafana/0"),
			strAttr("juju.hook", "start"),
		),
		grafanaB.span("uniter.RunOperation: grafana-source-relation-created", nil, 120*time.Millisecond,
			strAttr("executor.state", "runHook grafana-source-relation-created"),
			strAttr("executor.unit", "grafana/0"),
			strAttr("juju.hook", "relation-created"),
			strAttr("juju.relation", "grafana-source"),
			intAttr("juju.relation.id", 3),
		),
	)

	joinedID := grafanaB.newSpanID()
	// The relation-joined span is manually crafted so we can add a child
	// CommitHookChanges span to demonstrate causal children.
	joinedStart := opts.Start.Add(grafanaB.cursor)
	joinedEnd := joinedStart.Add(420 * time.Millisecond)
	grafanaB.cursor += 420*time.Millisecond + time.Millisecond
	grafanaSpans = append(grafanaSpans, &tv1.Span{
		TraceId:           grafanaB.traceID,
		SpanId:            joinedID,
		Name:              "uniter.RunOperation: grafana-source-relation-joined",
		Kind:              tv1.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(joinedStart.UnixNano()),
		EndTimeUnixNano:   uint64(joinedEnd.UnixNano()),
		Attributes: []*commonpb.KeyValue{
			strAttr("executor.state", "runHook grafana-source-relation-joined"),
			strAttr("executor.unit", "grafana/0"),
			strAttr("juju.hook", "relation-joined"),
			strAttr("juju.relation", "grafana-source"),
			strAttr("juju.remote.unit", "prometheus/0"),
			strAttr("juju.remote.app", "prometheus"),
			intAttr("juju.relation.id", 3),
		},
		Status: &tv1.Status{Code: tv1.Status_STATUS_CODE_OK},
	})
	grafanaSpans = append(grafanaSpans,
		grafanaB.span("uniter.CommitHookChanges", joinedID, 20*time.Millisecond,
			strAttr("juju.facade", "Uniter"),
			strAttr("juju.method", "CommitHookChanges"),
			strAttr("juju.unit", "grafana/0"),
			strAttr("juju.relation", "grafana-source"),
			strAttr("databag.write", `{"ingress-address":"10.1.2.3"}`),
		),
		// grafana/0 is the leader and sets both its unit status and the
		// application status. The two are independent state in Juju and
		// each becomes its own snapshot scope.
		grafanaB.span("jujuc.status-set", nil, 5*time.Millisecond,
			strAttr("juju.tool", "status-set"),
			strAttr("juju.unit", "grafana/0"),
			strAttr("juju.status.kind", "workload"),
			strAttr("juju.status.workload.value", "active"),
			strAttr("juju.status.workload.message", "Ready"),
		),
		grafanaB.span("jujuc.status-set --application", nil, 5*time.Millisecond,
			strAttr("juju.tool", "status-set"),
			strAttr("juju.unit", "grafana/0"),
			strAttr("juju.status.kind", "application"),
			strAttr("juju.status.application.value", "active"),
			strAttr("juju.status.application.message", "All units ready"),
		),
	)

	prometheusSpans := []*tv1.Span{
		prometheusB.span("uniter.RunOperation: install", nil, 500*time.Millisecond,
			strAttr("executor.state", "runHook install"),
			strAttr("executor.unit", "prometheus/0"),
			strAttr("juju.hook", "install"),
		),
		prometheusB.span("uniter.RunOperation: config-changed", nil, 200*time.Millisecond,
			strAttr("executor.state", "runHook config-changed"),
			strAttr("executor.unit", "prometheus/0"),
			strAttr("juju.hook", "config-changed"),
		),
		prometheusB.span("uniter.RunOperation: start", nil, 250*time.Millisecond,
			strAttr("executor.state", "runHook start"),
			strAttr("executor.unit", "prometheus/0"),
			strAttr("juju.hook", "start"),
		),
		prometheusB.span("uniter.RunOperation: grafana-source-relation-joined", nil, 380*time.Millisecond,
			strAttr("executor.state", "runHook grafana-source-relation-joined"),
			strAttr("executor.unit", "prometheus/0"),
			strAttr("juju.hook", "relation-joined"),
			strAttr("juju.relation", "grafana-source"),
			strAttr("juju.remote.unit", "grafana/0"),
			strAttr("juju.remote.app", "grafana"),
			intAttr("juju.relation.id", 3),
		),
		prometheusB.span("uniter.RunOperation: grafana-source-relation-changed", nil, 340*time.Millisecond,
			strAttr("executor.state", "runHook grafana-source-relation-changed"),
			strAttr("executor.unit", "prometheus/0"),
			strAttr("juju.hook", "relation-changed"),
			strAttr("juju.relation", "grafana-source"),
			strAttr("juju.remote.unit", "grafana/0"),
			intAttr("juju.relation.id", 3),
		),
		// prometheus/0 sets its own workload status and (as leader of its
		// application) sets the prometheus app status too. Status "waiting"
		// is deliberately distinct from grafana so the pane has variety.
		prometheusB.span("jujuc.status-set", nil, 5*time.Millisecond,
			strAttr("juju.tool", "status-set"),
			strAttr("juju.unit", "prometheus/0"),
			strAttr("juju.status.kind", "workload"),
			strAttr("juju.status.workload.value", "waiting"),
			strAttr("juju.status.workload.message", "waiting for grafana"),
		),
		prometheusB.span("jujuc.status-set --application", nil, 5*time.Millisecond,
			strAttr("juju.tool", "status-set"),
			strAttr("juju.unit", "prometheus/0"),
			strAttr("juju.status.kind", "application"),
			strAttr("juju.status.application.value", "waiting"),
			strAttr("juju.status.application.message", "waiting for peers"),
		),
	}

	return &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tv1.ResourceSpans{
			{
				Resource: resource(opts.ControllerName, opts.ModelName, "jujud", "grafana/0"),
				ScopeSpans: []*tv1.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "github.com/juju/juju/internal/worker/uniter",
							Version: "synth",
						},
						Spans: grafanaSpans,
					},
				},
			},
			{
				Resource: resource(opts.ControllerName, opts.ModelName, "jujud", "prometheus/0"),
				ScopeSpans: []*tv1.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "github.com/juju/juju/internal/worker/uniter",
							Version: "synth",
						},
						Spans: prometheusSpans,
					},
				},
			},
		},
	}
}
