package otlpsink

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tv1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// memWriter is a Writer that buffers everything in memory. It is a stand-in
// for the on-disk RotatingWriter and lets us assert on exact bytes.
type memWriter struct {
	mu    sync.Mutex
	lines [][]byte
}

func (m *memWriter) WriteLine(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dup := append([]byte(nil), p...)
	m.lines = append(m.lines, dup)
	return len(p) + 1, nil
}

func (m *memWriter) Snapshot() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.lines))
	for i, l := range m.lines {
		out[i] = append([]byte(nil), l...)
	}
	return out
}

func TestOTLPServerRoundtripTraces(t *testing.T) {
	writer := &memWriter{}
	sinks := &Sinks{Traces: writer}
	srv, err := NewServer("127.0.0.1:0", sinks)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	defer func() {
		srv.GracefulStop()
		<-done
	}()

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	defer conn.Close()

	client := tracepb.NewTraceServiceClient(conn)
	req := &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tv1.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{
							Key:   "service.name",
							Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "juju-lens-test"}},
						},
					},
				},
				ScopeSpans: []*tv1.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{Name: "test-scope"},
						Spans: []*tv1.Span{
							{
								Name:              "hello",
								TraceId:           bytes.Repeat([]byte{0xab}, 16),
								SpanId:            bytes.Repeat([]byte{0xcd}, 8),
								StartTimeUnixNano: 1,
								EndTimeUnixNano:   2,
							},
						},
					},
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Export(ctx, req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Wait a moment for the write to finish (it happens synchronously in the
	// handler, but the roundtrip is over gRPC).
	deadline := time.Now().Add(2 * time.Second)
	var got [][]byte
	for time.Now().Before(deadline) {
		got = writer.Snapshot()
		if len(got) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("wanted 1 written line, got %d", len(got))
	}

	// The line must be valid JSON and mention our service.name value.
	var v map[string]any
	if err := json.Unmarshal(got[0], &v); err != nil {
		t.Fatalf("written line is not JSON: %v (bytes=%q)", err, got[0])
	}
	if !strings.Contains(string(got[0]), "juju-lens-test") {
		t.Fatalf("written line does not contain service.name: %s", got[0])
	}
	if sinks.TraceCount() != 1 {
		t.Fatalf("TraceCount = %d, want 1", sinks.TraceCount())
	}
}

func TestOTLPServerNilWriterDropsGracefully(t *testing.T) {
	sinks := &Sinks{Traces: nil}
	srv, err := NewServer("127.0.0.1:0", sinks)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	defer func() {
		srv.GracefulStop()
		<-done
	}()

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := tracepb.NewTraceServiceClient(conn)
	if _, err := client.Export(context.Background(), &tracepb.ExportTraceServiceRequest{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sinks.TraceCount() != 1 {
		t.Fatalf("TraceCount = %d, want 1", sinks.TraceCount())
	}
}

func TestNewServerRejectsNilSinks(t *testing.T) {
	if _, err := NewServer("127.0.0.1:0", nil); err == nil {
		t.Fatal("expected error for nil sinks")
	}
}

func TestNewServerRejectsBadAddress(t *testing.T) {
	// Take a port and try to bind to it again.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := NewServer(l.Addr().String(), &Sinks{}); err == nil {
		t.Fatal("expected error binding to already-used port")
	}
}
