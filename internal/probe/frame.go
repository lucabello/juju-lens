// Package probe defines the juju-lens-probe wire protocol and the recorder-side
// machinery that drives it. The probe is a small binary that runs on the host
// containing a target jujud/containeragent process, attaches eBPF uprobes to
// the crypto/tls read/write boundary, and streams the captured plaintext back
// to the recorder as length-prefixed JSON frames on stdout.
//
// This file defines the frame protocol, which is shared by both sides so the
// probe binary and the recorder agree on the encoding without importing each
// other's internals. It has no OS-specific or eBPF dependencies and is fully
// unit-testable.
package probe

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// maxFrameBytes bounds a single decoded frame to guard against a corrupt or
// hostile length prefix. Juju TLS records are well under this.
const maxFrameBytes = 64 << 20

// Frame is one captured chunk of plaintext at the TLS boundary, or a periodic
// stats frame reporting ring-buffer drops. Data is base64-encoded by
// encoding/json automatically. A frame with len(Data)==0 and Drops>0 is a
// pure stats frame.
type Frame struct {
	TsUnixNano int64  `json:"ts"`
	PID        int    `json:"pid"`
	Dir        string `json:"dir"`            // "write" or "read"
	Conn       uint64 `json:"conn,omitempty"` // *tls.Conn pointer captured at probe time
	// Topology of the agent process, derived probe-side from /proc (cmdline +
	// agent.conf). These let the recorder attribute every RPC to the
	// controller→model→app→unit it came from without relying on the (often
	// unseen, mid-stream) websocket handshake.
	Controller string `json:"controller,omitempty"` // controller UUID from agent.conf
	Model      string `json:"model,omitempty"`      // model UUID from agent.conf
	App        string `json:"app,omitempty"`        // application name (from unit name)
	Unit       string `json:"unit,omitempty"`       // unit name, e.g. "grafana/0"
	Data       []byte `json:"data,omitempty"`
	Drops      uint64 `json:"drops,omitempty"` // ring-buffer records lost since the last frame
}

// TopoFilter restricts which agent processes the probe attaches to, by the
// topology it derives from each process. An empty filter attaches to every
// agent. ModelUUID is the more specific filter (a single model); ControllerUUID
// matches every model on a controller, including ones created after the probe
// starts (the dynamic PID watcher re-checks new processes against the filter).
type TopoFilter struct {
	ControllerUUID string
	ModelUUID      string
}

// Empty reports whether the filter matches everything.
func (f TopoFilter) Empty() bool { return f.ControllerUUID == "" && f.ModelUUID == "" }

// WriteFrame encodes f as a 4-byte big-endian length prefix followed by the
// JSON body. It is safe to call concurrently only if w is; the probe serialises
// writes through a single goroutine.
func WriteFrame(w io.Writer, f Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(body) > maxFrameBytes {
		return fmt.Errorf("probe: frame too large (%d bytes)", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one frame written by WriteFrame. It returns io.EOF cleanly
// when the stream ends on a frame boundary.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err // io.EOF on a clean boundary; io.ErrUnexpectedEOF mid-header
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return Frame{}, fmt.Errorf("probe: frame length %d exceeds cap", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return Frame{}, err
	}
	return f, nil
}
