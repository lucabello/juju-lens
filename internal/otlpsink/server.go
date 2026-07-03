// Package otlpsink implements the OTLP gRPC receiver used by `juju-lens
// record`. It exposes the three OTel Collector-shaped services (Traces,
// Logs, Metrics) and forwards every request verbatim to a rotating JSONL
// writer. In M1 we only need traces to be functional; logs and metrics are
// wired up so the on-disk layout is stable, but the underlying files stay
// empty until later milestones start pushing to them.
package otlpsink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Writer is the minimal interface the receiver needs to persist payloads.
// The recording package's RotatingWriter satisfies it.
type Writer interface {
	WriteLine(p []byte) (int, error)
}

// Sinks bundles a writer per OTLP signal. Nil writers cause the corresponding
// service to accept requests and silently drop them, which is convenient for
// tests and for signals we haven't wired up yet.
type Sinks struct {
	Traces  Writer
	Logs    Writer
	Metrics Writer

	// Counters (atomic) for observability. Read via *Counts methods.
	traceCount  atomic.Int64
	logCount    atomic.Int64
	metricCount atomic.Int64
}

// TraceCount returns the total number of trace ExportRequests received.
func (s *Sinks) TraceCount() int64 { return s.traceCount.Load() }

// LogCount returns the total number of log ExportRequests received.
func (s *Sinks) LogCount() int64 { return s.logCount.Load() }

// MetricCount returns the total number of metric ExportRequests received.
func (s *Sinks) MetricCount() int64 { return s.metricCount.Load() }

// jsonMarshalOpts is the canonical JSON encoding for OTLP. We use it for
// on-disk storage instead of raw protobuf so recordings stay text-first.
// UseProtoNames keeps the shape identical across writer versions.
var jsonMarshalOpts = protojson.MarshalOptions{
	EmitUnpopulated: false,
	UseProtoNames:   true,
	Multiline:       false,
}

// MarshalJSON returns the canonical single-line JSON encoding of any OTLP
// message. It is exported so callers (e.g. synth) can produce recordings
// with the same encoding the gRPC receiver uses.
func MarshalJSON(msg proto.Message) ([]byte, error) {
	return jsonMarshalOpts.Marshal(msg)
}

func writeProto(w Writer, msg proto.Message) error {
	if w == nil {
		return nil
	}
	b, err := MarshalJSON(msg)
	if err != nil {
		return fmt.Errorf("marshal OTLP payload: %w", err)
	}
	if _, err := w.WriteLine(b); err != nil {
		return fmt.Errorf("write OTLP payload: %w", err)
	}
	return nil
}

// --- Trace service ---

type traceService struct {
	tracepb.UnimplementedTraceServiceServer
	sinks *Sinks
}

func (t *traceService) Export(_ context.Context, req *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	if err := writeProto(t.sinks.Traces, req); err != nil {
		return nil, err
	}
	t.sinks.traceCount.Add(1)
	return &tracepb.ExportTraceServiceResponse{}, nil
}

// --- Log service ---

type logService struct {
	logspb.UnimplementedLogsServiceServer
	sinks *Sinks
}

func (l *logService) Export(_ context.Context, req *logspb.ExportLogsServiceRequest) (*logspb.ExportLogsServiceResponse, error) {
	if err := writeProto(l.sinks.Logs, req); err != nil {
		return nil, err
	}
	l.sinks.logCount.Add(1)
	return &logspb.ExportLogsServiceResponse{}, nil
}

// --- Metric service ---

type metricService struct {
	metricspb.UnimplementedMetricsServiceServer
	sinks *Sinks
}

func (m *metricService) Export(_ context.Context, req *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	if err := writeProto(m.sinks.Metrics, req); err != nil {
		return nil, err
	}
	m.sinks.metricCount.Add(1)
	return &metricspb.ExportMetricsServiceResponse{}, nil
}

// Server hosts the OTLP gRPC endpoint. The zero value is not usable; use
// NewServer.
type Server struct {
	sinks *Sinks
	grpc  *grpc.Server
	lis   net.Listener
	addr  string
}

// NewServer registers the OTLP services on a fresh gRPC server bound to
// address (e.g. ":4317" or "127.0.0.1:0" in tests). It does not start
// serving; call Serve.
func NewServer(address string, sinks *Sinks) (*Server, error) {
	if sinks == nil {
		return nil, errors.New("otlpsink: sinks must not be nil")
	}
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("otlpsink: listen %q: %w", address, err)
	}
	gs := grpc.NewServer()
	tracepb.RegisterTraceServiceServer(gs, &traceService{sinks: sinks})
	logspb.RegisterLogsServiceServer(gs, &logService{sinks: sinks})
	metricspb.RegisterMetricsServiceServer(gs, &metricService{sinks: sinks})
	return &Server{sinks: sinks, grpc: gs, lis: lis, addr: lis.Addr().String()}, nil
}

// Addr returns the actual address the server bound to. Useful when the
// caller asked for port 0.
func (s *Server) Addr() string { return s.addr }

// Serve blocks until the gRPC server stops. It returns nil on graceful stop.
func (s *Server) Serve() error {
	err := s.grpc.Serve(s.lis)
	if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// GracefulStop is a small wrapper so callers don't need to know about the
// underlying grpc.Server.
func (s *Server) GracefulStop() { s.grpc.GracefulStop() }
