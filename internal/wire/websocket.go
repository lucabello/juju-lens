package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"regexp"
)

// This file reassembles Juju's websocket RPC stream from the raw plaintext
// bytes captured at the TLS boundary. The recorder feeds each captured frame
// (a chunk of one direction of one connection) into a ConnDemux; the demux
// first consumes the HTTP upgrade handshake (to learn the model UUID from the
// request path) and then parses RFC 6455 frames, emitting one Envelope per
// complete websocket message.
//
// We implement just enough of RFC 6455 to read Juju's traffic: text/binary
// data frames, continuation frames, client-side masking, and skipping control
// frames (ping/pong/close). Juju never uses permessage-deflate on the API
// socket today; if that changes the inflate step slots in here.

// maxMessageBytes caps a single reassembled websocket message. Juju RPC
// messages are small (databags, status); anything larger is almost certainly a
// desync and is dropped rather than buffered unbounded.
const maxMessageBytes = 32 << 20

// modelPathRe extracts the model UUID from a Juju API websocket path such as
// "GET /model/<uuid>/api HTTP/1.1". Controller-wide connections use "/api" and
// yield no model.
var modelPathRe = regexp.MustCompile(`/model/([0-9a-fA-F-]{36})/`)

// frameReader reassembles one direction (masked or not) of one websocket
// connection. Bytes may arrive split across any number of Feed calls.
type frameReader struct {
	buf    []byte
	msg    []byte // accumulates payloads across continuation frames
	inFrag bool
	tooBig bool
}

// feed appends bytes and returns every complete websocket message payload that
// became available. Incomplete frames are retained for the next call.
func (r *frameReader) feed(b []byte) [][]byte {
	r.buf = append(r.buf, b...)
	var out [][]byte
	for {
		payload, isData, fin, n, ok := parseFrame(r.buf)
		if !ok {
			break
		}
		r.buf = r.buf[n:]
		if !isData {
			// Control frame (ping/pong/close): no application payload.
			continue
		}
		if len(r.msg)+len(payload) > maxMessageBytes {
			r.tooBig = true
		}
		if !r.tooBig {
			r.msg = append(r.msg, payload...)
		}
		if fin {
			if !r.tooBig && len(r.msg) > 0 {
				m := make([]byte, len(r.msg))
				copy(m, r.msg)
				out = append(out, m)
			}
			r.msg = r.msg[:0]
			r.inFrag = false
			r.tooBig = false
		} else {
			r.inFrag = true
		}
	}
	return out
}

// parseFrame decodes a single RFC 6455 frame from the front of buf. It returns
// the unmasked payload, whether the frame carries application data (opcode
// 0x0/0x1/0x2), the FIN bit, the number of bytes consumed, and ok=false when
// buf does not yet hold a complete frame.
func parseFrame(buf []byte) (payload []byte, isData, fin bool, consumed int, ok bool) {
	if len(buf) < 2 {
		return nil, false, false, 0, false
	}
	b0, b1 := buf[0], buf[1]
	fin = b0&0x80 != 0
	opcode := b0 & 0x0f
	masked := b1&0x80 != 0
	length := int(b1 & 0x7f)
	off := 2
	switch length {
	case 126:
		if len(buf) < off+2 {
			return nil, false, false, 0, false
		}
		length = int(binary.BigEndian.Uint16(buf[off : off+2]))
		off += 2
	case 127:
		if len(buf) < off+8 {
			return nil, false, false, 0, false
		}
		length = int(binary.BigEndian.Uint64(buf[off : off+8]))
		off += 8
	}
	var maskKey []byte
	if masked {
		if len(buf) < off+4 {
			return nil, false, false, 0, false
		}
		maskKey = buf[off : off+4]
		off += 4
	}
	if length < 0 || length > maxMessageBytes || len(buf) < off+length {
		if length < 0 || length > maxMessageBytes {
			// Corrupt length; consume the header so we don't spin forever.
			return nil, false, false, off, true
		}
		return nil, false, false, 0, false
	}
	raw := buf[off : off+length]
	consumed = off + length
	if masked {
		unmasked := make([]byte, length)
		for i := 0; i < length; i++ {
			unmasked[i] = raw[i] ^ maskKey[i&3]
		}
		payload = unmasked
	} else {
		payload = raw
	}
	// opcodes: 0x0 continuation, 0x1 text, 0x2 binary are data; 0x8-0xa control.
	isData = opcode == 0x0 || opcode == 0x1 || opcode == 0x2
	return payload, isData, fin, consumed, true
}

