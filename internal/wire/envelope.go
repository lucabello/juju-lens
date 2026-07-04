// Package wire models Juju's JSON-over-websocket RPC codec as it appears on
// the wire, plus the on-disk shape juju-lens writes for every captured RPC.
//
// The recorder captures raw plaintext at the TLS boundary (see internal/probe),
// strips websocket framing (websocket.go), and JSON-decodes each message into
// an Envelope. Every decoded message is wrapped in a CapturedMessage — the
// envelope plus the capture metadata we need to pair and attribute it — and
// written one-per-line to raw/rpc/<model>/calls-*.jsonl. Nothing in this
// package depends on eBPF or on the recording layout, so it is trivially
// testable and reusable by both the live recorder and `juju-lens index`.
package wire

import (
	"encoding/json"
	"time"
)

// Envelope is Juju's self-describing RPC message, matching inMsgV1/outMsgV1 in
// rpc/jsoncodec/codec.go. A request populates Type/Version/Request/Params; the
// matching response carries the same RequestID with Response or Error set. The
// trace-* fields are populated only when upstream tracing is configured; we
// treat them as optional and fall back to request-id pairing when absent.
type Envelope struct {
	RequestID  uint64          `json:"request-id"`
	Type       string          `json:"type,omitempty"` // facade, e.g. "Uniter"
	Version    int             `json:"version,omitempty"`
	ID         string          `json:"id,omitempty"`
	Request    string          `json:"request,omitempty"` // method, e.g. "CommitHookChanges"
	Params     json.RawMessage `json:"params,omitempty"`
	Error      string          `json:"error,omitempty"`
	ErrorCode  string          `json:"error-code,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	TraceID    string          `json:"trace-id,omitempty"`
	SpanID     string          `json:"span-id,omitempty"`
	TraceFlags int             `json:"trace-flags,omitempty"`
}

// IsRequest reports whether the envelope is the request half of an RPC. Only
// requests carry a facade/method; responses echo the request-id with a
// response or error payload.
func (e Envelope) IsRequest() bool { return e.Type != "" || e.Request != "" }

// Direction of a captured frame relative to the observed process.
type Direction string

const (
	// DirWrite is data the target process wrote (encrypted just after we saw
	// it). For a Juju agent this is the outbound request stream.
	DirWrite Direction = "write"
	// DirRead is data the target process read (decrypted just before we saw
	// it). For a Juju agent this is the inbound response stream.
	DirRead Direction = "read"
)

// CapturedMessage is one line in raw/rpc/<model>/calls-*.jsonl: a decoded RPC
// envelope with the metadata needed to pair and attribute it. The envelope is
// stored verbatim so the raw file remains a faithful, greppable record even if
// juju-lens' own decoders change later.
type CapturedMessage struct {
	Ts   time.Time `json:"ts"`             // wall-clock capture time at the TLS boundary
	PID  int       `json:"pid"`            // process the frame was captured from
	Dir  Direction `json:"dir"`            // write (request) or read (response)
	Conn uint64    `json:"conn,omitempty"` // derived from the *tls.Conn pointer at probe time
	// Topology: which controller/model/app/unit the capturing agent belongs to.
	// Filled from the agent process itself (see probe.Topology) and enriched by
	// the recorder with human names resolved from the juju client, so it is
	// present even for connections whose handshake we never observed.
	Controller string   `json:"controller,omitempty"` // controller name (falls back to UUID)
	Model      string   `json:"model,omitempty"`      // model name (falls back to UUID)
	ModelUUID  string   `json:"model_uuid,omitempty"` // model UUID
	App        string   `json:"app,omitempty"`        // application name
	Unit       string   `json:"unit,omitempty"`       // unit name, e.g. "grafana/0"
	Msg        Envelope `json:"msg"`
}

// MarshalLine renders a CapturedMessage as a single JSON line (no trailing
// newline). The RotatingWriter appends the newline.
func (c CapturedMessage) MarshalLine() ([]byte, error) { return json.Marshal(c) }

// UnmarshalLine parses one raw/rpc line into a CapturedMessage.
func UnmarshalLine(b []byte) (CapturedMessage, error) {
	var c CapturedMessage
	err := json.Unmarshal(b, &c)
	return c, err
}