// ConnDemux tracks both directions of one websocket connection: it consumes
// the HTTP upgrade handshake (to attribute the connection to a Juju model) and
// then reassembles websocket messages, decoding each into an Envelope.
type ConnDemux struct {
	conn    uint64
	model   string
	upWrite bool   // handshake request (GET) seen
	upRead  bool   // handshake response (101) seen
	preW    []byte // buffered write-side handshake header
	preR    []byte // buffered read-side handshake header
	write   frameReader
	read    frameReader
}

// NewConnDemux creates a demux for a connection identified by connID.
func NewConnDemux(connID uint64) *ConnDemux { return &ConnDemux{conn: connID} }

// Model returns the model UUID the handshake advertised, or "" for a
// controller-wide connection or before the handshake was seen.
func (d *ConnDemux) Model() string { return d.model }

// Feed pushes one captured chunk (one direction) into the demux and returns
// every Envelope that became fully reassembled. Malformed JSON is skipped
// (returned envelopes are only those that decoded cleanly).
func (d *ConnDemux) Feed(dir Direction, b []byte) []Envelope {
	// The HTTP upgrade precedes any websocket frame on both directions. We
	// only need the request line, which is in the first write chunk, to learn
	// the model. Consume handshakes on both sides before parsing frames.
	if dir == DirWrite && !d.upWrite {
		rest, done := d.consumeHandshake(b, true)
		if !done {
			return nil
		}
		b = rest
	}
	if dir == DirRead && !d.upRead {
		rest, done := d.consumeHandshake(b, false)
		if !done {
			return nil
		}
		b = rest
	}
	var frames [][]byte
	if dir == DirWrite {
		frames = d.write.feed(b)
	} else {
		frames = d.read.feed(b)
	}
	if len(frames) == 0 {
		return nil
	}
	out := make([]Envelope, 0, len(frames))
	for _, f := range frames {
		var e Envelope
		if err := json.Unmarshal(f, &e); err != nil {
			continue // not an RPC envelope; skip
		}
		out = append(out, e)
	}
	return out
}

// consumeHandshake buffers bytes until the end of the HTTP header block
// (\r\n\r\n). On the write side it extracts the model UUID from the request
// path. It returns the bytes following the header block and whether the
// handshake is complete.
//
// When juju-lens attaches to an already-running agent, the API connection's
// upgrade handshake happened long before we started capturing, so it will
// never appear on the wire. We detect that on the first chunk: if the bytes
// don't begin like an HTTP upgrade (GET/POST on the write side, "HTTP/" on the
// read side), we assume a mid-stream attach and hand the bytes straight to the
// frame parser (a Write/Read call boundary is also a websocket frame
// boundary). Such connections carry no model tag, since the path we read it
// from is gone.
func (d *ConnDemux) consumeHandshake(b []byte, write bool) (rest []byte, done bool) {
	pre := &d.preR
	if write {
		pre = &d.preW
	}
	if len(*pre) == 0 && len(b) > 0 && !looksLikeHandshake(b[0], write) {
		if write {
			d.upWrite = true
		} else {
			d.upRead = true
		}
		return b, true // mid-stream attach: parse frames immediately, no model
	}
	*pre = append(*pre, b...)
	idx := bytes.Index(*pre, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, false
	}
	header := (*pre)[:idx]
	after := append([]byte(nil), (*pre)[idx+4:]...)
	*pre = nil
	if write {
		if m := modelPathRe.FindSubmatch(header); m != nil {
			d.model = string(m[1])
		}
		d.upWrite = true
	} else {
		d.upRead = true
	}
	return after, true
}

// looksLikeHandshake reports whether the first byte of a direction's stream
// begins an HTTP upgrade handshake rather than a websocket frame. Upgrade
// requests are "GET"/"POST" (0x47/0x50) and responses "HTTP/" (0x48); websocket
// data-frame first bytes are 0x00–0x0f or 0x80–0x8f, so there is no overlap.
func looksLikeHandshake(first byte, write bool) bool {
	if write {
		return first == 'G' || first == 'P'
	}
	return first == 'H'
}
